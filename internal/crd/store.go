package crd

import (
    "sync"
    "k8s.io/apimachinery/pkg/labels"
)

type ScaleConfig struct {
    Enabled        bool
    MinReplicas    int32
    MaxReplicas    int32
    Step           int32
    AllowHPAOverride bool
}

type Ticketing struct {
    Provider      string
    ProjectOrRepo string
    Assignees     []string
    Labels        []string
}

type AnomalyRule struct {
    Name           string
    PromQL         string
    ZScoreThreshold float64
    MinSamples     int
}

type Policy struct {
    Namespace      string
    Name           string
    Selector       labels.Selector
    RestartStuckPods bool
    BumpMemoryPercent int
    Scale          ScaleConfig
    SlackChannel   string
    Ticketing      Ticketing
    RunbookURL     string
    Cooldown       string
    MaxActionsPerHour int
    RequireApproval bool
    Anomalies      []AnomalyRule
}

type Store struct {
    mu sync.RWMutex
    byNS map[string][]Policy
}

func NewStore() *Store { return &Store{byNS: make(map[string][]Policy)} }

func (s *Store) Update(ns string, ps []Policy) {
    s.mu.Lock(); defer s.mu.Unlock()
    s.byNS[ns] = ps
}
func (s *Store) List(ns string) []Policy {
    s.mu.RLock(); defer s.mu.RUnlock()
    return append([]Policy(nil), s.byNS[ns]...)
}
func (s *Store) Match(ns string, lbls map[string]string) []Policy {
    s.mu.RLock(); defer s.mu.RUnlock()
    out := []Policy{}
    for _, p := range s.byNS[ns] {
        if p.Selector.Matches(labels.Set(lbls)) {
            out = append(out, p)
        }
    }
    return out
}
