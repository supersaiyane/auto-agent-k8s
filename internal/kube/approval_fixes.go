package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The R3 fixes (PLAN-002 phase 15, C2): each builds the exact change a
// finding proposes. Nothing here runs until an approver accepts it, and
// then only through applyMutation. A change may be approved long after the
// check pass that built it ended, so each Apply uses its own bounded
// context instead of the pass's.

const (
	approvalApplyTimeout = 30 * time.Second
	storeTimeout         = 10 * time.Second // one read and write of the stored queue (approval_store.go)
)

func approvalCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), approvalApplyTimeout)
}

// fixSpec is an R3 change as data, so the approval queue can store it
// (PLAN-004, ISS-078). buildFix turns it back into the change; every
// propose function goes through buildFix, so a stored approval applies
// exactly what a fresh one would.
type fixSpec struct {
	Action    string   `json:"action"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`             // the object changed
	Kind      string   `json:"kind,omitempty"`   // raise_cpu_limit: Deployment, StatefulSet or DaemonSet
	Target    string   `json:"target,omitempty"` // raise_hpa_max: the workload the autoscaler scales
	From      int32    `json:"from,omitempty"`
	To        int32    `json:"to,omitempty"`
	Size      string   `json:"size,omitempty"` // expand_pvc, e.g. "20Gi"
	Pods      []string `json:"pods,omitempty"`
	Container string   `json:"container,omitempty"`
	CPU       string   `json:"cpu,omitempty"`
	Job       string   `json:"job,omitempty"` // run_cronjob_now: the Job to create
}

var errUnknownFix = errors.New("unknown fix")

// buildFix builds the mutation for s.
func buildFix(deps *Deps, s fixSpec) (*mutation, error) {
	var m *mutation
	switch s.Action {
	case "raise_hpa_max":
		m = hpaMaxFix(deps, s)
	case "resume_deployment":
		m = resumeFix(deps, s)
	case "expand_pvc":
		m = expandPVCFix(deps, s)
	case "delete_stuck_pods":
		m = deleteStuckFix(deps, s)
	case "run_cronjob_now":
		m = runNowFix(deps, s)
	case "raise_cpu_limit":
		var err error
		if m, err = cpuLimitFix(deps, s); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w %q", errUnknownFix, s.Action)
	}
	m.Fix = &s
	return m, nil
}

// proposeHPAMax raises an autoscaler's maxReplicas.
func proposeHPAMax(deps *Deps, hpa *autoscalingv2.HorizontalPodAutoscaler, target string, to int32) *mutation {
	return mustFix(deps, fixSpec{Action: "raise_hpa_max", Namespace: hpa.Namespace, Name: hpa.Name, Target: target, From: hpa.Spec.MaxReplicas, To: to})
}

// mustFix builds a spec whose action is one of the cases above.
func mustFix(deps *Deps, s fixSpec) *mutation {
	m, err := buildFix(deps, s)
	if err != nil {
		panic(err) // a propose function named an action buildFix does not know
	}
	return m
}

func hpaMaxFix(deps *Deps, s fixSpec) *mutation {
	ns, name, to := s.Namespace, s.Name, s.To
	return &mutation{Namespace: ns, Workload: s.Target, Reason: "HPAMaxedOut", ActionType: s.Action,
		SuccessMsg: fmt.Sprintf("raised maxReplicas of autoscaler %s to %d", name, to),
		SuggestMsg: fmt.Sprintf("raise maxReplicas of autoscaler %s from %d to %d", name, s.From, to),
		Apply: func() error {
			ctx, cancel := approvalCtx()
			defer cancel()
			_, err := deps.Client.AutoscalingV2().HorizontalPodAutoscalers(ns).Patch(ctx, name, types.MergePatchType,
				mergePatch(map[string]any{"spec": map[string]any{"maxReplicas": to}}), metav1.PatchOptions{})
			return err
		}}
}

// proposeResume resumes a paused Deployment.
func proposeResume(deps *Deps, d *appsv1.Deployment) *mutation {
	return mustFix(deps, fixSpec{Action: "resume_deployment", Namespace: d.Namespace, Name: d.Name})
}

func resumeFix(deps *Deps, s fixSpec) *mutation {
	ns, name := s.Namespace, s.Name
	return &mutation{Namespace: ns, Workload: "deployment/" + name, Reason: "DeploymentPaused", ActionType: s.Action,
		SuccessMsg: "resumed deployment " + name, SuggestMsg: "resume deployment " + name,
		Apply: func() error {
			ctx, cancel := approvalCtx()
			defer cancel()
			_, err := deps.Client.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType,
				mergePatch(map[string]any{"spec": map[string]any{"paused": false}}), metav1.PatchOptions{})
			return err
		}}
}

// proposeExpandPVC grows a claim whose StorageClass allows expansion.
func proposeExpandPVC(deps *Deps, ns, claim string, gi int64) *mutation {
	return mustFix(deps, fixSpec{Action: "expand_pvc", Namespace: ns, Name: claim, Size: fmt.Sprintf("%dGi", gi)})
}

func expandPVCFix(deps *Deps, s fixSpec) *mutation {
	ns, claim, size := s.Namespace, s.Name, s.Size
	return &mutation{Namespace: ns, Workload: "pvc/" + claim, Reason: "VolumeAlmostFull", ActionType: s.Action,
		SuccessMsg: fmt.Sprintf("asked to expand claim %s to %s", claim, size),
		SuggestMsg: fmt.Sprintf("expand claim %s to %s", claim, size),
		Apply: func() error {
			ctx, cancel := approvalCtx()
			defer cancel()
			_, err := deps.Client.CoreV1().PersistentVolumeClaims(ns).Patch(ctx, claim, types.MergePatchType,
				mergePatch(map[string]any{"spec": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": size}}}}),
				metav1.PatchOptions{})
			return err
		}}
}

// proposeDeleteStuck deletes DaemonSet pods that stay not ready; the
// DaemonSet recreates each on its node.
func proposeDeleteStuck(deps *Deps, ds *appsv1.DaemonSet, pods []string) *mutation {
	return mustFix(deps, fixSpec{Action: "delete_stuck_pods", Namespace: ds.Namespace, Name: ds.Name, Pods: append([]string(nil), pods...)})
}

func deleteStuckFix(deps *Deps, s fixSpec) *mutation {
	ns, pods := s.Namespace, s.Pods
	return &mutation{Namespace: ns, Workload: "daemonset/" + s.Name, Reason: "DaemonSetMissing", ActionType: s.Action,
		SuccessMsg: fmt.Sprintf("deleted stuck pods %s", strings.Join(pods, ", ")),
		SuggestMsg: fmt.Sprintf("delete stuck pods %s so the DaemonSet recreates them", strings.Join(pods, ", ")),
		Apply: func() error {
			ctx, cancel := approvalCtx()
			defer cancel()
			for _, p := range pods {
				if err := deps.Client.CoreV1().Pods(ns).Delete(ctx, p, metav1.DeleteOptions{}); err != nil {
					return fmt.Errorf("delete pod %s: %w", p, err)
				}
			}
			return nil
		}}
}

// manualJobName is "<cronjob>-manual-<unix seconds>", cut to the 63
// characters a name label allows.
func manualJobName(cron string, now time.Time) string {
	const suffixLen = len("-manual-") + 10
	if len(cron) > 63-suffixLen {
		cron = strings.TrimRight(cron[:63-suffixLen], "-.")
	}
	return fmt.Sprintf("%s-manual-%d", cron, now.Unix())
}

// proposeRunNow starts one Job from a CronJob's template, as
// `kubectl create job --from=cronjob/<name>` does.
func proposeRunNow(deps *Deps, cj *batchv1.CronJob, now time.Time) *mutation {
	return mustFix(deps, fixSpec{Action: "run_cronjob_now", Namespace: cj.Namespace, Name: cj.Name, Job: manualJobName(cj.Name, now)})
}

// runNowFix reads the CronJob when the change is applied, so the Job runs
// the template as it is at approval time, as kubectl would.
func runNowFix(deps *Deps, s fixSpec) *mutation {
	ns, cron, jobName := s.Namespace, s.Name, s.Job
	return &mutation{Namespace: ns, Workload: "cronjob/" + cron, Reason: "CronJobMissed", ActionType: s.Action,
		SuccessMsg: "started job " + jobName, SuggestMsg: fmt.Sprintf("start job %s from cronjob %s now", jobName, cron),
		Apply: func() error {
			ctx, cancel := approvalCtx()
			defer cancel()
			cj, err := deps.Client.BatchV1().CronJobs(ns).Get(ctx, cron, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("read cronjob %s: %w", cron, err)
			}
			_, err = deps.Client.BatchV1().Jobs(ns).Create(ctx, jobFromCronJob(cj, jobName), metav1.CreateOptions{})
			return err
		}}
}

// jobFromCronJob is the Job `kubectl create job --from` would create.
func jobFromCronJob(cj *batchv1.CronJob, name string) *batchv1.Job {
	annotations := map[string]string{"cronjob.kubernetes.io/instantiate": "manual"}
	for k, v := range cj.Spec.JobTemplate.Annotations {
		annotations[k] = v
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cj.Namespace, Labels: cj.Spec.JobTemplate.Labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cj, batchv1.SchemeGroupVersion.WithKind("CronJob"))}},
		Spec: *cj.Spec.JobTemplate.Spec.DeepCopy(),
	}
}

// templateOwner is the workload whose pod template a pod comes from:
// a Deployment (through its ReplicaSet), a StatefulSet or a DaemonSet.
// The Deployment name comes from the ReplicaSet name less the pod's
// pod-template-hash, so no ReplicaSet read is needed.
func templateOwner(p *corev1.Pod) (kind, name string, ok bool) {
	for _, o := range p.OwnerReferences {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		switch o.Kind {
		case "StatefulSet", "DaemonSet":
			return o.Kind, o.Name, true
		case "ReplicaSet":
			hash := p.Labels["pod-template-hash"]
			if hash != "" && strings.HasSuffix(o.Name, "-"+hash) {
				return "Deployment", strings.TrimSuffix(o.Name, "-"+hash), true
			}
		}
	}
	return "", "", false
}

// cpuLimitPatch is the strategic merge patch that sets one container's CPU
// limit; containers are matched by name and the others are left alone.
func cpuLimitPatch(container, cpu string) ([]byte, error) {
	return json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []map[string]any{{"name": container, "resources": map[string]any{"limits": map[string]any{"cpu": cpu}}}}}}}})
}

// proposeCPULimit raises one container's CPU limit in its workload's pod
// template. It returns nil when the pod has no such workload.
func proposeCPULimit(deps *Deps, p *corev1.Pod, container string, to *resource.Quantity) (*mutation, error) {
	kind, name, ok := templateOwner(p)
	if !ok {
		return nil, nil
	}
	return buildFix(deps, fixSpec{Action: "raise_cpu_limit", Namespace: p.Namespace, Name: name, Kind: kind, Container: container, CPU: to.String()})
}

func cpuLimitFix(deps *Deps, s fixSpec) (*mutation, error) {
	body, err := cpuLimitPatch(s.Container, s.CPU)
	if err != nil {
		return nil, fmt.Errorf("cpu limit patch: %w", err)
	}
	ns, name, kind := s.Namespace, s.Name, s.Kind
	if kind != "Deployment" && kind != "StatefulSet" && kind != "DaemonSet" {
		return nil, fmt.Errorf("%w: raise_cpu_limit on kind %q", errUnknownFix, kind)
	}
	workload := strings.ToLower(kind) + "/" + name
	return &mutation{Namespace: ns, Workload: workload, Reason: "CPUThrottled", ActionType: s.Action,
		SuccessMsg: fmt.Sprintf("raised the CPU limit of %s in %s to %s", s.Container, workload, s.CPU),
		SuggestMsg: fmt.Sprintf("raise the CPU limit of container %s in %s to %s (rolls the pods)", s.Container, workload, s.CPU),
		Apply: func() error {
			ctx, cancel := approvalCtx()
			defer cancel()
			var err error
			switch kind {
			case "Deployment":
				_, err = deps.Client.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{})
			case "StatefulSet":
				_, err = deps.Client.AppsV1().StatefulSets(ns).Patch(ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{})
			case "DaemonSet":
				_, err = deps.Client.AppsV1().DaemonSets(ns).Patch(ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{})
			}
			return err
		}}, nil
}
