package kube

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// writeAged writes a file under dir with its modification time set age ago.
func writeAged(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestCleanOldFiles_RemovesOnlyOldFilesAndEmptiedDirs(t *testing.T) {
	base := t.TempDir()
	old := writeAged(t, base, "ns/pod/old.log", 10*24*time.Hour)
	oldOnly := writeAged(t, base, "gone/deep/old.log", 10*24*time.Hour)
	fresh := writeAged(t, base, "ns/pod/new.log", time.Hour)
	if err := os.MkdirAll(filepath.Join(base, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := cleanOldFiles(base, time.Now().Add(-7*24*time.Hour)); got != 2 {
		t.Fatalf("cleaned %d files, want 2", got)
	}
	if exists(old) || exists(oldOnly) {
		t.Fatal("files older than the cutoff must be removed")
	}
	if !exists(fresh) {
		t.Fatal("a file newer than the cutoff must stay")
	}
	if exists(filepath.Join(base, "gone")) || exists(filepath.Join(base, "empty")) {
		t.Fatal("directories left empty must be removed, deepest first")
	}
	if !exists(filepath.Join(base, "ns", "pod")) || !exists(base) {
		t.Fatal("a directory that still holds a file, and the base itself, must stay")
	}
}

func TestCleanOldFiles_FailuresAreCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "locked")
	writeAged(t, base, "locked/old.log", 10*24*time.Hour)
	if err := os.Chmod(locked, 0o500); err != nil { // files inside cannot be removed
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil { // let t.TempDir clean up
			t.Error(err)
		}
	})
	removeFails := obs.HandlerErrorsTotal.WithLabelValues("retention", "remove")
	before := testutil.ToFloat64(removeFails)
	if got := cleanOldFiles(base, time.Now().Add(-7*24*time.Hour)); got != 0 {
		t.Fatalf("cleaned %d, want 0", got)
	}
	if testutil.ToFloat64(removeFails) <= before {
		t.Fatal("a file that cannot be removed must be counted")
	}

	readFails := obs.HandlerErrorsTotal.WithLabelValues("retention", "read")
	before = testutil.ToFloat64(readFails)
	cleanOldFiles(filepath.Join(base, "missing"), time.Now())
	if testutil.ToFloat64(readFails) <= before {
		t.Fatal("a path that cannot be read must be counted")
	}
}

func TestStartLogRetention_CleansAtStartAndStops(t *testing.T) {
	base := t.TempDir()
	old := writeAged(t, base, "old.log", 3*24*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the first pass still runs, then the loop sees ctx is done
	done := make(chan struct{})
	go func() { StartLogRetention(ctx, config.Storage{Store: "efs", EFSPath: base}, 2); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retention did not stop when its context ended")
	}
	if exists(old) {
		t.Fatal("the first clean must run at start, not an hour later")
	}
}

func TestStartLogRetention_SkipsOtherStores(t *testing.T) {
	base := t.TempDir()
	old := writeAged(t, base, "old.log", 30*24*time.Hour)
	StartLogRetention(context.Background(), config.Storage{Store: "s3", EFSPath: base}, 7) // returns at once
	if !exists(old) {
		t.Fatal("retention must not touch files when logs go to another store")
	}
}

func TestStartLogRetention_DefaultsSevenDays(t *testing.T) {
	base := t.TempDir()
	week := writeAged(t, base, "week.log", 6*24*time.Hour)
	older := writeAged(t, base, "older.log", 8*24*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	StartLogRetention(ctx, config.Storage{EFSPath: base}, 0)
	if !exists(week) || exists(older) {
		t.Fatal("no retention set means seven days")
	}
}
