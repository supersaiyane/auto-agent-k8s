package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// target splits "deploy/api" into a normalised kind and a name.
func target(s string) (kind, name string, err error) {
	k, n, ok := strings.Cut(s, "/")
	if !ok || n == "" {
		return "", "", fmt.Errorf("give a target such as deploy/<name>")
	}
	switch strings.ToLower(k) {
	case "deploy", "deployment", "deployments":
		return "deployment", n, nil
	case "sts", "statefulset", "statefulsets":
		return "statefulset", n, nil
	case "po", "pod", "pods":
		return "pod", n, nil
	}
	return "", "", fmt.Errorf("unsupported target kind %q", k)
}

// runLogs prints a container's logs; deploy/<name> picks a pod of the
// Deployment, a running one when there is one (PLAN-003 2.4).
func runLogs(ctx context.Context, e *termEnv, a termArgs) (string, error) {
	if len(a.pos) == 0 {
		return "", fmt.Errorf("usage: logs <pod> | deploy/<name>")
	}
	pod := a.pos[0]
	if strings.Contains(pod, "/") {
		k, name, err := target(pod)
		if err != nil {
			return "", err
		}
		if k == "pod" {
			pod = name
		} else if pod, err = podOf(ctx, e, a.ns, name); err != nil {
			return "", err
		}
	}
	opts := &corev1.PodLogOptions{TailLines: &a.tail, Previous: a.previous, Container: a.container}
	if a.since > 0 {
		s := int64(a.since.Seconds())
		opts.SinceSeconds = &s
	}
	out, err := e.kc.CoreV1().Pods(a.ns).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// podOf picks a pod of the Deployment, preferring a running one.
func podOf(ctx context.Context, e *termEnv, ns, deploy string) (string, error) {
	d, err := e.kc.AppsV1().Deployments(ns).Get(ctx, deploy, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return "", err
	}
	pods, err := e.kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("deployment %s has no pod", deploy)
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	return pods.Items[0].Name, nil
}

// eventAt is when an event last happened.
func eventAt(ev *corev1.Event) time.Time {
	switch {
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

// sortEvents orders events oldest first, so the newest are last.
func sortEvents(evs []corev1.Event) {
	sort.SliceStable(evs, func(i, j int) bool { return eventAt(&evs[i]).Before(eventAt(&evs[j])) })
}

// runEvents prints events, newest last, optionally for one object (2.5).
func runEvents(ctx context.Context, e *termEnv, a termArgs) (string, error) {
	var kind, name string
	if a.forObj != "" {
		k, n, ok := strings.Cut(a.forObj, "/")
		if !ok {
			return "", fmt.Errorf("--for needs <kind>/<name>")
		}
		kind, name = strings.ToLower(k), n
	}
	nss, err := namespaces(ctx, e, a)
	if err != nil {
		return "", err
	}
	var evs []corev1.Event
	for _, ns := range nss {
		l, err := e.kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		for _, ev := range l.Items {
			if name == "" || (ev.InvolvedObject.Name == name && kindMatches(kind, ev.InvolvedObject.Kind)) {
				evs = append(evs, ev)
			}
		}
	}
	if len(evs) == 0 {
		return "No events found.", nil
	}
	sortEvents(evs)
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LAST SEEN\tNAMESPACE\tTYPE\tREASON\tOBJECT\tMESSAGE")
	for i := range evs {
		ev := &evs[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s/%s\t%s\n", age(eventAt(ev)), ev.Namespace, ev.Type, ev.Reason,
			strings.ToLower(ev.InvolvedObject.Kind), ev.InvolvedObject.Name, trunc(ev.Message, 120))
	}
	w.Flush()
	return buf.String(), nil
}

func kindMatches(want, got string) bool {
	got = strings.ToLower(got)
	switch want {
	case "po", "pods":
		want = "pod"
	case "deploy", "deployments":
		want = "deployment"
	case "sts", "statefulsets":
		want = "statefulset"
	}
	return want == got
}

// runRolloutStatus reports a rollout's progress, as kubectl does (2.6).
func runRolloutStatus(ctx context.Context, e *termEnv, a termArgs) (string, error) {
	k, name, err := target(first(a.pos))
	if err != nil {
		return "", err
	}
	switch k {
	case "deployment":
		d, err := e.kc.AppsV1().Deployments(a.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return deploymentRollout(d), nil
	case "statefulset":
		s, err := e.kc.AppsV1().StatefulSets(a.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return statefulSetRollout(s), nil
	}
	return "", fmt.Errorf("rollout status works on deploy/<name> and sts/<name>")
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func deploymentRollout(d *appsv1.Deployment) string {
	want := replicas(d.Spec.Replicas)
	st := d.Status
	for _, c := range st.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return fmt.Sprintf("deployment %q exceeded its progress deadline: %s", d.Name, c.Message)
		}
	}
	switch {
	case st.ObservedGeneration < d.Generation:
		return fmt.Sprintf("Waiting for deployment %q spec update to be observed...", d.Name)
	case st.UpdatedReplicas < want:
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d new replicas have been updated...", d.Name, st.UpdatedReplicas, want)
	case st.Replicas > st.UpdatedReplicas:
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d old replicas are pending termination...", d.Name, st.Replicas-st.UpdatedReplicas)
	case st.AvailableReplicas < st.UpdatedReplicas:
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available...", d.Name, st.AvailableReplicas, st.UpdatedReplicas)
	}
	return fmt.Sprintf("deployment %q successfully rolled out", d.Name)
}

func statefulSetRollout(s *appsv1.StatefulSet) string {
	want := replicas(s.Spec.Replicas)
	st := s.Status
	switch {
	case st.ObservedGeneration < s.Generation:
		return fmt.Sprintf("Waiting for statefulset %q spec update to be observed...", s.Name)
	case st.ReadyReplicas < want:
		return fmt.Sprintf("Waiting for %d pods to be ready...", want-st.ReadyReplicas)
	case st.UpdateRevision != st.CurrentRevision:
		return fmt.Sprintf("Waiting for partitioned roll out to finish: %d out of %d new pods have been updated...", st.UpdatedReplicas, want)
	}
	return fmt.Sprintf("statefulset rolling update complete %d pods at revision %s...", want, st.CurrentRevision)
}

// runRolloutHistory lists revisions and their images (2.6).
func runRolloutHistory(ctx context.Context, e *termEnv, a termArgs) (string, error) {
	k, name, err := target(first(a.pos))
	if err != nil {
		return "", err
	}
	type rev struct {
		n     int64
		image string
	}
	var revs []rev
	switch k {
	case "deployment":
		rss, err := e.kc.AppsV1().ReplicaSets(a.ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		for _, rs := range rss.Items {
			for _, o := range rs.OwnerReferences {
				if o.Kind == "Deployment" && o.Name == name {
					n, _ := strconv.ParseInt(rs.Annotations["deployment.kubernetes.io/revision"], 10, 64)
					revs = append(revs, rev{n, images(rs.Spec.Template.Spec)})
				}
			}
		}
	case "statefulset":
		crs, err := e.kc.AppsV1().ControllerRevisions(a.ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		for _, cr := range crs.Items {
			for _, o := range cr.OwnerReferences {
				if o.Kind == "StatefulSet" && o.Name == name {
					revs = append(revs, rev{cr.Revision, cr.Name})
				}
			}
		}
	default:
		return "", fmt.Errorf("rollout history works on deploy/<name> and sts/<name>")
	}
	if len(revs) == 0 {
		return fmt.Sprintf("no revisions found for %s/%s", k, name), nil
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].n < revs[j].n })
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REVISION\tIMAGES OR REVISION")
	for _, r := range revs {
		fmt.Fprintf(w, "%d\t%s\n", r.n, r.image)
	}
	w.Flush()
	return buf.String(), nil
}

// runCanI asks the API server what the agent itself may do, with a
// SelfSubjectAccessReview: a question, not a change (2.7).
func runCanI(ctx context.Context, e *termEnv, a termArgs) (string, error) {
	if len(a.pos) < 2 {
		return "", fmt.Errorf("usage: auth can-i <verb> <resource> [-n <ns>]")
	}
	res, group, _ := strings.Cut(a.pos[1], ".")
	review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{
		Namespace: a.ns, Verb: a.pos[0], Resource: res, Group: group}}}
	got, err := e.kc.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	if got.Status.Allowed {
		return "yes", nil
	}
	if got.Status.Reason != "" {
		return "no: " + got.Status.Reason, nil
	}
	return "no", nil
}

func runVersion(ctx context.Context, e *termEnv, _ termArgs) (string, error) {
	sv, err := e.kc.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Server Version: %s\nPlatform: %s", sv.GitVersion, sv.Platform), nil
}
