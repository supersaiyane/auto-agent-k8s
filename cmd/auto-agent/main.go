package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/logging"
)

// version is set at build time: -ldflags "-X main.version=<v>" (ISS-018).
var version = "dev"

// main only reads configuration, builds in-cluster clients and calls run
// (PLAN-002 9.5); everything else lives in run.go and is tested there.
func main() {
	klog.InitFlags(nil)
	conf := config.Load(os.Getenv) // the only environment read in the agent
	if v, ok := config.KlogVerbosity(conf.Policy.LogLevel); ok {
		_ = flag.Set("v", strconv.Itoa(v)) // agent.logLevel (ISS-032)
	} else {
		klog.Warningf("unknown LOG_LEVEL %q, keeping klog defaults", conf.Policy.LogLevel)
	}
	logging.Init(conf.LogFormat)

	rc, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("in-cluster config: %v", err)
	}
	rc.QPS, rc.Burst = 50, 100
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		klog.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		klog.Fatalf("dynamic client: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, conf, Clients{Kube: kc, Dynamic: dyn}, RunOptions{}); err != nil {
		klog.Fatalf("auto-agent: %v", err)
	}
}
