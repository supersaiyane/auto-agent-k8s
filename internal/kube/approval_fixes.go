package kube

import (
	"context"
	"encoding/json"
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

const approvalApplyTimeout = 30 * time.Second

func approvalCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), approvalApplyTimeout)
}

// proposeHPAMax raises an autoscaler's maxReplicas.
func proposeHPAMax(deps *Deps, hpa *autoscalingv2.HorizontalPodAutoscaler, target string, to int32) *mutation {
	ns, name := hpa.Namespace, hpa.Name
	return &mutation{Namespace: ns, Workload: target, Reason: "HPAMaxedOut", ActionType: "raise_hpa_max",
		SuccessMsg: fmt.Sprintf("raised maxReplicas of autoscaler %s to %d", name, to),
		SuggestMsg: fmt.Sprintf("raise maxReplicas of autoscaler %s from %d to %d", name, hpa.Spec.MaxReplicas, to),
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
	ns, name := d.Namespace, d.Name
	return &mutation{Namespace: ns, Workload: "deployment/" + name, Reason: "DeploymentPaused", ActionType: "resume_deployment",
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
	size := fmt.Sprintf("%dGi", gi)
	return &mutation{Namespace: ns, Workload: "pvc/" + claim, Reason: "VolumeAlmostFull", ActionType: "expand_pvc",
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
	ns := ds.Namespace
	return &mutation{Namespace: ns, Workload: "daemonset/" + ds.Name, Reason: "DaemonSetMissing", ActionType: "delete_stuck_pods",
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
	ns := cj.Namespace
	annotations := map[string]string{"cronjob.kubernetes.io/instantiate": "manual"}
	for k, v := range cj.Spec.JobTemplate.Annotations {
		annotations[k] = v
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: manualJobName(cj.Name, now), Namespace: ns, Labels: cj.Spec.JobTemplate.Labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cj, batchv1.SchemeGroupVersion.WithKind("CronJob"))}},
		Spec: *cj.Spec.JobTemplate.Spec.DeepCopy(),
	}
	return &mutation{Namespace: ns, Workload: "cronjob/" + cj.Name, Reason: "CronJobMissed", ActionType: "run_cronjob_now",
		SuccessMsg: "started job " + job.Name, SuggestMsg: fmt.Sprintf("start job %s from cronjob %s now", job.Name, cj.Name),
		Apply: func() error {
			ctx, cancel := approvalCtx()
			defer cancel()
			_, err := deps.Client.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
			return err
		}}
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
	body, err := cpuLimitPatch(container, to.String())
	if err != nil {
		return nil, fmt.Errorf("cpu limit patch: %w", err)
	}
	ns, workload := p.Namespace, strings.ToLower(kind)+"/"+name
	return &mutation{Namespace: ns, Workload: workload, Reason: "CPUThrottled", ActionType: "raise_cpu_limit",
		SuccessMsg: fmt.Sprintf("raised the CPU limit of %s in %s to %s", container, workload, to.String()),
		SuggestMsg: fmt.Sprintf("raise the CPU limit of container %s in %s to %s (rolls the pods)", container, workload, to.String()),
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
