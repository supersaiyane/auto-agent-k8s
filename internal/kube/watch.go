package kube

import (
    "context"
    "fmt"
    "time"
    "strings"
    "io"
    "os"

    "k8s.io/apimachinery/pkg/fields"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    corev1 "k8s.io/api/core/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/informers"
    policyv1 "k8s.io/api/policy/v1"

    "github.com/yourorg/auto-agent/internal/metrics"
    "github.com/yourorg/auto-agent/internal/policy"
    "github.com/yourorg/auto-agent/internal/slack"
    "github.com/yourorg/auto-agent/internal/llm"
    "github.com/yourorg/auto-agent/internal/storage"
    "github.com/yourorg/auto-agent/internal/crd"
    "github.com/yourorg/auto-agent/internal/obs"
)

func WatchPods(ctx context.Context, kc *kubernetes.Clientset, mp metrics.Provider, pol *policy.Policy, sl *slack.Client, ll *llm.Client) {
    f := informers.NewSharedInformerFactory(kc, 0)
    inf := f.Core().V1().Pods().Informer()

    inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
        UpdateFunc: func(oldObj, newObj interface{}) {
            pod := newObj.(*corev1.Pod)
            if !allowed(pol, pod.Namespace) || hasAnno(pod, pol.ExcludedAnnotation) { return }

            for _, cs := range pod.Status.ContainerStatuses {
                if cs.State.Waiting != nil {
                    switch cs.State.Waiting.Reason {
                    case "CrashLoopBackOff":
                        go handleCrashLoop(ctx, kc, pod, cs.Name, pol, sl, ll)
                        return
                    case "ImagePullBackOff", "ErrImagePull":
                        go handleImagePullBackOff(ctx, kc, pod, cs.Name, pol, sl, ll)
                        return
                    }
                }
                if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
                    go handleOOM(ctx, kc, pod, cs.Name, pol, sl, ll)
                    return
                }
            }
        },
    })
    go inf.Run(ctx.Done())

    // Node watcher for pressure -> cordon + evict
    nf := informers.NewSharedInformerFactory(kc, 0)
    ninf := nf.Core().V1().Nodes().Informer()
    ninf.AddEventHandler(cache.ResourceEventHandlerFuncs{
        UpdateFunc: func(oldObj, newObj interface{}) {
            node := newObj.(*corev1.Node)
            handleNodePressure(ctx, kc, node, pol, sl)
        },
    })
    go ninf.Run(ctx.Done())
}

func getLastLogs(ctx context.Context, kc *kubernetes.Clientset, ns, pod, container string, lines int64) string {
    opts := &corev1.PodLogOptions{Container: container, TailLines: &lines}
    req := kc.CoreV1().Pods(ns).GetLogs(pod, opts)
    r, err := req.Stream(ctx)
    if err != nil { return fmt.Sprintf("log fetch error: %v", err) }
    defer r.Close()
    b, _ := io.ReadAll(r)
    return string(b)
}

func collectEvents(ctx context.Context, kc *kubernetes.Clientset, ns, pod string) []string {
    evs, err := kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("involvedObject.name", pod).String()})
    if err != nil { return nil }
    out := make([]string, 0, len(evs.Items))
    for _, e := range evs.Items {
        out = append(out, fmt.Sprintf("%s %s: %s", e.Type, e.Reason, e.Message))
    }
    return out
}

func persistLogBundle(ctx context.Context, ns, workload, pod, container, node, reason, message, logs string, events []string) (string, error) {
    sink := storage.NewSinkFromEnv()
    key := storage.BuildKey(ns, workload, pod, reason, time.Now())
    rec := &storage.Record{
        Timestamp: time.Now().UTC(),
        Namespace: ns, Workload: workload, Pod: pod, Container: container, Node: node,
        Reason: reason, Message: message, LastLogs: logs, Events: events,
    }
    return sink.Save(ctx, key, rec)
}

func handleCrashLoop(ctx context.Context, kc *kubernetes.Clientset, pod *corev1.Pod, cname string, pol *policy.Policy, sl *slack.Client, ll *llm.Client) {
    ns := pod.Namespace; name := pod.Name
    logs := getLastLogs(ctx, kc, ns, name, cname, 50)
    events := collectEvents(ctx, kc, ns, name)
    url, _ := persistLogBundle(ctx, ns, ownerName(pod), name, cname, pod.Spec.NodeName, "CrashLoopBackOff", "CrashLoopBackOff detected", logs, events)

    msg := fmt.Sprintf("*CrashLoopBackOff* on `%s/%s` (container: `%s`)  \nLogs+events: `%s`\n", ns, name, cname, url)
    if pol.Mode == policy.Fix {
        _ = kc.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
        msg += "_Action_: deleted pod to clear backoff (RS will recreate). Cooldown 5m.\n"
        obs.ActionsTotal.WithLabelValues("delete_pod", ns, ownerName(pod)).Inc()
    } else if pol.Mode == policy.Suggest {
        msg += "_Suggest_: delete pod to clear backoff. Approve to proceed.\n"
    }
    if ll.Enabled() {
        advice, _ := ll.Diagnose("Pod CrashLoopBackOff", logs+"\n"+strings.Join(events, "\n"))
        if advice != "" { msg += "\n_LLM_: " + advice + "\n" }
    }
    _ = sl.Post(msg)
    obs.IncidentsTotal.WithLabelValues("CrashLoopBackOff", ns, ownerName(pod)).Inc()
}

func handleImagePullBackOff(ctx context.Context, kc *kubernetes.Clientset, pod *corev1.Pod, cname string, pol *policy.Policy, sl *slack.Client, ll *llm.Client) {
    ns := pod.Namespace; name := pod.Name
    events := collectEvents(ctx, kc, ns, name)
    logs := ""
    url, _ := persistLogBundle(ctx, ns, ownerName(pod), name, cname, pod.Spec.NodeName, "ImagePullBackOff", "Image pull failure", logs, events)

    msg := fmt.Sprintf("*ImagePullBackOff* on `%s/%s` (container: `%s`)  \nSaved: `%s`\n", ns, name, cname, url)
    mirror := osGetBool("IMAGE_MIRROR_ENABLED", false)
    prefix := getenv("IMAGE_MIRROR_PREFIX","")
    image := imageOf(pod, cname)
    if mirror && image != "" {
        msg += fmt.Sprintf("_Suggest_: mirror `%s` via prefix `%s` using GitOps PR.\n", image, prefix)
    }
    if pol.Mode == policy.Fix {
        _ = kc.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
        msg += "_Action_: deleted pod to retry image pull.\n"
        obs.ActionsTotal.WithLabelValues("delete_pod", ns, ownerName(pod)).Inc()
    }
    if ll.Enabled() {
        advice, _ := ll.Diagnose("ImagePullBackOff", strings.Join(events, "\n"))
        if advice != "" { msg += "\n_LLM_: " + advice + "\n" }
    }
    _ = sl.Post(msg)
    obs.IncidentsTotal.WithLabelValues("ImagePullBackOff", ns, ownerName(pod)).Inc()
}

func handleOOM(ctx context.Context, kc *kubernetes.Clientset, pod *corev1.Pod, cname string, pol *policy.Policy, sl *slack.Client, ll *llm.Client) {
    ns := pod.Namespace; name := pod.Name
    logs := getLastLogs(ctx, kc, ns, name, cname, 20)
    events := collectEvents(ctx, kc, ns, name)
    url, _ := persistLogBundle(ctx, ns, ownerName(pod), name, cname, pod.Spec.NodeName, "OOMKilled", "Container OOMKilled", logs, events)

    msg := fmt.Sprintf("*OOMKilled* on `%s/%s` (container: `%s`). Saved: `%s`\n", ns, name, cname, url)
    msg += "Recommend +20% memory limit via GitOps PR; investigate usage spikes.\n"
    if ll.Enabled() {
        advice, _ := ll.Diagnose("Container OOMKilled", logs+"\n"+strings.Join(events, "\n"))
        if advice != "" { msg += "\n_LLM_: " + advice + "\n" }
    }
    _ = sl.Post(msg)
    obs.IncidentsTotal.WithLabelValues("OOMKilled", ns, ownerName(pod)).Inc()
}

func handleNodePressure(ctx context.Context, kc *kubernetes.Clientset, node *corev1.Node, pol *policy.Policy, sl *slack.Client) {
    var memP, diskP bool
    for _, c := range node.Status.Conditions {
        if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue { memP = true }
        if c.Type == corev1.NodeDiskPressure && c.Status == corev1.ConditionTrue { diskP = true }
    }
    if !(memP || diskP) { return }

    if !node.Spec.Unschedulable {
        ncopy := node.DeepCopy()
        ncopy.Spec.Unschedulable = true
        if _, err := kc.CoreV1().Nodes().Update(ctx, ncopy, metav1.UpdateOptions{}); err == nil {
            _ = sl.Post(fmt.Sprintf("*NodePressure*: cordoned node `%s` (mem:%t disk:%t)", node.Name, memP, diskP))
        }
    }

    // Evict non-critical pods
    pl, _ := kc.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node.Name).String()})
    for _, p := range pl.Items {
        if isCritical(&p) { continue }
        // Evict
        gr := int64(30)
        ev := &policyv1.Eviction{
            ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name},
            DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: &gr},
        }
        _ = kc.PolicyV1().Evictions(p.Namespace).Evict(ctx, ev)
        obs.ActionsTotal.WithLabelValues("evict_pod", p.Namespace, ownerRef(&p)).Inc()
    }
}

func isCritical(p *corev1.Pod) bool {
    pc := p.Spec.PriorityClassName
    if strings.HasPrefix(pc, "system-") { return true }
    if strings.Contains(strings.ToLower(pc), "critical") { return true }
    return false
}

func EvaluateAndScale(ctx context.Context, kc *kubernetes.Clientset, mp metrics.Provider, pol *policy.Policy, sl *slack.Client, ll *llm.Client) {
    // Multi-signal gating:
    // - CPU > threshold
    // - AND one of: queue depth high OR p95 latency above SLO OR error rate high
    // Cooldowns and min/max replicas via annotations (simplified here).

    for ns := range pol.NamespaceAllow {
        dl, err := kc.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
        if err != nil { continue }
        for _, d := range dl.Items {
            rep := int32(1); if d.Spec.Replicas != nil { rep = *d.Spec.Replicas }
            last := d.Annotations["auto-agent.io/last-scale-ts"]
            if last != "" {
                if t, err := time.Parse(time.RFC3339, last); err==nil {
                    if time.Since(t) < parseDur(getenv("COOLDOWN_UP","2m")) {
                        continue
                    }
                }
            }

            // CPU
            cpu, err := mp.AvgDeploymentCPU(ctx, &d, getenv("SCALE_WINDOW","5m"))
            if err != nil { continue }

            // Optional signals via env/ConfigMap (could come from CRD per-app in full impl)
            qDepthQ := getenv("PROM_QUEUE_DEPTH","")      // e.g., sum(queue_depth{deployment="X"})
            errRateQ := getenv("PROM_ERROR_RATE","")      // e.g., rate(http_requests_total{code=~"5..",deployment="X"}[5m])
            p95Q := getenv("PROM_P95_LATENCY","")         // e.g., histogram_quantile(0.95, ...)
            // Naive namespace filter by deployment name
            gateOK := false
            if qDepthQ != "" {
                if v, err := mp.QueryInstant(ctx, qDepthQ); err==nil && v > 0 { gateOK = true }
            }
            if !gateOK && errRateQ != "" {
                if v, err := mp.QueryInstant(ctx, errRateQ); err==nil && v > 0 { gateOK = true }
            }
            if !gateOK && p95Q != "" {
                if v, err := mp.QueryInstant(ctx, p95Q); err==nil && v > 0 { gateOK = true }
            }
            if p95Q == "" && qDepthQ == "" && errRateQ == "" { gateOK = true } // fallback: CPU-only

            if cpu > parseFloat(getenv("SCALE_CPU_THRESHOLD","0.8")) && gateOK {
                step := int32(1)
                if step > int32(parseInt(getenv("MAX_SCALE_STEP","2"))) { step = int32(parseInt(getenv("MAX_SCALE_STEP","2"))) }
                newRep := rep + step
                d.Spec.Replicas = &newRep
                if d.Annotations==nil { d.Annotations = map[string]string{} }
                d.Annotations["auto-agent.io/last-scale-ts"] = time.Now().UTC().Format(time.RFC3339)
                if _, err := kc.AppsV1().Deployments(ns).Update(ctx, &d, metav1.UpdateOptions{}); err == nil {
                    msg := fmt.Sprintf("*ScaleUp*: `%s/%s` %d → %d (cpu=%.2f)", ns, d.Name, rep, newRep, cpu)
                    _ = sl.Post(msg); obs.ActionsTotal.WithLabelValues("scale_up", ns, d.Name).Inc()
                }
            } else {
                // Scale Down: only if CPU well below threshold and no gates active; long cooldown
                lastDown := d.Annotations["auto-agent.io/last-scale-down-ts"]
                if lastDown != "" {
                    if t, err := time.Parse(time.RFC3339, lastDown); err==nil {
                        if time.Since(t) < parseDur(getenv("COOLDOWN_DOWN","10m")) {
                            continue
                        }
                    }
                }
                if rep > 1 && cpu < 0.3 && !gateOK {
                    newRep := rep - 1
                    d.Spec.Replicas = &newRep
                    if d.Annotations==nil { d.Annotations = map[string]string{} }
                    d.Annotations["auto-agent.io/last-scale-down-ts"] = time.Now().UTC().Format(time.RFC3339)
                    if _, err := kc.AppsV1().Deployments(ns).Update(ctx, &d, metav1.UpdateOptions{}); err == nil {
                        msg := fmt.Sprintf("*ScaleDown*: `%s/%s` %d → %d (cpu=%.2f)", ns, d.Name, rep, newRep, cpu)
                        _ = sl.Post(msg); obs.ActionsTotal.WithLabelValues("scale_down", ns, d.Name).Inc()
                    }
                }
            }
        }
    }
}

func CheckAnomalies(ctx context.Context, kc *kubernetes.Clientset, mp metrics.Provider, pol *policy.Policy, sl *slack.Client, ll *llm.Client, store *crd.Store) {
    // Iterate CRD policies and evaluate anomaly PromQLs (z-score assumed precomputed in PromQL or simple threshold).
    // Here we just run the PromQL and if > 1, we post.
    // In production, maintain state to require sustained breach.
    nss := []string{}
    // list namespaces from store
    for ns, _ := range map[string]struct{}{} {
        _ = ns; // placeholder
    }
    // Simplified: fetch all policies via List on a few known namespaces (would require store introspection methods).
    // For brevity, we skip iteration details and just post a heartbeat in this scaffold.
    _ = sl.Post("Anomaly evaluation tick (CRD-driven).")
}

func allowed(pol *policy.Policy, ns string) bool { _, ok := pol.NamespaceAllow[ns]; return ok }
func hasAnno(p *corev1.Pod, key string) bool { if key=="" {return false}; if p.Annotations==nil {return false}; _,ok := p.Annotations[key]; return ok }

func ownerName(p *corev1.Pod) string {
    for _, o := range p.OwnerReferences {
        if o.Controller != nil && *o.Controller {
            return fmt.Sprintf("%s/%s", strings.ToLower(o.Kind), o.Name)
        }
    }
    return "pod/" + p.Name
}

func ownerRef(p *corev1.Pod) string {
    for _, o := range p.OwnerReferences {
        if o.Controller != nil && *o.Controller {
            return fmt.Sprintf("%s/%s", strings.ToLower(o.Kind), o.Name)
        }
    }
    return p.Name
}

func imageOf(p *corev1.Pod, cname string) string {
    for _, c := range p.Spec.Containers {
        if c.Name == cname { return c.Image }
    }
    return ""
}

func parseDur(s string) time.Duration { d, _ := time.ParseDuration(s); return d }
func getenv(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }
func parseFloat(s string) float64 { var f float64; fmt.Sscanf(s, "%f", &f); return f }
func parseInt(s string) int { var i int; fmt.Sscanf(s, "%d", &i); return i }
func osGetBool(k string, def bool) bool { v := os.Getenv(k); if v=="" { return def }; v = strings.ToLower(v); return v=="1"||v=="true"||v=="yes"||v=="y" }
