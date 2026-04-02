package kube

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
)

const informerResyncPeriod = 5 * time.Minute

// StartWatchers initializes pod and node informers with event handlers.
func StartWatchers(ctx context.Context, deps *Deps) {
	factory := informers.NewSharedInformerFactory(deps.Client, informerResyncPeriod)

	// Pod watcher
	podInf := factory.Core().V1().Pods().Informer()
	podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, okOld := oldObj.(*corev1.Pod)
			newPod, okNew := newObj.(*corev1.Pod)
			if !okOld || !okNew {
				return
			}
			handlePodUpdate(ctx, deps, oldPod, newPod)
		},
	})

	// Node watcher
	nodeInf := factory.Core().V1().Nodes().Informer()
	nodeInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, okOld := oldObj.(*corev1.Node)
			newNode, okNew := newObj.(*corev1.Node)
			if !okOld || !okNew {
				return
			}
			// Only handle if pressure state actually changed
			if pressureChanged(oldNode, newNode) {
				go handleNodePressure(ctx, deps, newNode)
			}
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	klog.Infof("watcher: informers synced and running")
}

// handlePodUpdate dispatches pod status changes to appropriate handlers.
func handlePodUpdate(ctx context.Context, deps *Deps, oldPod, newPod *corev1.Pod) {
	if !deps.Policy.AllowedNamespace(newPod.Namespace) {
		return
	}
	if hasAnnotation(newPod, deps.Policy.ExcludedAnnotation) {
		return
	}

	for _, cs := range newPod.Status.ContainerStatuses {
		// CrashLoopBackOff
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			key := dedupKey(newPod.Namespace, newPod.Name, "CrashLoopBackOff")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("CrashLoopBackOff").Inc()
				return
			}
			go handleCrashLoop(ctx, deps, newPod, cs.Name)
			return
		}

		// ImagePullBackOff / ErrImagePull
		if cs.State.Waiting != nil &&
			(cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull") {
			key := dedupKey(newPod.Namespace, newPod.Name, "ImagePullBackOff")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("ImagePullBackOff").Inc()
				return
			}
			go handleImagePullBackOff(ctx, deps, newPod, cs.Name)
			return
		}

		// OOMKilled
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			key := dedupKey(newPod.Namespace, newPod.Name, "OOMKilled")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("OOMKilled").Inc()
				return
			}
			go handleOOM(ctx, deps, newPod, cs.Name)
			return
		}

		// NotReady (container running but not ready for extended period)
		if cs.State.Running != nil && !cs.Ready && cs.RestartCount == 0 {
			if cs.State.Running.StartedAt.Time.Before(time.Now().Add(-3 * time.Minute)) {
				key := dedupKey(newPod.Namespace, newPod.Name, "NotReady")
				if !deps.Dedup.Check(key) {
					obs.DedupSkippedTotal.WithLabelValues("NotReady").Inc()
					return
				}
				go handleNotReady(ctx, deps, newPod, cs.Name)
				return
			}
		}
	}

	// Pending pod detection
	if newPod.Status.Phase == corev1.PodPending {
		// Only trigger if pending for > 5 minutes
		if newPod.CreationTimestamp.Time.Before(time.Now().Add(-5 * time.Minute)) {
			key := dedupKey(newPod.Namespace, newPod.Name, "Pending")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("Pending").Inc()
				return
			}
			go handlePending(ctx, deps, newPod)
		}
	}
}

// pressureChanged returns true if node memory or disk pressure status changed.
func pressureChanged(oldNode, newNode *corev1.Node) bool {
	return conditionStatus(oldNode, corev1.NodeMemoryPressure) != conditionStatus(newNode, corev1.NodeMemoryPressure) ||
		conditionStatus(oldNode, corev1.NodeDiskPressure) != conditionStatus(newNode, corev1.NodeDiskPressure)
}

func conditionStatus(node *corev1.Node, condType corev1.NodeConditionType) corev1.ConditionStatus {
	for _, c := range node.Status.Conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return corev1.ConditionUnknown
}
