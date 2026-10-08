package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/logging"
)

// version is set at build time: -ldflags "-X main.version=<v>" (ISS-018).
var version = "dev"

// main reads configuration, builds clients and calls run (PLAN-002 9.5).
// The environment is read here and in internal/config only.
func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(command(os.Args[1:], os.Getenv, config.Environ(), os.Stdout))
	}
	klog.InitFlags(nil)
	conf := config.Load(os.Getenv)
	setLogLevel(conf.Policy.LogLevel)
	logging.Init(conf.LogFormat)
	for _, k := range config.UnknownKeys(config.Environ()) {
		klog.Warningf("config: %s is set but the agent does not read it; check the spelling (auto-agent check-config)", k)
	}

	rc, err := restConfig(rest.InClusterConfig)
	if err != nil {
		klog.Fatalf("kubernetes config: %v", err)
	}
	cl, err := newClients(rc, conf)
	if err != nil {
		klog.Fatalf("%v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, conf, cl, RunOptions{}); err != nil {
		klog.Fatalf("auto-agent: %v", err)
	}
}

// command runs a subcommand and returns the exit code (ISS-056).
func command(args []string, get config.Getenv, environ []string, w io.Writer) int {
	switch args[0] {
	case "version":
		fmt.Fprintln(w, version)
		return 0
	case "check-config":
		return checkConfig(get, environ, w)
	}
	fmt.Fprintf(w, "usage: auto-agent [version | check-config]\nunknown command %q\n", args[0])
	return 2
}

// checkConfig prints the effective scope and every setting, secrets
// redacted, and names keys the agent does not read. It exits 1 when there
// are unknown keys, so it can gate a deployment.
func checkConfig(get config.Getenv, environ []string, w io.Writer) int {
	conf := config.Load(get)
	fmt.Fprintf(w, "role=%s mode=%s %s\n\n", conf.Role, conf.Policy.Mode, scopeSummary(conf.Policy))
	for _, line := range config.Describe(get) {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "\nreserved, not implemented yet (PLAN-002 phase 17): %s\n", strings.Join(config.Reserved(), ", "))
	unknown := config.UnknownKeys(environ)
	if len(unknown) == 0 {
		fmt.Fprintln(w, "\nno unknown keys")
		return 0
	}
	fmt.Fprintf(w, "\nunknown keys (not read by the agent): %s\n", strings.Join(unknown, ", "))
	return 1
}

// setLogLevel maps agent.logLevel to klog -v (ISS-032), reporting a failure
// instead of discarding it (ISS-056).
func setLogLevel(level string) {
	v, ok := config.KlogVerbosity(level)
	if !ok {
		klog.Warningf("unknown LOG_LEVEL %q, keeping klog defaults", level)
		return
	}
	if err := flag.Set("v", strconv.Itoa(v)); err != nil {
		klog.Warningf("LOG_LEVEL %q not applied: %v", level, err)
	}
}

// restConfig uses the in-cluster config, or outside a cluster the usual
// kubeconfig (KUBECONFIG or ~/.kube/config) for local runs (ISS-056).
func restConfig(inCluster func() (*rest.Config, error)) (*rest.Config, error) {
	rc, err := inCluster()
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, err
	}
	rc, kerr := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if kerr != nil {
		return nil, fmt.Errorf("not in a cluster and no usable kubeconfig: %w", kerr)
	}
	klog.Infof("not in a cluster: using kubeconfig, server %s", rc.Host)
	return rc, nil
}

// newClients builds the API clients with the configured rate limits.
func newClients(rc *rest.Config, conf config.Config) (Clients, error) {
	rc.QPS, rc.Burst = conf.APIQPS, conf.APIBurst
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return Clients{}, fmt.Errorf("kube client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return Clients{}, fmt.Errorf("dynamic client: %w", err)
	}
	return Clients{Kube: kc, Dynamic: dyn}, nil
}
