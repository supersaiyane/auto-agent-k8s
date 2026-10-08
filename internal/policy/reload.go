package policy

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// HotReloader watches a ConfigMap and reloads the policy when it changes.
// With WatchScope it also applies the dashboard's fix scope choice (ADR-002).
type HotReloader struct {
	mu        sync.RWMutex
	policy    *Policy
	configMap string
	namespace string
	scopeMap  string // the dashboard choice ConfigMap; "" when not watched
}

func NewHotReloader(initial *Policy, namespace, configMap string) *HotReloader {
	return &HotReloader{
		policy:    initial,
		configMap: configMap,
		namespace: namespace,
	}
}

// WatchScope makes Start also follow the dashboard's fix scope choice in the
// ConfigMap name. Until that ConfigMap has been read, the fix scope is empty:
// a choice that narrowed the scope is never bypassed during start.
// Call it before Start.
func (hr *HotReloader) WatchScope(name string) {
	hr.update(func(p *Policy) *Policy { return p.WithFixOverride([]string{}) })
	hr.scopeMap = name
}

// update replaces the snapshot with fn's result under one lock, so the
// ConfigMap reload and the scope choice never overwrite each other.
func (hr *HotReloader) update(fn func(*Policy) *Policy) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	hr.policy = fn(hr.policy)
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

	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { hr.applyScope(obj) },
		UpdateFunc: func(oldObj, newObj interface{}) {
			cm, ok := newObj.(*corev1.ConfigMap)
			if ok && cm.Name == hr.configMap {
				hr.reload(cm.Data)
			}
			hr.applyScope(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if cm, ok := obj.(*corev1.ConfigMap); ok && hr.scopeMap != "" && cm.Name == hr.scopeMap {
				hr.setScope(nil)
			}
		},
	}); err != nil {
		klog.Errorf("policy: cannot watch ConfigMaps: %v", err)
		return
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		klog.Warning("policy: ConfigMap cache never synced; the fix scope stays empty")
		return
	}
	if hr.scopeMap != "" {
		if _, exists, err := inf.GetStore().GetByKey(hr.namespace + "/" + hr.scopeMap); err == nil && !exists {
			hr.setScope(nil) // no dashboard choice: FIX_NAMESPACES applies
		}
	}
	klog.Infof("policy: hot-reload watching ConfigMap %s/%s", hr.namespace, hr.configMap)
}

// applyScope applies the dashboard choice when obj is the scope ConfigMap.
func (hr *HotReloader) applyScope(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || hr.scopeMap == "" || cm.Name != hr.scopeMap {
		return
	}
	hr.setScope(ScopeChoice(cm.Data))
}

func (hr *HotReloader) setScope(names []string) {
	hr.update(func(p *Policy) *Policy { return p.WithFixOverride(names) })
	klog.Infof("policy: fix scope is now %v (dashboard choice: %v)", hr.Get().FixScope(), names != nil)
}

func (hr *HotReloader) reload(data map[string]string) {
	var newPol *Policy
	hr.update(func(old *Policy) *Policy {
		newPol = applyConfigMapData(old, data)
		if err := newPol.Validate(); err != nil {
			klog.Warningf("policy: reload rejected, validation failed: %v", err)
			newPol = nil
			return old
		}
		return newPol
	})
	if newPol == nil {
		return
	}
	watch, all := newPol.WatchList()
	klog.Infof("policy: reloaded from ConfigMap (mode=%s, watch all=%v %v, fix=%v)", newPol.Mode, all, watch, newPol.FixScope())
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
	case Observe, Suggest, Fix, DryRun:
		p.Mode = m
	default:
		klog.Warningf("policy: reload ignored invalid AUTO_MODE %q, keeping %s", m, p.Mode)
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
	// The ceiling (FIX_CEILING, FIX_ANYWHERE) mirrors the chart's RBAC and
	// changes only with a Helm upgrade, so it is not reloaded here.
	// The ceiling (FIX_CEILING, FIX_ANYWHERE) mirrors the chart's RBAC and
	// changes only with a Helm upgrade, so it is not reloaded here.
	if v, ok := scopeValue(data, "WATCH_NAMESPACES"); ok {
		p.WatchAll = isAll(v)
		p.WatchNamespaces = nil
		if !p.WatchAll {
			p.WatchNamespaces = parseNamespaceList(v)
		}
	}
	if v, ok := scopeValue(data, "FIX_NAMESPACES"); ok {
		p.FixNamespaces = parseNamespaceList(v)
	}

	return &p
}

// scopeValue reads a scope key; a present key wins even when empty, so the
// list can be cleared. NAMESPACE_ALLOWLIST is the deprecated fallback.
func scopeValue(data map[string]string, key string) (string, bool) {
	if v, ok := data[key]; ok {
		return v, true
	}
	if v := data["NAMESPACE_ALLOWLIST"]; v != "" {
		return v, true
	}
	return "", false
}
