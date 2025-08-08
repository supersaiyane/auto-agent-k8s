package main

import (
    "context"
    "os"
    "os/signal"
    "syscall"
    "time"

    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/dynamic"
    "k8s.io/klog/v2"

    "github.com/yourorg/auto-agent/internal/httpapi"
    "github.com/yourorg/auto-agent/internal/leader"
    "github.com/yourorg/auto-agent/internal/kube"
    "github.com/yourorg/auto-agent/internal/metrics"
    "github.com/yourorg/auto-agent/internal/policy"
    "github.com/yourorg/auto-agent/internal/slack"
    "github.com/yourorg/auto-agent/internal/llm"
    "github.com/yourorg/auto-agent/internal/crd"
)

func main() {
    klog.InitFlags(nil)
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    cfg, err := rest.InClusterConfig()
    if err != nil { klog.Fatalf("in-cluster config: %v", err) }

    kc, err := kubernetes.NewForConfig(cfg)
    if err != nil { klog.Fatalf("kube client: %v", err) }
    dyn, err := dynamic.NewForConfig(cfg)
    if err != nil { klog.Fatalf("dynamic client: %v", err) }

    pol := policy.LoadFromEnv()
    sl := slack.New(os.Getenv("SLACK_WEBHOOK_URL"))
    ll := llm.New(os.Getenv("LLM_API_URL"), os.Getenv("LLM_API_KEY"), os.Getenv("LLM_MODEL"), pol.LLMEnabled)

    mp, err := metrics.NewProviderFromEnv(ctx)
    if err != nil { klog.Fatalf("metrics provider: %v", err) }

    // health + metrics endpoint
    go httpapi.Serve(":8080")

    // CRD controller
    store := crd.NewStore()
    crd.StartController(ctx, dyn, store)

    // leader election (for cluster-wide scaling)
    le := leader.Start(ctx, kc, "auto-agent-leader")

    // start pod watcher: node-local remediation
    go kube.WatchPods(ctx, kc, mp, pol, sl, ll)

    // scaling + anomalies (leader-only)
    go func() {
        t := time.NewTicker(30 * time.Second)
        defer t.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-t.C:
                if !le.IsLeader() { continue }
                kube.EvaluateAndScale(ctx, kc, mp, pol, sl, ll) // global policy + values
                kube.CheckAnomalies(ctx, kc, mp, pol, sl, ll, store) // CRD-driven anomalies
            }
        }
    }()

    <-ctx.Done()
}
