package crd

import (
    "context"
    "fmt"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/labels"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/client-go/dynamic"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/dynamic/dynamicinformer"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var gvr = schema.GroupVersionResource{
    Group: "autoagent.io", Version: "v1alpha1", Resource: "autoremidiationpolicies",
}

// StartController watches ARP CRs and keeps Store in sync.
func StartController(ctx context.Context, dyn dynamic.Interface, store *Store) {
    f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll, nil)
    inf := f.ForResource(gvr).Informer()

    inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) { syncAll(store, inf.GetStore()) },
        UpdateFunc: func(oldObj, newObj interface{}) { syncAll(store, inf.GetStore()) },
        DeleteFunc: func(obj interface{}) { syncAll(store, inf.GetStore()) },
    })
    go inf.Run(ctx.Done())
}

func syncAll(store *Store, s cache.Store) {
    // Reindex by namespace
    nsMap := map[string][]Policy{}
    for _, o := range s.List() {
        u := o.(*unstructured.Unstructured)
        ns := u.GetNamespace()
        sp, err := parse(u)
        if err != nil { continue }
        nsMap[ns] = append(nsMap[ns], sp)
    }
    for ns, ps := range nsMap {
        store.Update(ns, ps)
    }
}

func parse(u *unstructured.Unstructured) (Policy, error) {
    p := Policy{ Namespace: u.GetNamespace(), Name: u.GetName() }
    spec, ok := u.Object["spec"].(map[string]interface{})
    if !ok { return p, fmt.Errorf("no spec") }

    if ts, ok := spec["targetSelector"].(map[string]interface{}); ok {
        sel := labels.Set{}
        if ml, ok := ts["matchLabels"].(map[string]interface{}); ok {
            for k, v := range ml { sel[k] = fmt.Sprintf("%v", v) }
        }
        p.Selector = labels.SelectorFromSet(sel)
    } else {
        p.Selector = labels.Everything()
    }

    if act, ok := spec["actions"].(map[string]interface{}); ok {
        if v, ok := act["restartStuckPods"].(bool); ok { p.RestartStuckPods = v }
        if v, ok := act["bumpMemoryPercent"].(int64); ok { p.BumpMemoryPercent = int(v) }
        if sc, ok := act["scale"].(map[string]interface{}); ok {
            if v, ok := sc["enabled"].(bool); ok { p.Scale.Enabled = v }
            if v, ok := sc["minReplicas"].(int64); ok { p.Scale.MinReplicas = int32(v) }
            if v, ok := sc["maxReplicas"].(int64); ok { p.Scale.MaxReplicas = int32(v) }
            if v, ok := sc["step"].(int64); ok { p.Scale.Step = int32(v) }
            if v, ok := sc["allowHPAOverride"].(bool); ok { p.Scale.AllowHPAOverride = v }
        }
    }
    if esc, ok := spec["escalation"].(map[string]interface{}); ok {
        if v, ok := esc["slackChannel"].(string); ok { p.SlackChannel = v }
        if t, ok := esc["ticketing"].(map[string]interface{}); ok {
            if v, ok := t["provider"].(string); ok { p.Ticketing.Provider = v }
            if v, ok := t["projectOrRepo"].(string); ok { p.Ticketing.ProjectOrRepo = v }
        }
    }
    if sa, ok := spec["safety"].(map[string]interface{}); ok {
        if v, ok := sa["cooldown"].(string); ok { p.Cooldown = v }
        if v, ok := sa["maxActionsPerHour"].(int64); ok { p.MaxActionsPerHour = int(v) }
        if v, ok := sa["requireApproval"].(bool); ok { p.RequireApproval = v }
    }
    if an, ok := spec["anomalies"].([]interface{}); ok {
        for _, x := range an {
            if m, ok := x.(map[string]interface{}); ok {
                r := AnomalyRule{}
                if v, ok := m["name"].(string); ok { r.Name = v }
                if v, ok := m["promql"].(string); ok { r.PromQL = v }
                if v, ok := m["zscoreThreshold"].(float64); ok { r.ZScoreThreshold = v }
                if v, ok := m["minSamples"].(int64); ok { r.MinSamples = int(v) }
                p.Anomalies = append(p.Anomalies, r)
            }
        }
    }
    return p, nil
}
