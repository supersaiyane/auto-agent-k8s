package crd

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

var gvr = schema.GroupVersionResource{
	Group: "autoagent.io", Version: "v1alpha1", Resource: "autoremediationpolicies",
}

// StartController watches AutoRemediationPolicy CRs and keeps Store in sync.
func StartController(ctx context.Context, dyn dynamic.Interface, store *Store) {
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll, nil)
	inf := f.ForResource(gvr).Informer()

	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { syncAll(store, inf.GetStore()) },
		UpdateFunc: func(_, _ interface{}) { syncAll(store, inf.GetStore()) },
		DeleteFunc: func(obj interface{}) { syncAll(store, inf.GetStore()) },
	})
	go inf.Run(ctx.Done())
	klog.Infof("crd: controller started for %s", gvr.String())
}

func syncAll(store *Store, s cache.Store) {
	nsMap := map[string][]Policy{}
	for _, o := range s.List() {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		ns := u.GetNamespace()
		sp, err := parse(u)
		if err != nil {
			klog.Warningf("crd: failed to parse %s/%s: %v", ns, u.GetName(), err)
			continue
		}
		nsMap[ns] = append(nsMap[ns], sp)
	}

	// Update store: set known namespaces, delete removed ones
	knownNs := store.AllNamespaces()
	for _, ns := range knownNs {
		if _, ok := nsMap[ns]; !ok {
			store.Delete(ns)
		}
	}
	for ns, ps := range nsMap {
		store.Update(ns, ps)
	}
	klog.V(3).Infof("crd: synced %d policies across %d namespaces", countPolicies(nsMap), len(nsMap))
}

func countPolicies(m map[string][]Policy) int {
	n := 0
	for _, ps := range m {
		n += len(ps)
	}
	return n
}

func parse(u *unstructured.Unstructured) (Policy, error) {
	p := Policy{Namespace: u.GetNamespace(), Name: u.GetName()}
	spec, ok := u.Object["spec"].(map[string]interface{})
	if !ok {
		return p, fmt.Errorf("no spec")
	}

	if ts, ok := spec["targetSelector"].(map[string]interface{}); ok {
		sel := labels.Set{}
		if ml, ok := ts["matchLabels"].(map[string]interface{}); ok {
			for k, v := range ml {
				sel[k] = fmt.Sprintf("%v", v)
			}
		}
		p.Selector = labels.SelectorFromSet(sel)
	} else {
		p.Selector = labels.Everything()
	}

	if act, ok := spec["actions"].(map[string]interface{}); ok {
		if v, ok := act["restartStuckPods"].(bool); ok {
			p.RestartStuckPods = v
		}
		if v, ok := act["bumpMemoryPercent"].(int64); ok {
			p.BumpMemoryPercent = int(v)
		}
		// Also try float64 (JSON numbers can decode as float64)
		if v, ok := act["bumpMemoryPercent"].(float64); ok {
			p.BumpMemoryPercent = int(v)
		}
		if sc, ok := act["scale"].(map[string]interface{}); ok {
			p.Scale = parseScaleConfig(sc)
		}
	}

	if esc, ok := spec["escalation"].(map[string]interface{}); ok {
		if v, ok := esc["slackChannel"].(string); ok {
			p.SlackChannel = v
		}
		if v, ok := esc["runbookURL"].(string); ok {
			p.RunbookURL = v
		}
		if t, ok := esc["ticketing"].(map[string]interface{}); ok {
			p.Ticketing = parseTicketing(t)
		}
	}

	if sa, ok := spec["safety"].(map[string]interface{}); ok {
		if v, ok := sa["cooldown"].(string); ok {
			p.Cooldown = v
		}
		if v, ok := sa["maxActionsPerHour"].(int64); ok {
			p.MaxActionsPerHour = int(v)
		}
		if v, ok := sa["maxActionsPerHour"].(float64); ok {
			p.MaxActionsPerHour = int(v)
		}
		if v, ok := sa["requireApproval"].(bool); ok {
			p.RequireApproval = v
		}
	}

	if an, ok := spec["anomalies"].([]interface{}); ok {
		for _, x := range an {
			if m, ok := x.(map[string]interface{}); ok {
				r := AnomalyRule{}
				if v, ok := m["name"].(string); ok {
					r.Name = v
				}
				if v, ok := m["promql"].(string); ok {
					r.PromQL = v
				}
				if v, ok := m["zscoreThreshold"].(float64); ok {
					r.ZScoreThreshold = v
				}
				if v, ok := m["minSamples"].(int64); ok {
					r.MinSamples = int(v)
				}
				if v, ok := m["minSamples"].(float64); ok {
					r.MinSamples = int(v)
				}
				p.Anomalies = append(p.Anomalies, r)
			}
		}
	}
	return p, nil
}

func parseScaleConfig(sc map[string]interface{}) ScaleConfig {
	var c ScaleConfig
	if v, ok := sc["enabled"].(bool); ok {
		c.Enabled = v
	}
	if v, ok := sc["minReplicas"].(int64); ok {
		c.MinReplicas = int32(v)
	}
	if v, ok := sc["minReplicas"].(float64); ok {
		c.MinReplicas = int32(v)
	}
	if v, ok := sc["maxReplicas"].(int64); ok {
		c.MaxReplicas = int32(v)
	}
	if v, ok := sc["maxReplicas"].(float64); ok {
		c.MaxReplicas = int32(v)
	}
	if v, ok := sc["step"].(int64); ok {
		c.Step = int32(v)
	}
	if v, ok := sc["step"].(float64); ok {
		c.Step = int32(v)
	}
	if v, ok := sc["allowHPAOverride"].(bool); ok {
		c.AllowHPAOverride = v
	}
	return c
}

func parseTicketing(t map[string]interface{}) Ticketing {
	var tk Ticketing
	if v, ok := t["provider"].(string); ok {
		tk.Provider = v
	}
	if v, ok := t["projectOrRepo"].(string); ok {
		tk.ProjectOrRepo = v
	}
	if a, ok := t["assignees"].([]interface{}); ok {
		for _, v := range a {
			if s, ok := v.(string); ok {
				tk.Assignees = append(tk.Assignees, s)
			}
		}
	}
	if a, ok := t["labels"].([]interface{}); ok {
		for _, v := range a {
			if s, ok := v.(string); ok {
				tk.Labels = append(tk.Labels, s)
			}
		}
	}
	return tk
}
