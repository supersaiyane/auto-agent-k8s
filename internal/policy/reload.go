package policy

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// HotReloader watches a ConfigMap and reloads the policy when it changes.
type HotReloader struct {
	mu        sync.RWMutex
	policy    *Policy
	configMap string
	namespace string
}

func NewHotReloader(initial *Policy, namespace, configMap string) *HotReloader {
	return &HotReloader{
		policy:    initial,
		configMap: configMap,
		namespace: namespace,
	}
}

// Get returns the current policy (thread-safe).
func (hr *HotReloader) Get() *Policy {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	return hr.policy
}

// Start begins watching the ConfigMap for changes.
func (hr *HotReloader) Start(ctx context.Context, kc kubernetes.Interface) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		kc, 30*time.Second,
		informers.WithNamespace(hr.namespace),
	)
	inf := factory.Core().V1().ConfigMaps().Informer()

	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			cm, ok := newObj.(*corev1.ConfigMap)
			if !ok || cm.Name != hr.configMap {
				return
			}
			hr.reload(cm.Data)
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	klog.Infof("policy: hot-reload watching ConfigMap %s/%s", hr.namespace, hr.configMap)
}

func (hr *HotReloader) reload(data map[string]string) {
	newPol := applyConfigMapData(hr.Get(), data)
	if err := newPol.Validate(); err != nil {
		klog.Warningf("policy: reload rejected — validation failed: %v", err)
		return
	}
	hr.mu.Lock()
	hr.policy = newPol
	hr.mu.Unlock()
	klog.Infof("policy: reloaded from ConfigMap (mode=%s, namespaces=%v)", newPol.Mode, namespaceKeys(newPol))
}

// applyConfigMapData creates a new policy from configmap key-value pairs,
// using the existing policy as defaults for any missing keys.
func applyConfigMapData(existing *Policy, data map[string]string) *Policy {
	get := func(key, fallback string) string {
		if v, ok := data[key]; ok && v != "" {
			return v
		}
		return fallback
	}

	// Copy existing
	p := *existing

	// Override from configmap
	m := Mode(get("AUTO_MODE", string(p.Mode)))
	switch m {
	case Observe, Suggest, Fix:
		p.Mode = m
	}

	if v := get("SCALE_CPU_THRESHOLD", ""); v != "" {
		p.CPUThreshold = envFloatVal(v, p.CPUThreshold)
	}
	if v := get("MAX_SCALE_STEP", ""); v != "" {
		p.MaxScaleStep = envIntVal(v, p.MaxScaleStep)
	}
	if v := get("MAX_ACTIONS_PER_10M", ""); v != "" {
		p.MaxActionsPer10m = envIntVal(v, p.MaxActionsPer10m)
	}
	if v := get("MAX_REPLICAS", ""); v != "" {
		p.MaxReplicas = int32(envIntVal(v, int(p.MaxReplicas)))
	}
	if v := get("MIN_REPLICAS", ""); v != "" {
		p.MinReplicas = int32(envIntVal(v, int(p.MinReplicas)))
	}
	if v := get("COOLDOWN_UP", ""); v != "" {
		p.CooldownUp = v
	}
	if v := get("COOLDOWN_DOWN", ""); v != "" {
		p.CooldownDown = v
	}
	if v := get("NAMESPACE_ALLOWLIST", ""); v != "" {
		ns := parseNamespaceList(v)
		if len(ns) > 0 {
			p.NamespaceAllow = ns
		}
	}

	return &p
}

func namespaceKeys(p *Policy) []string {
	keys := make([]string, 0, len(p.NamespaceAllow))
	for k := range p.NamespaceAllow {
		keys = append(keys, k)
	}
	return keys
}

// InitialLoad loads the policy from configmap on startup, falling back to env.
func InitialLoad(ctx context.Context, kc kubernetes.Interface, namespace, configMap string) *Policy {
	cm, err := kc.CoreV1().ConfigMaps(namespace).Get(ctx, configMap, metav1.GetOptions{})
	if err != nil {
		klog.V(2).Infof("policy: ConfigMap %s/%s not found, using env vars: %v", namespace, configMap, err)
		return LoadFromEnv()
	}
	base := LoadFromEnv()
	return applyConfigMapData(base, cm.Data)
}
