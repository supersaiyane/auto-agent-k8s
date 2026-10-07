package kube

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func at(hh, mm int) *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 7, hh, mm, 0, 0, time.UTC)}
}

// PLAN-002 8.5: a window that crosses midnight, including its edges.
func TestQuietHours_WindowCrossingMidnight(t *testing.T) {
	qh := NewQuietHours("22:00-02:00")
	for _, tc := range []struct {
		hh, mm int
		quiet  bool
	}{{21, 59, false}, {22, 0, true}, {23, 30, true}, {0, 0, true}, {1, 59, true}, {2, 0, false}, {12, 0, false}} {
		qh.now = at(tc.hh, tc.mm).now
		if got := qh.IsQuiet(); got != tc.quiet {
			t.Errorf("%02d:%02d quiet=%v, want %v", tc.hh, tc.mm, got, tc.quiet)
		}
	}
	if (&QuietHours{}).IsQuiet() {
		t.Error("an empty QuietHours is never quiet")
	}
}

func TestBlastRadius_ResetsAfterWindow(t *testing.T) {
	c := at(12, 0)
	b := NewBlastRadiusTracker(1, time.Hour)
	b.now, b.lastReset = c.now, c.t
	for i := 0; i < 2; i++ {
		if !b.AllowAction("a") {
			t.Fatalf("action %d in the same namespace must stay allowed", i+1)
		}
	}
	if b.WouldAllow("b") || b.AllowAction("b") {
		t.Fatal("a second namespace exceeds a limit of 1")
	}
	c.advance(time.Hour)
	if !b.WouldAllow("b") {
		t.Fatal("the window reset frees the budget")
	}
	if b.AffectedNamespaces() != 1 {
		t.Fatal("WouldAllow must not record anything (ISS-022)")
	}
	if !b.AllowAction("b") {
		t.Fatal("after the window a new namespace is allowed")
	}
}

func TestLearningMode_PeriodEnds(t *testing.T) {
	c := at(12, 0)
	lm := NewLearningMode(t.TempDir()+"/baselines.json", 24*time.Hour)
	lm.now, lm.startTime = c.now, c.t
	if !lm.IsLearning() {
		t.Fatal("learning during the period")
	}
	c.advance(24 * time.Hour)
	if lm.IsLearning() {
		t.Fatal("learning stops when the period ends")
	}
}
