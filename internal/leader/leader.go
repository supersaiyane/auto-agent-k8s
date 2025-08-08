package leader

import (
    "context"
    "sync/atomic"

    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/leaderelection"
    "k8s.io/client-go/tools/leaderelection/resourcelock"
)

type Elector struct{ leader int32 }
func (e *Elector) IsLeader() bool { return atomic.LoadInt32(&e.leader)==1 }

func Start(ctx context.Context, kc *kubernetes.Clientset, name string) *Elector {
    e := &Elector{}
    lock, _ := resourcelock.New(resourcelock.LeasesResourceLock, "kube-system", name,
        kc.CoreV1(), kc.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: name})
    go leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
        Lock: lock, ReleaseOnCancel: true,
        LeaseDuration: 15e9, RenewDeadline: 10e9, RetryPeriod: 2e9,
        Callbacks: leaderelection.LeaderCallbacks{
            OnStartedLeading: func(context.Context){ atomic.StoreInt32(&e.leader,1) },
            OnStoppedLeading: func(){ atomic.StoreInt32(&e.leader,0) },
        },
    })
    return e
}
