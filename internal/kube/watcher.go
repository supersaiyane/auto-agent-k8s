package kube

import (
	"context"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
)

const (
	informerResyncPeriod = 5 * time.Minute
	maxConcurrentHandlers = 20
	handlerTimeout        = 60 * time.Second
)

// handlerSem limits the number of concurrent handler goroutines.
var handlerSem = make(chan struct{}, maxConcurrentHandlers)

// runHandler launches a handler with concurrency limiting and a timeout.
func runHandler(parentCtx context.Context, fn func(ctx context.Context)) {
	select {
	case handlerSem <- struct{}{}:
	default:
		klog.V(2).Infof("watcher: handler pool full (%d), dropping event", maxConcurrentHandlers)
		return
	}
	go func() {
		defer func() { <-handlerSem }()
		ctx, cancel := context.WithTimeout(parentCtx, handlerTimeout)
		defer cancel()
		fn(ctx)
	}()
}

// StartWatchers initializes pod and node informers with event handlers.
func StartWatchers(ctx context.Context, deps *Deps) {
	// Pod informer: filter to local node only (NODE_NAME set via downward API)
	nodeName := os.Getenv("NODE_NAME")
	var factory informers.SharedInformerFactory
	if nodeName != "" {
		klog.Infof("watcher: filtering pod informer to node %s", nodeName)
		factory = informers.NewSharedInformerFactoryWithOptions(
			deps.Client, informerResyncPeriod,
			informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
				opts.FieldSelector = "spec.nodeName=" + nodeName
			}),
		)
	} else {
		klog.Warningf("watcher: NODE_NAME not set, watching all pods cluster-wide (not recommended)")
		factory = informers.NewSharedInformerFactory(deps.Client, informerResyncPeriod)
	}

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

	// Node informer: separate factory (unfiltered — nodes are cluster-scoped)
	nodeFactory := informers.NewSharedInformerFactory(deps.Client, informerResyncPeriod)
	nodeInf := nodeFactory.Core().V1().Nodes().Informer()
	nodeInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, okOld := oldObj.(*corev1.Node)
			newNode, okNew := newObj.(*corev1.Node)
			if !okOld || !okNew {
				return
			}
			if pressureChanged(oldNode, newNode) {
				runHandler(ctx, func(hCtx context.Context) {
					handleNodePressure(hCtx, deps, oldNode, newNode)
				})
			}
		},
	})

	factory.Start(ctx.Done())
	nodeFactory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	nodeFactory.WaitForCacheSync(ctx.Done())
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

	wl := ownerName(newPod) // workload-level dedup key

	// Init container failures
	for _, ics := range newPod.Status.InitContainerStatuses {
		if ics.State.Waiting != nil {
			r := ics.State.Waiting.Reason
			if r == "CrashLoopBackOff" || r == "Error" || r == "ImagePullBackOff" {
				key := dedupKey(newPod.Namespace, wl, "InitFailed-"+ics.Name)
				if !deps.Dedup.Check(key) {
					obs.DedupSkippedTotal.WithLabelValues("InitContainerFailed").Inc()
					return
				}
				runHandler(ctx, func(hCtx context.Context) {
					handleInitContainerFailure(hCtx, deps, newPod, ics.Name, r)
				})
				return
			}
		}
	}

	for _, cs := range newPod.Status.ContainerStatuses {
		// CrashLoopBackOff
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			key := dedupKey(newPod.Namespace, wl, "CrashLoopBackOff")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("CrashLoopBackOff").Inc()
				return
			}
			runHandler(ctx, func(hCtx context.Context) {
				handleCrashLoop(hCtx, deps, newPod, cs.Name)
			})
			return
		}

		// ImagePullBackOff / ErrImagePull
		if cs.State.Waiting != nil &&
			(cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull") {
			key := dedupKey(newPod.Namespace, wl, "ImagePullBackOff")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("ImagePullBackOff").Inc()
				return
			}
			runHandler(ctx, func(hCtx context.Context) {
				handleImagePullBackOff(hCtx, deps, newPod, cs.Name)
			})
			return
		}

		// OOMKilled
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			key := dedupKey(newPod.Namespace, wl, "OOMKilled")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("OOMKilled").Inc()
				return
			}
			runHandler(ctx, func(hCtx context.Context) {
				handleOOM(hCtx, deps, newPod, cs.Name)
			})
			return
		}

		// CreateContainerConfigError
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CreateContainerConfigError" {
			key := dedupKey(newPod.Namespace, wl, "ConfigError")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("ConfigError").Inc()
				return
			}
			runHandler(ctx, func(hCtx context.Context) {
				handleConfigError(hCtx, deps, newPod, cs.Name, cs.State.Waiting.Message)
			})
			return
		}

		// Restart storm (>5 restarts but not yet in CrashLoopBackOff)
		if cs.RestartCount >= 5 && (cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff") {
			key := dedupKey(newPod.Namespace, wl, "RestartStorm")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("RestartStorm").Inc()
				return
			}
			runHandler(ctx, func(hCtx context.Context) {
				handleRestartStorm(hCtx, deps, newPod, cs.Name, cs.RestartCount)
			})
			return
		}

		// Additional pod waiting reasons (RunContainerError, ContainerCannotRun, etc.)
		if cs.State.Waiting != nil && isAdditionalPodReason(cs.State.Waiting.Reason) {
			reason := cs.State.Waiting.Reason
			key := dedupKey(newPod.Namespace, wl, reason)
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues(reason).Inc()
				return
			}
			runHandler(ctx, func(hCtx context.Context) {
				handleAdditionalPodIssue(hCtx, deps, newPod, cs.Name, reason, cs.State.Waiting.Message)
			})
			return
		}

		// NotReady (container running but not ready for extended period)
		if cs.State.Running != nil && !cs.Ready && cs.RestartCount == 0 {
			if cs.State.Running.StartedAt.Time.Before(time.Now().Add(-3 * time.Minute)) {
				key := dedupKey(newPod.Namespace, wl, "NotReady")
				if !deps.Dedup.Check(key) {
					obs.DedupSkippedTotal.WithLabelValues("NotReady").Inc()
					return
				}
				runHandler(ctx, func(hCtx context.Context) {
					handleNotReady(hCtx, deps, newPod, cs.Name)
				})
				return
			}
		}
	}

	// Pending pod detection
	if newPod.Status.Phase == corev1.PodPending {
		if newPod.CreationTimestamp.Time.Before(time.Now().Add(-5 * time.Minute)) {
			key := dedupKey(newPod.Namespace, wl, "Pending")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("Pending").Inc()
				return
			}
			runHandler(ctx, func(hCtx context.Context) {
				handlePending(hCtx, deps, newPod)
			})
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
