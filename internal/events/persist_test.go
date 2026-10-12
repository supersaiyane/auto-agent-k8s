package events

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

func TestRecorder_HistorySurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "events.jsonl") // the directory is created
	r := NewRecorder(10)
	r.EnablePersistence(path)
	r.Record(Event{Type: Incident, Reason: "CrashLoopBackOff"})
	r.Record(Event{Type: Scaling, Reason: "ScaledUp"})
	r.Close()

	again := NewRecorder(10)
	again.EnablePersistence(path)
	defer again.Close()
	got := again.Recent(0)
	if len(got) != 2 || got[0].Reason != "ScaledUp" || got[1].Reason != "CrashLoopBackOff" {
		t.Fatalf("history after a restart: %+v", got)
	}
	again.Record(Event{Type: Incident, Reason: "OOMKilled"})
	if id := again.Recent(1)[0].ID; id != 3 {
		t.Fatalf("ids continue after a restart, got %d", id)
	}
}

func TestRecorder_LoadKeepsTheNewestAndCountsBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lines := `{"id":1,"reason":"a"}
not json
{"id":2,"reason":"b"}
{"id":3,"reason":"c"}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := obs.HandlerErrorsTotal.WithLabelValues("events", "read")
	before := testutil.ToFloat64(bad)
	r := NewRecorder(2)
	r.EnablePersistence(path)
	defer r.Close()
	got := r.Recent(0)
	if len(got) != 2 || got[0].Reason != "c" || got[1].Reason != "b" {
		t.Fatalf("only the newest maxLen events are kept: %+v", got)
	}
	if testutil.ToFloat64(bad) != before+1 {
		t.Fatal("unreadable lines must be counted")
	}
}

func TestRecorder_FailuresAreCountedAndCloseIsSafe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Error(err)
		}
	})
	open := obs.HandlerErrorsTotal.WithLabelValues("events", "open")
	before := testutil.ToFloat64(open)
	r := NewRecorder(5)
	r.EnablePersistence(filepath.Join(dir, "events.jsonl"))
	if testutil.ToFloat64(open) != before+1 {
		t.Fatal("a history file that cannot be opened must be counted")
	}
	r.Record(Event{Reason: "kept in memory"})
	if r.Count() != 1 {
		t.Fatal("events are still kept in memory without a file")
	}
	r.Close()
	r.Close() // twice is safe
	r.EnablePersistence("")
	if r.Count() != 1 {
		t.Fatal("an empty path changes nothing")
	}
}
