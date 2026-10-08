package kube

import (
	"context"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Start watches ConfigMaps in the watch scope, and Secrets in the fix
// ceiling when reload.secrets is on, then ticks every second until ctx
// ends. Reads follow the scope (constraint 4): with every namespace watched,
// one informer excludes the system namespaces by field selector; with a
// list, one informer per namespace. The scope at start applies; widening it
// needs a restart for reloads.
func (r *Reloader) WatchConfig(ctx context.Context) {
	pol := r.deps.Policy()
	watchNS, watchAll := pol.WatchList()
	r.reloadInformers(ctx, watchNS, watchAll, func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
		inf := f.Core().V1().ConfigMaps().Informer()
		_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: func(old, cur any) {
			o, ok1 := old.(*corev1.ConfigMap)
			c, ok2 := cur.(*corev1.ConfigMap)
			if ok1 && ok2 {
				r.configMapChanged(o, c)
			}
		}})
		if err != nil {
			klog.Errorf("reload: cannot watch ConfigMaps: %v", err)
		}
		return inf
	})
	if r.cfg.Secrets {
		ceiling, anywhere := secretScope(pol.FixScope(), pol.FixAnywhere, pol.FixCeiling, pol.Watched)
		r.reloadInformers(ctx, ceiling, anywhere, func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			inf := f.Core().V1().Secrets().Informer()
			_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: func(old, cur any) {
				o, ok1 := old.(*corev1.Secret)
				c, ok2 := cur.(*corev1.Secret)
				if ok1 && ok2 {
					r.secretChanged(o, c)
				}
			}})
			if err != nil {
				klog.Errorf("reload: cannot watch Secrets: %v", err)
			}
			return inf
		})
	}
	klog.Infof("reload: watching ConfigMaps (secrets=%v, reloadOn=%s, debounce=%s)", r.cfg.Secrets, r.cfg.On, r.cfg.Debounce)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.reloadTick(ctx)
			}
		}
	}()
}

// informers starts one informer per namespace in names, or one for every
// namespace except the system ones when all is true.
func (r *Reloader) reloadInformers(ctx context.Context, names []string, all bool, build func(informers.SharedInformerFactory) cache.SharedIndexInformer) {
	var factories []informers.SharedInformerFactory
	if all {
		sel := systemExclusion(r.deps.Policy().System)
		factories = append(factories, informers.NewSharedInformerFactoryWithOptions(r.deps.Client, 10*time.Minute,
			informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.FieldSelector = sel })))
	}
	for _, ns := range names {
		factories = append(factories, informers.NewSharedInformerFactoryWithOptions(r.deps.Client, 10*time.Minute, informers.WithNamespace(ns)))
	}
	for _, f := range factories {
		build(f)
		f.Start(ctx.Done())
	}
}

// systemExclusion is a field selector that leaves out the system namespaces.
func systemExclusion(system map[string]struct{}) string {
	names := make([]string, 0, len(system))
	for ns := range system {
		names = append(names, "metadata.namespace!="+ns)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// secretScope is where Secrets are watched: the fix ceiling (the only
// namespaces where a reload can act), or every non-system namespace with
// fix-anywhere. ceiling is pol.FixCeiling; fixScope covers fix-anywhere
// choices outside it.
func secretScope(fixScope []string, anywhere bool, ceiling map[string]struct{}, watched func(string) bool) ([]string, bool) {
	if anywhere {
		return nil, true
	}
	set := map[string]bool{}
	for ns := range ceiling {
		if watched(ns) {
			set[ns] = true
		}
	}
	for _, ns := range fixScope {
		set[ns] = true
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out, false
}
