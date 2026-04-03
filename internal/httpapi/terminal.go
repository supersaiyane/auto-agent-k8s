package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type kubectlRequest struct {
	Command string `json:"command"`
}

type kubectlResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// handleKubectl executes kubectl-like commands via the K8s API.
func (s *Server) handleKubectl(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, kubectlResponse{Error: "POST required"})
		return
	}
	if s.kc == nil {
		writeJSON(w, kubectlResponse{Error: "no cluster connection"})
		return
	}

	var req kubectlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, kubectlResponse{Error: "invalid request"})
		return
	}

	cmd := strings.TrimSpace(req.Command)
	// Strip leading "kubectl " if present
	cmd = strings.TrimPrefix(cmd, "kubectl ")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	output, err := executeKubectl(ctx, s.kc, cmd)
	resp := kubectlResponse{Output: output}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, resp)
}

func executeKubectl(ctx context.Context, kc kubernetes.Interface, cmd string) (string, error) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}

	verb := parts[0]
	switch verb {
	case "get":
		return handleGet(ctx, kc, parts[1:])
	case "describe":
		return handleDescribe(ctx, kc, parts[1:])
	case "logs":
		return handleLogs(ctx, kc, parts[1:])
	case "top":
		return handleTop(ctx, kc, parts[1:])
	case "version":
		return handleVersion(ctx, kc)
	case "cluster-info":
		return "Kubernetes control plane is running\nUse 'get nodes' for node info", nil
	case "help":
		return `Supported commands:
  get pods [-n <ns>] [-A]
  get deployments [-n <ns>]
  get services [-n <ns>]
  get nodes
  get namespaces
  get events [-n <ns>]
  get jobs [-n <ns>]
  get all [-n <ns>]
  describe pod <name> [-n <ns>]
  describe deploy <name> [-n <ns>]
  describe node <name>
  logs <pod> [-n <ns>] [-c <container>] [--tail <n>]
  top pods [-n <ns>]
  top nodes
  version`, nil
	default:
		return "", fmt.Errorf("unsupported command: %s\nType 'help' for supported commands", verb)
	}
}

func parseFlags(args []string) (resource, name, namespace, container string, allNs bool, tail int64) {
	namespace = "default"
	tail = 50
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--namespace":
			if i+1 < len(args) {
				namespace = args[i+1]
				i++
			}
		case "-A", "--all-namespaces":
			allNs = true
		case "-c", "--container":
			if i+1 < len(args) {
				container = args[i+1]
				i++
			}
		case "--tail":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &tail)
				i++
			}
		default:
			if resource == "" {
				resource = args[i]
			} else if name == "" {
				name = args[i]
			}
		}
	}
	return
}

func handleGet(ctx context.Context, kc kubernetes.Interface, args []string) (string, error) {
	resource, name, ns, _, allNs, _ := parseFlags(args)
	if allNs {
		ns = ""
	}

	switch resource {
	case "pods", "pod", "po":
		return getPods(ctx, kc, ns, name)
	case "deployments", "deployment", "deploy":
		return getDeployments(ctx, kc, ns, name)
	case "services", "service", "svc":
		return getServices(ctx, kc, ns)
	case "nodes", "node", "no":
		return getNodes(ctx, kc)
	case "namespaces", "namespace", "ns":
		return getNamespaces(ctx, kc)
	case "events", "event", "ev":
		return getEvents(ctx, kc, ns)
	case "jobs", "job":
		return getJobs(ctx, kc, ns)
	case "all":
		return getAll(ctx, kc, ns)
	default:
		return "", fmt.Errorf("unsupported resource: %s", resource)
	}
}

func getPods(ctx context.Context, kc kubernetes.Interface, ns, name string) (string, error) {
	if name != "" {
		pod, err := kc.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return formatPods([]corev1.Pod{*pod}, ns == ""), nil
	}
	pods, err := kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return fmt.Sprintf("No resources found in %s namespace.", ns), nil
	}
	return formatPods(pods.Items, ns == ""), nil
}

func formatPods(pods []corev1.Pod, showNs bool) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	if showNs {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tREADY\tSTATUS\tRESTARTS\tAGE\tNODE")
	} else {
		fmt.Fprintln(w, "NAME\tREADY\tSTATUS\tRESTARTS\tAGE\tNODE")
	}
	for _, p := range pods {
		ready, total := 0, len(p.Spec.Containers)
		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		status := podStatus(&p)
		if showNs {
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%d\t%s\t%s\n",
				p.Namespace, p.Name, ready, total, status, restarts, age(p.CreationTimestamp.Time), p.Spec.NodeName)
		} else {
			fmt.Fprintf(w, "%s\t%d/%d\t%s\t%d\t%s\t%s\n",
				p.Name, ready, total, status, restarts, age(p.CreationTimestamp.Time), p.Spec.NodeName)
		}
	}
	w.Flush()
	return buf.String()
}

func getDeployments(ctx context.Context, kc kubernetes.Interface, ns, name string) (string, error) {
	if name != "" {
		d, err := kc.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return formatDeploys([]appsv1.Deployment{*d}), nil
	}
	deploys, err := kc.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(deploys.Items) == 0 {
		return fmt.Sprintf("No resources found in %s namespace.", ns), nil
	}
	return formatDeploys(deploys.Items), nil
}

func formatDeploys(deploys []appsv1.Deployment) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREADY\tUP-TO-DATE\tAVAILABLE\tAGE")
	for _, d := range deploys {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		fmt.Fprintf(w, "%s\t%d/%d\t%d\t%d\t%s\n",
			d.Name, d.Status.ReadyReplicas, desired, d.Status.UpdatedReplicas, d.Status.AvailableReplicas, age(d.CreationTimestamp.Time))
	}
	w.Flush()
	return buf.String()
}

func getServices(ctx context.Context, kc kubernetes.Interface, ns string) (string, error) {
	svcs, err := kc.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tCLUSTER-IP\tPORT(S)\tAGE")
	for _, s := range svcs.Items {
		ports := ""
		for i, p := range s.Spec.Ports {
			if i > 0 {
				ports += ","
			}
			ports += fmt.Sprintf("%d/%s", p.Port, p.Protocol)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Spec.Type, s.Spec.ClusterIP, ports, age(s.CreationTimestamp.Time))
	}
	w.Flush()
	return buf.String(), nil
}

func getNodes(ctx context.Context, kc kubernetes.Interface) (string, error) {
	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tROLES\tAGE\tVERSION")
	for _, n := range nodes.Items {
		roles := ""
		for k := range n.Labels {
			if strings.HasPrefix(k, "node-role.kubernetes.io/") {
				if roles != "" {
					roles += ","
				}
				roles += strings.TrimPrefix(k, "node-role.kubernetes.io/")
			}
		}
		if roles == "" {
			roles = "<none>"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n.Name, nodeStatus(&n), roles, age(n.CreationTimestamp.Time), n.Status.NodeInfo.KubeletVersion)
	}
	w.Flush()
	return buf.String(), nil
}

func getNamespaces(ctx context.Context, kc kubernetes.Interface) (string, error) {
	nss, err := kc.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tAGE")
	for _, n := range nss.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\n", n.Name, n.Status.Phase, age(n.CreationTimestamp.Time))
	}
	w.Flush()
	return buf.String(), nil
}

func getEvents(ctx context.Context, kc kubernetes.Interface, ns string) (string, error) {
	events, err := kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tREASON\tOBJECT\tMESSAGE\tAGE")
	start := 0
	if len(events.Items) > 50 {
		start = len(events.Items) - 50
	}
	for i := start; i < len(events.Items); i++ {
		e := events.Items[i]
		ts := e.LastTimestamp.Time
		if ts.IsZero() {
			ts = e.CreationTimestamp.Time
		}
		obj := fmt.Sprintf("%s/%s", strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name)
		msg := e.Message
		if len(msg) > 80 {
			msg = msg[:80] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Type, e.Reason, obj, msg, age(ts))
	}
	w.Flush()
	return buf.String(), nil
}

func getJobs(ctx context.Context, kc kubernetes.Interface, ns string) (string, error) {
	jobs, err := kc.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tCOMPLETIONS\tAGE")
	for _, j := range jobs.Items {
		status := "Running"
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == "True" {
				status = "Complete"
			}
			if c.Type == batchv1.JobFailed && c.Status == "True" {
				status = "Failed"
			}
		}
		desired := int32(1)
		if j.Spec.Completions != nil {
			desired = *j.Spec.Completions
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\n", j.Name, status, j.Status.Succeeded, desired, age(j.CreationTimestamp.Time))
	}
	w.Flush()
	return buf.String(), nil
}

func getAll(ctx context.Context, kc kubernetes.Interface, ns string) (string, error) {
	var buf bytes.Buffer

	if pods, err := getPods(ctx, kc, ns, ""); err == nil && pods != "" {
		buf.WriteString("=== Pods ===\n")
		buf.WriteString(pods)
		buf.WriteString("\n")
	}
	if deploys, err := getDeployments(ctx, kc, ns, ""); err == nil && deploys != "" {
		buf.WriteString("=== Deployments ===\n")
		buf.WriteString(deploys)
		buf.WriteString("\n")
	}
	if svcs, err := getServices(ctx, kc, ns); err == nil && svcs != "" {
		buf.WriteString("=== Services ===\n")
		buf.WriteString(svcs)
		buf.WriteString("\n")
	}
	if jobs, err := getJobs(ctx, kc, ns); err == nil {
		buf.WriteString("=== Jobs ===\n")
		buf.WriteString(jobs)
	}
	return buf.String(), nil
}

func handleDescribe(ctx context.Context, kc kubernetes.Interface, args []string) (string, error) {
	resource, name, ns, _, _, _ := parseFlags(args)
	if name == "" {
		return "", fmt.Errorf("usage: describe <resource> <name> [-n <namespace>]")
	}

	switch resource {
	case "pod", "pods", "po":
		return describePod(ctx, kc, ns, name)
	case "deploy", "deployment", "deployments":
		return describeDeployment(ctx, kc, ns, name)
	case "node", "nodes":
		return describeNode(ctx, kc, name)
	default:
		return "", fmt.Errorf("describe not supported for: %s", resource)
	}
}

func describePod(ctx context.Context, kc kubernetes.Interface, ns, name string) (string, error) {
	p, err := kc.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Name:         %s\n", p.Name)
	fmt.Fprintf(&buf, "Namespace:    %s\n", p.Namespace)
	fmt.Fprintf(&buf, "Node:         %s\n", p.Spec.NodeName)
	fmt.Fprintf(&buf, "Status:       %s\n", p.Status.Phase)
	fmt.Fprintf(&buf, "IP:           %s\n", p.Status.PodIP)
	fmt.Fprintf(&buf, "Age:          %s\n", age(p.CreationTimestamp.Time))
	if len(p.Labels) > 0 {
		fmt.Fprintf(&buf, "Labels:       ")
		for k, v := range p.Labels {
			fmt.Fprintf(&buf, "%s=%s ", k, v)
		}
		buf.WriteString("\n")
	}
	buf.WriteString("\nContainers:\n")
	for _, cs := range p.Status.ContainerStatuses {
		fmt.Fprintf(&buf, "  %s:\n", cs.Name)
		fmt.Fprintf(&buf, "    Image:     %s\n", cs.Image)
		fmt.Fprintf(&buf, "    Ready:     %v\n", cs.Ready)
		fmt.Fprintf(&buf, "    Restarts:  %d\n", cs.RestartCount)
		if cs.State.Running != nil {
			fmt.Fprintf(&buf, "    State:     Running (since %s)\n", age(cs.State.Running.StartedAt.Time))
		} else if cs.State.Waiting != nil {
			fmt.Fprintf(&buf, "    State:     Waiting (%s)\n", cs.State.Waiting.Reason)
		} else if cs.State.Terminated != nil {
			fmt.Fprintf(&buf, "    State:     Terminated (%s, exit %d)\n", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
	}
	// Events
	events, _ := kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name,
	})
	if events != nil && len(events.Items) > 0 {
		buf.WriteString("\nEvents:\n")
		for _, e := range events.Items {
			fmt.Fprintf(&buf, "  %s  %s  %s: %s\n", e.Type, age(e.LastTimestamp.Time), e.Reason, e.Message)
		}
	}
	return buf.String(), nil
}

func describeDeployment(ctx context.Context, kc kubernetes.Interface, ns, name string) (string, error) {
	d, err := kc.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Name:               %s\n", d.Name)
	fmt.Fprintf(&buf, "Namespace:          %s\n", d.Namespace)
	fmt.Fprintf(&buf, "Replicas:           %d desired | %d updated | %d available | %d ready\n",
		desired, d.Status.UpdatedReplicas, d.Status.AvailableReplicas, d.Status.ReadyReplicas)
	fmt.Fprintf(&buf, "Strategy:           %s\n", d.Spec.Strategy.Type)
	fmt.Fprintf(&buf, "Age:                %s\n", age(d.CreationTimestamp.Time))
	buf.WriteString("\nConditions:\n")
	for _, c := range d.Status.Conditions {
		fmt.Fprintf(&buf, "  %s: %s (%s) %s\n", c.Type, c.Status, c.Reason, c.Message)
	}
	buf.WriteString("\nContainers:\n")
	for _, c := range d.Spec.Template.Spec.Containers {
		fmt.Fprintf(&buf, "  %s:\n", c.Name)
		fmt.Fprintf(&buf, "    Image:  %s\n", c.Image)
		if c.Resources.Limits != nil {
			fmt.Fprintf(&buf, "    Limits: cpu=%s, memory=%s\n",
				c.Resources.Limits.Cpu().String(), c.Resources.Limits.Memory().String())
		}
	}
	return buf.String(), nil
}

func describeNode(ctx context.Context, kc kubernetes.Interface, name string) (string, error) {
	n, err := kc.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Name:               %s\n", n.Name)
	fmt.Fprintf(&buf, "Status:             %s\n", nodeStatus(n))
	fmt.Fprintf(&buf, "Version:            %s\n", n.Status.NodeInfo.KubeletVersion)
	fmt.Fprintf(&buf, "OS:                 %s (%s)\n", n.Status.NodeInfo.OSImage, n.Status.NodeInfo.Architecture)
	fmt.Fprintf(&buf, "Kernel:             %s\n", n.Status.NodeInfo.KernelVersion)
	fmt.Fprintf(&buf, "Container Runtime:  %s\n", n.Status.NodeInfo.ContainerRuntimeVersion)
	fmt.Fprintf(&buf, "Unschedulable:      %v\n", n.Spec.Unschedulable)
	fmt.Fprintf(&buf, "Age:                %s\n", age(n.CreationTimestamp.Time))
	buf.WriteString("\nCapacity:\n")
	fmt.Fprintf(&buf, "  CPU:     %s\n", n.Status.Capacity.Cpu().String())
	fmt.Fprintf(&buf, "  Memory:  %s\n", formatMemory(n.Status.Capacity.Memory().Value()))
	fmt.Fprintf(&buf, "  Pods:    %s\n", n.Status.Capacity.Pods().String())
	buf.WriteString("\nConditions:\n")
	for _, c := range n.Status.Conditions {
		fmt.Fprintf(&buf, "  %s: %s (%s)\n", c.Type, c.Status, c.Message)
	}
	return buf.String(), nil
}

func handleLogs(ctx context.Context, kc kubernetes.Interface, args []string) (string, error) {
	_, name, ns, container, _, tail := parseFlags(args)
	if name == "" {
		return "", fmt.Errorf("usage: logs <pod-name> [-n <namespace>] [-c <container>] [--tail <n>]")
	}
	opts := &corev1.PodLogOptions{TailLines: &tail}
	if container != "" {
		opts.Container = container
	}
	req := kc.CoreV1().Pods(ns).GetLogs(name, opts)
	result, err := req.DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(result), nil
}

func handleTop(ctx context.Context, kc kubernetes.Interface, args []string) (string, error) {
	resource, _, _, _, _, _ := parseFlags(args)
	switch resource {
	case "nodes", "node":
		return "top nodes requires metrics-server API (not implemented in web terminal)\nUse 'get nodes' for node status", nil
	case "pods", "pod":
		return "top pods requires metrics-server API (not implemented in web terminal)\nUse 'get pods -A' for pod status", nil
	default:
		return "", fmt.Errorf("usage: top [nodes|pods]")
	}
}

func handleVersion(ctx context.Context, kc kubernetes.Interface) (string, error) {
	sv, err := kc.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Server Version: %s\nPlatform: %s", sv.GitVersion, sv.Platform), nil
}
