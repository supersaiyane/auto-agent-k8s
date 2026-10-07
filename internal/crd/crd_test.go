package crd

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func policyObj(ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "autoagent.io/v1alpha1",
		"kind":       "AutoRemediationPolicy",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
	}}
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

func fullSpec(num func(int) interface{}) map[string]interface{} {
	return map[string]interface{}{
		"targetSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "api"}},
		"actions": map[string]interface{}{
			"restartStuckPods":  true,
			"bumpMemoryPercent": num(25),
			"scale": map[string]interface{}{"enabled": true, "minReplicas": num(2), "maxReplicas": num(8),
				"step": num(1), "allowHPAOverride": true},
		},
		"escalation": map[string]interface{}{
			"slackChannel": "#ops", "runbookURL": "https://runbooks.corp.test/api",
			"ticketing": map[string]interface{}{"provider": "jira", "projectOrRepo": "OPS",
				"assignees": []interface{}{"alice", 7}, "labels": []interface{}{"auto"}},
		},
		"safety": map[string]interface{}{"cooldown": "10m", "maxActionsPerHour": num(3), "requireApproval": true},
		"anomalies": []interface{}{
			map[string]interface{}{"name": "lat", "promql": "up", "zscoreThreshold": 3.5, "minSamples": num(20)},
			"not a map",
		},
	}
}

func TestParse_EveryField(t *testing.T) {
	want := Policy{
		Namespace: "ns", Name: "p", RestartStuckPods: true, BumpMemoryPercent: 25,
		Scale:        ScaleConfig{Enabled: true, MinReplicas: 2, MaxReplicas: 8, Step: 1, AllowHPAOverride: true},
		SlackChannel: "#ops", RunbookURL: "https://runbooks.corp.test/api",
		Ticketing: Ticketing{Provider: "jira", ProjectOrRepo: "OPS", Assignees: []string{"alice"}, Labels: []string{"auto"}},
		Cooldown:  "10m", MaxActionsPerHour: 3, RequireApproval: true,
		Anomalies: []AnomalyRule{{Name: "lat", PromQL: "up", ZScoreThreshold: 3.5, MinSamples: 20}},
	}
	// The API server decodes JSON numbers as int64; other paths give float64.
	for name, num := range map[string]func(int) interface{}{
		"int64":   func(n int) interface{} { return int64(n) },
		"float64": func(n int) interface{} { return float64(n) },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parse(policyObj("ns", "p", fullSpec(num)))
			if err != nil {
				t.Fatal(err)
			}
			if !got.Selector.Matches(labels.Set{"app": "api"}) || got.Selector.Matches(labels.Set{"app": "web"}) {
				t.Fatalf("selector: %v", got.Selector)
			}
			got.Selector = nil
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parse:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestParse_NoSpecAndEmptySpec(t *testing.T) {
	if _, err := parse(policyObj("ns", "p", nil)); err == nil {
		t.Fatal("a policy without spec is an error")
	}
	p, err := parse(policyObj("ns", "p", map[string]interface{}{}))
	if err != nil || !p.Selector.Empty() {
		t.Fatalf("an empty spec matches everything: %v %v", p.Selector, err)
	}
}

func TestStore(t *testing.T) {
	s := NewStore()
	api, _ := parse(policyObj("a", "api", fullSpec(func(n int) interface{} { return int64(n) })))
	all, _ := parse(policyObj("a", "all", map[string]interface{}{}))
	s.Update("a", []Policy{api, all})
	s.Update("b", []Policy{all})
	if len(s.List("a")) != 2 || len(s.AllPolicies()) != 3 || len(s.AllNamespaces()) != 2 {
		t.Fatal("store contents")
	}
	if m := s.Match("a", map[string]string{"app": "web"}); len(m) != 1 || m[0].Name != "all" {
		t.Fatalf("match: %+v", m)
	}
	l := s.List("a")
	l[0].Name = "mutated"
	if s.List("a")[0].Name == "mutated" {
		t.Fatal("List returns a copy")
	}
	s.Delete("b")
	if len(s.AllNamespaces()) != 1 {
		t.Fatal("delete")
	}
}

// The controller follows creates and deletes on the API server.
func TestController_SyncsAddAndDelete(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "AutoRemediationPolicyList"})
	store := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartController(ctx, dyn, store)

	res := dyn.Resource(gvr).Namespace("team-a")
	if _, err := res.Create(ctx, policyObj("team-a", "p1", map[string]interface{}{}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Create(ctx, policyObj("team-a", "broken", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return len(store.List("team-a")) == 1 })
	if store.List("team-a")[0].Name != "p1" {
		t.Fatal("an unparseable policy is skipped, the valid one is kept")
	}

	if err := res.Delete(ctx, "p1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return len(store.AllNamespaces()) == 0 })
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
