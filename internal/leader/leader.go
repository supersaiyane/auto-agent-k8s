package leader

import (
	"context"
	"os"
	"sync/atomic"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

type Elector struct{ leader int32 }

func (e *Elector) IsLeader() bool { return atomic.LoadInt32(&e.leader) == 1 }

func Start(ctx context.Context, kc kubernetes.Interface, namespace, name, identity string) *Elector {
	e := &Elector{}

	// Each pod must have a unique identity for leader election.
	// The caller passes POD_NAME (downward API); fall back to the hostname.
	if identity == "" {
		var err error
		identity, err = os.Hostname()
		if err != nil {
			klog.Fatalf("cannot determine leader identity: %v", err)
		}
	}
	klog.Infof("leader election identity: %s", identity)

	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		namespace,
		name,
		kc.CoreV1(),
		kc.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: identity},
	)
	if err != nil {
		klog.Fatalf("failed to create leader lock: %v", err)
	}

	go leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15e9, // 15s
		RenewDeadline:   10e9, // 10s
		RetryPeriod:     2e9,  // 2s
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {
				klog.Infof("acquired leader lease")
				atomic.StoreInt32(&e.leader, 1)
			},
			OnStoppedLeading: func() {
				klog.Infof("lost leader lease")
				atomic.StoreInt32(&e.leader, 0)
			},
			OnNewLeader: func(identity string) {
				klog.Infof("current leader: %s", identity)
			},
		},
	})
	return e
}
