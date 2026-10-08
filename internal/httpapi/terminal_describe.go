package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// describeKinds is every kind `describe` reads (PLAN-003 2.2).
func describeKinds() []getKind {
	d := func(name string, aliases []string, about string, cluster bool, f func(ctx context.Context, e *termEnv, ns, name string) (string, error)) getKind {
		return getKind{name: name, aliases: aliases, about: about, cluster: cluster, run: func(ctx context.Context, e *termEnv, a termArgs) (string, error) {
			if len(a.pos) == 0 {
				return "", fmt.Errorf("usage: describe %s <name>", name)
			}
			return f(ctx, e, a.ns, a.pos[0])
		}}
	}
	return []getKind{
		d("pod", []string{"pods", "po"}, "a pod, its containers and events", false, describePod),
		d("deployment", []string{"deployments", "deploy"}, "a Deployment, its conditions and containers", false, describeDeployment),
		d("statefulset", []string{"statefulsets", "sts"}, "a StatefulSet and its revisions", false, describeStatefulSet),
		d("job", []string{"jobs"}, "a Job and its conditions", false, describeJob),
		d("service", []string{"services", "svc"}, "a Service and its ready endpoints", false, describeService),
		d("pvc", []string{"persistentvolumeclaim", "persistentvolumeclaims"}, "a claim and its events", false, describePVC),
		d("hpa", []string{"horizontalpodautoscaler", "horizontalpodautoscalers"}, "an autoscaler and its conditions", false, describeHPA),
		d("ingress", []string{"ingresses", "ing"}, "an Ingress and its rules", false, describeIngress),
		d("node", []string{"nodes", "no"}, "a node, its capacity and conditions", true, func(ctx context.Context, e *termEnv, _, name string) (string, error) {
			return describeNode(ctx, e, name)
		}),
	}
}

// field writes "Label:  value" lines.
type field struct {
	buf bytes.Buffer
}

func (f *field) add(label, format string, args ...any) {
	fmt.Fprintf(&f.buf, "%-18s %s\n", label+":", fmt.Sprintf(format, args...))
}

func (f *field) section(title string) { fmt.Fprintf(&f.buf, "\n%s:\n", title) }

func (f *field) line(format string, args ...any) { fmt.Fprintf(&f.buf, "  "+format+"\n", args...) }

// objectEvents appends the events about one object, newest last.
func objectEvents(ctx context.Context, e *termEnv, f *field, ns, kind, name string) {
	l, err := e.kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + name})
	if err != nil {
		f.section("Events")
		f.line("(could not read events: %v)", err)
		return
	}
	var evs []corev1.Event
	for _, ev := range l.Items {
		if ev.InvolvedObject.Name == name && (kind == "" || ev.InvolvedObject.Kind == kind) {
			evs = append(evs, ev)
		}
	}
	if len(evs) == 0 {
		return
	}
	sortEvents(evs)
	f.section("Events")
	for i := range evs {
		f.line("%s  %s  %s: %s", evs[i].Type, age(eventAt(&evs[i])), evs[i].Reason, evs[i].Message)
	}
}

func describePod(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	p, err := e.kc.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", p.Name)
	f.add("Namespace", "%s", p.Namespace)
	f.add("Node", "%s", p.Spec.NodeName)
	f.add("Status", "%s", podStatus(p))
	f.add("IP", "%s", p.Status.PodIP)
	f.add("Age", "%s", age(p.CreationTimestamp.Time))
	f.add("Labels", "%s", joinMap(p.Labels))
	f.section("Containers")
	for _, cs := range p.Status.ContainerStatuses {
		state := "unknown"
		switch {
		case cs.State.Running != nil:
			state = "Running since " + age(cs.State.Running.StartedAt.Time)
		case cs.State.Waiting != nil:
			state = "Waiting: " + cs.State.Waiting.Reason
		case cs.State.Terminated != nil:
			state = fmt.Sprintf("Terminated: %s, exit %d", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
		f.line("%s  image %s  ready %v  restarts %d  %s", cs.Name, cs.Image, cs.Ready, cs.RestartCount, state)
	}
	objectEvents(ctx, e, f, ns, "Pod", name)
	return f.buf.String(), nil
}

func describeDeployment(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	d, err := e.kc.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", d.Name)
	f.add("Namespace", "%s", d.Namespace)
	f.add("Replicas", "%d desired, %d updated, %d available, %d ready", replicas(d.Spec.Replicas), d.Status.UpdatedReplicas, d.Status.AvailableReplicas, d.Status.ReadyReplicas)
	f.add("Strategy", "%s", d.Spec.Strategy.Type)
	f.add("Paused", "%v", d.Spec.Paused)
	f.add("Age", "%s", age(d.CreationTimestamp.Time))
	f.section("Conditions")
	for _, c := range d.Status.Conditions {
		f.line("%s=%s (%s) %s", c.Type, c.Status, c.Reason, c.Message)
	}
	f.section("Containers")
	for _, c := range d.Spec.Template.Spec.Containers {
		f.line("%s  image %s  requests %s  limits %s", c.Name, c.Image, resourceList(c.Resources.Requests), resourceList(c.Resources.Limits))
	}
	objectEvents(ctx, e, f, ns, "Deployment", name)
	return f.buf.String(), nil
}

func resourceList(r corev1.ResourceList) string {
	var out []string
	for k, v := range r {
		out = append(out, string(k)+"="+v.String())
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "<none>"
	}
	return strings.Join(out, ",")
}

func describeStatefulSet(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	s, err := e.kc.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", s.Name)
	f.add("Namespace", "%s", s.Namespace)
	f.add("Replicas", "%d desired, %d ready, %d updated", replicas(s.Spec.Replicas), s.Status.ReadyReplicas, s.Status.UpdatedReplicas)
	f.add("Pod management", "%s", s.Spec.PodManagementPolicy)
	f.add("Revisions", "current %s, update %s", s.Status.CurrentRevision, s.Status.UpdateRevision)
	f.add("Images", "%s", images(s.Spec.Template.Spec))
	objectEvents(ctx, e, f, ns, "StatefulSet", name)
	return f.buf.String(), nil
}

func describeJob(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	j, err := e.kc.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", j.Name)
	f.add("Namespace", "%s", j.Namespace)
	f.add("Pods", "%d active, %d succeeded, %d failed", j.Status.Active, j.Status.Succeeded, j.Status.Failed)
	f.add("Images", "%s", images(j.Spec.Template.Spec))
	f.section("Conditions")
	for _, c := range j.Status.Conditions {
		f.line("%s=%s (%s) %s", c.Type, c.Status, c.Reason, c.Message)
	}
	objectEvents(ctx, e, f, ns, "Job", name)
	return f.buf.String(), nil
}

func describeService(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	s, err := e.kc.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", s.Name)
	f.add("Namespace", "%s", s.Namespace)
	f.add("Type", "%s", s.Spec.Type)
	f.add("Cluster IP", "%s", s.Spec.ClusterIP)
	f.add("Selector", "%s", joinMap(s.Spec.Selector))
	slices, err := e.kc.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + name})
	if err == nil {
		var ready []string
		for _, sl := range slices.Items {
			for _, ep := range sl.Endpoints {
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					ready = append(ready, ep.Addresses...)
				}
			}
		}
		f.add("Ready endpoints", "%d %s", len(ready), strings.Join(ready, ","))
	}
	objectEvents(ctx, e, f, ns, "Service", name)
	return f.buf.String(), nil
}

func describePVC(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	c, err := e.kc.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", c.Name)
	f.add("Namespace", "%s", c.Namespace)
	f.add("Status", "%s", c.Status.Phase)
	f.add("Volume", "%s", c.Spec.VolumeName)
	if c.Spec.StorageClassName != nil {
		f.add("StorageClass", "%s", *c.Spec.StorageClassName)
	}
	f.add("Requested", "%s", resourceList(c.Spec.Resources.Requests))
	objectEvents(ctx, e, f, ns, "PersistentVolumeClaim", name)
	return f.buf.String(), nil
}

func describeHPA(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	h, err := e.kc.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", h.Name)
	f.add("Namespace", "%s", h.Namespace)
	f.add("Target", "%s/%s", h.Spec.ScaleTargetRef.Kind, h.Spec.ScaleTargetRef.Name)
	f.add("Replicas", "min %d, max %d, current %d, desired %d", replicas(h.Spec.MinReplicas), h.Spec.MaxReplicas, h.Status.CurrentReplicas, h.Status.DesiredReplicas)
	f.section("Conditions")
	for _, c := range h.Status.Conditions {
		f.line("%s=%s (%s) %s", c.Type, c.Status, c.Reason, c.Message)
	}
	objectEvents(ctx, e, f, ns, "HorizontalPodAutoscaler", name)
	return f.buf.String(), nil
}

func describeIngress(ctx context.Context, e *termEnv, ns, name string) (string, error) {
	g, err := e.kc.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", g.Name)
	f.add("Namespace", "%s", g.Namespace)
	if g.Spec.DefaultBackend != nil && g.Spec.DefaultBackend.Service != nil {
		f.add("Default backend", "%s", g.Spec.DefaultBackend.Service.Name)
	}
	f.section("Rules")
	for _, r := range g.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			backend := "<resource>"
			if p.Backend.Service != nil {
				backend = p.Backend.Service.Name
			}
			f.line("%s%s -> %s", r.Host, p.Path, backend)
		}
	}
	objectEvents(ctx, e, f, ns, "Ingress", name)
	return f.buf.String(), nil
}

func describeNode(ctx context.Context, e *termEnv, name string) (string, error) {
	n, err := e.kc.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	f := &field{}
	f.add("Name", "%s", n.Name)
	f.add("Status", "%s", nodeStatus(n))
	f.add("Version", "%s", n.Status.NodeInfo.KubeletVersion)
	f.add("OS", "%s (%s)", n.Status.NodeInfo.OSImage, n.Status.NodeInfo.Architecture)
	f.add("Runtime", "%s", n.Status.NodeInfo.ContainerRuntimeVersion)
	f.add("Unschedulable", "%v", n.Spec.Unschedulable)
	f.add("Capacity", "cpu %s, memory %s, pods %s", n.Status.Capacity.Cpu().String(), formatMemory(n.Status.Capacity.Memory().Value()), n.Status.Capacity.Pods().String())
	f.section("Conditions")
	for _, c := range n.Status.Conditions {
		f.line("%s=%s (%s)", c.Type, c.Status, c.Message)
	}
	return f.buf.String(), nil
}
