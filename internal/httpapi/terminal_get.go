package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// getKind is one `get <kind>` row of the command table.
type getKind struct {
	name    string
	aliases []string
	about   string
	cluster bool
	run     func(ctx context.Context, e *termEnv, a termArgs) (string, error)
}

// termRow is one line of a `get` table.
type termRow struct {
	ns, name   string
	cols, wide []string
}

// lister reads one kind in one namespace ("" for cluster-scoped kinds).
type lister func(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error)

// kind builds a getKind from its columns and lister.
func kind(name string, aliases []string, about string, cluster bool, head, wideHead []string, list lister) getKind {
	return getKind{name: name, aliases: aliases, about: about, cluster: cluster,
		run: func(ctx context.Context, e *termEnv, a termArgs) (string, error) {
			return runList(ctx, e, a, cluster, head, wideHead, list)
		}}
}

func runList(ctx context.Context, e *termEnv, a termArgs, cluster bool, head, wideHead []string, list lister) (string, error) {
	opts := metav1.ListOptions{LabelSelector: a.selector, FieldSelector: a.fieldSelector}
	nss := []string{""}
	if !cluster {
		var err error
		if nss, err = namespaces(ctx, e, a); err != nil {
			return "", err
		}
	}
	var rows []termRow
	for _, ns := range nss {
		got, err := list(ctx, e, ns, opts)
		if err != nil {
			return "", err
		}
		rows = append(rows, got...)
	}
	if len(a.pos) > 0 {
		var one []termRow
		for _, r := range rows {
			if r.name == a.pos[0] {
				one = append(one, r)
			}
		}
		if len(one) == 0 {
			return "", fmt.Errorf("%q not found", a.pos[0])
		}
		rows = one
	}
	if len(rows) == 0 {
		if cluster {
			return "No resources found.", nil
		}
		return fmt.Sprintf("No resources found in %s.", map[bool]string{true: "the watched namespaces", false: a.ns + " namespace"}[a.allNs]), nil
	}
	return table(head, wideHead, rows, a.allNs && !cluster, a.output == "wide"), nil
}

func table(head, wideHead []string, rows []termRow, showNs, wide bool) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	h := append([]string{"NAME"}, head...)
	if wide {
		h = append(h, wideHead...)
	}
	if showNs {
		h = append([]string{"NAMESPACE"}, h...)
	}
	fmt.Fprintln(w, strings.Join(h, "\t"))
	for _, r := range rows {
		c := append([]string{r.name}, r.cols...)
		if wide {
			c = append(c, r.wide...)
		}
		if showNs {
			c = append([]string{r.ns}, c...)
		}
		fmt.Fprintln(w, strings.Join(c, "\t"))
	}
	w.Flush()
	return buf.String()
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

func ratio(a, b int32) string { return fmt.Sprintf("%d/%d", a, b) }

func joinMap(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "<none>"
	}
	return strings.Join(out, ",")
}

// getKinds is every kind `get` reads (PLAN-003 2.1). Secrets are absent on
// purpose; ConfigMaps show their key count, never values.
func getKinds() []getKind {
	return []getKind{
		kind("pods", []string{"pod", "po"}, "pods", false, []string{"READY", "STATUS", "RESTARTS", "AGE"}, []string{"IP", "NODE"}, listPods),
		kind("deployments", []string{"deployment", "deploy"}, "Deployments", false, []string{"READY", "UP-TO-DATE", "AVAILABLE", "AGE"}, []string{"IMAGES", "SELECTOR"}, listDeployments),
		kind("statefulsets", []string{"statefulset", "sts"}, "StatefulSets", false, []string{"READY", "AGE"}, []string{"IMAGES"}, listStatefulSets),
		kind("daemonsets", []string{"daemonset", "ds"}, "DaemonSets", false, []string{"DESIRED", "CURRENT", "READY", "UP-TO-DATE", "AGE"}, []string{"NODE-SELECTOR"}, listDaemonSets),
		kind("replicasets", []string{"replicaset", "rs"}, "ReplicaSets", false, []string{"DESIRED", "CURRENT", "READY", "AGE"}, []string{"OWNER"}, listReplicaSets),
		kind("jobs", []string{"job"}, "Jobs", false, []string{"STATUS", "COMPLETIONS", "AGE"}, nil, listJobs),
		kind("cronjobs", []string{"cronjob", "cj"}, "CronJobs", false, []string{"SCHEDULE", "SUSPEND", "ACTIVE", "LAST-SCHEDULE", "AGE"}, nil, listCronJobs),
		kind("services", []string{"service", "svc"}, "Services", false, []string{"TYPE", "CLUSTER-IP", "PORTS", "AGE"}, []string{"SELECTOR"}, listServices),
		kind("endpointslices", []string{"endpointslice"}, "EndpointSlices: ready addresses per Service", false, []string{"SERVICE", "READY", "TOTAL", "AGE"}, nil, listEndpointSlices),
		kind("ingresses", []string{"ingress", "ing"}, "Ingresses", false, []string{"CLASS", "HOSTS", "AGE"}, nil, listIngresses),
		kind("networkpolicies", []string{"networkpolicy", "netpol"}, "NetworkPolicies", false, []string{"POD-SELECTOR", "TYPES", "AGE"}, nil, listNetworkPolicies),
		kind("hpa", []string{"horizontalpodautoscalers", "horizontalpodautoscaler"}, "HorizontalPodAutoscalers", false, []string{"TARGET", "MIN", "MAX", "REPLICAS", "AGE"}, nil, listHPAs),
		kind("pdb", []string{"poddisruptionbudgets", "poddisruptionbudget"}, "PodDisruptionBudgets", false, []string{"MIN-AVAILABLE", "MAX-UNAVAILABLE", "ALLOWED", "AGE"}, nil, listPDBs),
		kind("pvc", []string{"persistentvolumeclaims", "persistentvolumeclaim"}, "PersistentVolumeClaims", false, []string{"STATUS", "VOLUME", "CAPACITY", "CLASS", "AGE"}, nil, listPVCs),
		kind("resourcequotas", []string{"resourcequota", "quota"}, "ResourceQuotas, used of hard", false, []string{"USED", "AGE"}, nil, listQuotas),
		kind("configmaps", []string{"configmap", "cm"}, "ConfigMaps: key count only, never values", false, []string{"DATA", "AGE"}, nil, listConfigMaps),
		kind("events", []string{"event", "ev"}, "events, newest last", false, []string{"TYPE", "REASON", "OBJECT", "MESSAGE", "AGE"}, nil, listEvents),
		kind("autoremediationpolicies", []string{"autoremediationpolicy", "arp"}, "the agent's AutoRemediationPolicy objects", false, []string{"AGE"}, nil, listPolicies),
		kind("nodes", []string{"node", "no"}, "nodes", true, []string{"STATUS", "ROLES", "AGE", "VERSION"}, []string{"INTERNAL-IP", "OS", "RUNTIME"}, listNodes),
		kind("namespaces", []string{"namespace", "ns"}, "namespaces", true, []string{"STATUS", "AGE"}, nil, listNamespaces),
	}
}

func listPods(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().Pods(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		p := &l.Items[i]
		ready, restarts := int32(0), int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		rows = append(rows, termRow{ns: p.Namespace, name: p.Name,
			cols: []string{ratio(ready, int32(len(p.Spec.Containers))), podStatus(p), itoa(restarts), age(p.CreationTimestamp.Time)},
			wide: []string{p.Status.PodIP, p.Spec.NodeName}})
	}
	return rows, nil
}

func images(spec corev1.PodSpec) string {
	var out []string
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return strings.Join(out, ",")
}

func listDeployments(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.AppsV1().Deployments(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		d := &l.Items[i]
		sel := "<none>"
		if d.Spec.Selector != nil {
			sel = joinMap(d.Spec.Selector.MatchLabels)
		}
		rows = append(rows, termRow{ns: d.Namespace, name: d.Name,
			cols: []string{ratio(d.Status.ReadyReplicas, replicas(d.Spec.Replicas)), itoa(d.Status.UpdatedReplicas), itoa(d.Status.AvailableReplicas), age(d.CreationTimestamp.Time)},
			wide: []string{images(d.Spec.Template.Spec), sel}})
	}
	return rows, nil
}

func replicas(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

func listStatefulSets(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.AppsV1().StatefulSets(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		s := &l.Items[i]
		rows = append(rows, termRow{ns: s.Namespace, name: s.Name, cols: []string{ratio(s.Status.ReadyReplicas, replicas(s.Spec.Replicas)), age(s.CreationTimestamp.Time)},
			wide: []string{images(s.Spec.Template.Spec)}})
	}
	return rows, nil
}

func listDaemonSets(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.AppsV1().DaemonSets(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		d := &l.Items[i]
		st := d.Status
		rows = append(rows, termRow{ns: d.Namespace, name: d.Name,
			cols: []string{itoa(st.DesiredNumberScheduled), itoa(st.CurrentNumberScheduled), itoa(st.NumberReady), itoa(st.UpdatedNumberScheduled), age(d.CreationTimestamp.Time)},
			wide: []string{joinMap(d.Spec.Template.Spec.NodeSelector)}})
	}
	return rows, nil
}

func listReplicaSets(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.AppsV1().ReplicaSets(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		r := &l.Items[i]
		owner := "<none>"
		if len(r.OwnerReferences) > 0 {
			owner = strings.ToLower(r.OwnerReferences[0].Kind) + "/" + r.OwnerReferences[0].Name
		}
		rows = append(rows, termRow{ns: r.Namespace, name: r.Name,
			cols: []string{itoa(replicas(r.Spec.Replicas)), itoa(r.Status.Replicas), itoa(r.Status.ReadyReplicas), age(r.CreationTimestamp.Time)}, wide: []string{owner}})
	}
	return rows, nil
}

func listJobs(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.BatchV1().Jobs(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		j := &l.Items[i]
		status := "Running"
		for _, c := range j.Status.Conditions {
			switch {
			case c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue:
				status = "Complete"
			case c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue:
				status = "Failed"
			}
		}
		rows = append(rows, termRow{ns: j.Namespace, name: j.Name, cols: []string{status, ratio(j.Status.Succeeded, replicas(j.Spec.Completions)), age(j.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listCronJobs(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.BatchV1().CronJobs(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		c := &l.Items[i]
		last := "<none>"
		if c.Status.LastScheduleTime != nil {
			last = age(c.Status.LastScheduleTime.Time)
		}
		suspend := c.Spec.Suspend != nil && *c.Spec.Suspend
		rows = append(rows, termRow{ns: c.Namespace, name: c.Name,
			cols: []string{c.Spec.Schedule, strconv.FormatBool(suspend), strconv.Itoa(len(c.Status.Active)), last, age(c.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listServices(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().Services(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		s := &l.Items[i]
		var ports []string
		for _, p := range s.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
		rows = append(rows, termRow{ns: s.Namespace, name: s.Name,
			cols: []string{string(s.Spec.Type), s.Spec.ClusterIP, strings.Join(ports, ","), age(s.CreationTimestamp.Time)}, wide: []string{joinMap(s.Spec.Selector)}})
	}
	return rows, nil
}

func listEndpointSlices(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.DiscoveryV1().EndpointSlices(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		s := &l.Items[i]
		ready, total := 0, 0
		for _, ep := range s.Endpoints {
			total += len(ep.Addresses)
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				ready += len(ep.Addresses)
			}
		}
		rows = append(rows, termRow{ns: s.Namespace, name: s.Name,
			cols: []string{s.Labels["kubernetes.io/service-name"], strconv.Itoa(ready), strconv.Itoa(total), age(s.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listIngresses(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.NetworkingV1().Ingresses(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		g := &l.Items[i]
		class := "<none>"
		if g.Spec.IngressClassName != nil {
			class = *g.Spec.IngressClassName
		}
		var hosts []string
		for _, r := range g.Spec.Rules {
			hosts = append(hosts, r.Host)
		}
		rows = append(rows, termRow{ns: g.Namespace, name: g.Name, cols: []string{class, strings.Join(hosts, ","), age(g.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listNetworkPolicies(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.NetworkingV1().NetworkPolicies(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		p := &l.Items[i]
		var types []string
		for _, t := range p.Spec.PolicyTypes {
			types = append(types, string(t))
		}
		rows = append(rows, termRow{ns: p.Namespace, name: p.Name, cols: []string{joinMap(p.Spec.PodSelector.MatchLabels), strings.Join(types, ","), age(p.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listHPAs(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		h := &l.Items[i]
		rows = append(rows, termRow{ns: h.Namespace, name: h.Name, cols: []string{strings.ToLower(h.Spec.ScaleTargetRef.Kind) + "/" + h.Spec.ScaleTargetRef.Name,
			itoa(replicas(h.Spec.MinReplicas)), itoa(h.Spec.MaxReplicas), itoa(h.Status.CurrentReplicas), age(h.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listPDBs(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.PolicyV1().PodDisruptionBudgets(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		p := &l.Items[i]
		minA, maxU := "N/A", "N/A"
		if p.Spec.MinAvailable != nil {
			minA = p.Spec.MinAvailable.String()
		}
		if p.Spec.MaxUnavailable != nil {
			maxU = p.Spec.MaxUnavailable.String()
		}
		rows = append(rows, termRow{ns: p.Namespace, name: p.Name, cols: []string{minA, maxU, itoa(p.Status.DisruptionsAllowed), age(p.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listPVCs(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().PersistentVolumeClaims(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		c := &l.Items[i]
		class, capacity := "<none>", ""
		if c.Spec.StorageClassName != nil {
			class = *c.Spec.StorageClassName
		}
		if q, ok := c.Status.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		rows = append(rows, termRow{ns: c.Namespace, name: c.Name, cols: []string{string(c.Status.Phase), c.Spec.VolumeName, capacity, class, age(c.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listQuotas(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().ResourceQuotas(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		q := &l.Items[i]
		var used []string
		for r, hard := range q.Status.Hard {
			u := q.Status.Used[r]
			used = append(used, fmt.Sprintf("%s: %s/%s", r, u.String(), hard.String()))
		}
		sort.Strings(used)
		rows = append(rows, termRow{ns: q.Namespace, name: q.Name, cols: []string{strings.Join(used, ", "), age(q.CreationTimestamp.Time)}})
	}
	return rows, nil
}

// listConfigMaps shows how many keys each ConfigMap has, never a value.
func listConfigMaps(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().ConfigMaps(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		c := &l.Items[i]
		rows = append(rows, termRow{ns: c.Namespace, name: c.Name, cols: []string{strconv.Itoa(len(c.Data) + len(c.BinaryData)), age(c.CreationTimestamp.Time)}})
	}
	return rows, nil
}

func listEvents(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().Events(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	evs := l.Items
	sortEvents(evs)
	rows := make([]termRow, 0, len(evs))
	for i := range evs {
		ev := &evs[i]
		rows = append(rows, termRow{ns: ev.Namespace, name: ev.Name, cols: []string{ev.Type, ev.Reason,
			strings.ToLower(ev.InvolvedObject.Kind) + "/" + ev.InvolvedObject.Name, trunc(ev.Message, 100), age(eventAt(ev))}})
	}
	return rows, nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

var policyGVR = schema.GroupVersionResource{Group: "autoagent.io", Version: "v1alpha1", Resource: "autoremediationpolicies"}

func listPolicies(ctx context.Context, e *termEnv, ns string, o metav1.ListOptions) ([]termRow, error) {
	if e.dyn == nil {
		return nil, fmt.Errorf("AutoRemediationPolicy reads need the dynamic client, which this server was not given")
	}
	l, err := e.dyn.Resource(policyGVR).Namespace(ns).List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		rows = append(rows, termRow{ns: l.Items[i].GetNamespace(), name: l.Items[i].GetName(), cols: []string{age(l.Items[i].GetCreationTimestamp().Time)}})
	}
	return rows, nil
}

func listNodes(ctx context.Context, e *termEnv, _ string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().Nodes().List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		n := &l.Items[i]
		var roles []string
		for k := range n.Labels {
			if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok {
				roles = append(roles, r)
			}
		}
		sort.Strings(roles)
		if len(roles) == 0 {
			roles = []string{"<none>"}
		}
		ip := ""
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				ip = a.Address
			}
		}
		rows = append(rows, termRow{name: n.Name, cols: []string{nodeStatus(n), strings.Join(roles, ","), age(n.CreationTimestamp.Time), n.Status.NodeInfo.KubeletVersion},
			wide: []string{ip, n.Status.NodeInfo.OSImage, n.Status.NodeInfo.ContainerRuntimeVersion}})
	}
	return rows, nil
}

func listNamespaces(ctx context.Context, e *termEnv, _ string, o metav1.ListOptions) ([]termRow, error) {
	l, err := e.kc.CoreV1().Namespaces().List(ctx, o)
	if err != nil {
		return nil, err
	}
	rows := make([]termRow, 0, len(l.Items))
	for i := range l.Items {
		rows = append(rows, termRow{name: l.Items[i].Name, cols: []string{string(l.Items[i].Status.Phase), age(l.Items[i].CreationTimestamp.Time)}})
	}
	return rows, nil
}
