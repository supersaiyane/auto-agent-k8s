package kube

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// retentionEvery is how often old log files are removed.
const retentionEvery = time.Hour

// StartLogRetention removes log files older than days from the filesystem
// sink, once at start and then every retentionEvery, until ctx ends. It
// runs only when LOG_STORE is efs or unset (the filesystem fallback).
func StartLogRetention(ctx context.Context, store config.Storage, days int) {
	if store.Store != "efs" && store.Store != "" {
		klog.V(3).Infof("retention: skipping (LOG_STORE=%s, only runs for efs/filesystem)", store.Store)
		return
	}
	basePath := store.EFSPath
	if basePath == "" {
		basePath = config.DefaultLogDir
	}
	if days <= 0 {
		days = 7
	}
	maxAge := time.Duration(days) * 24 * time.Hour
	klog.Infof("retention: started (path=%s, retention=%d days)", basePath, days)

	ticker := time.NewTicker(retentionEvery)
	defer ticker.Stop()
	for {
		if cleaned := cleanOldFiles(basePath, time.Now().Add(-maxAge)); cleaned > 0 {
			klog.Infof("retention: cleaned %d files older than %d days", cleaned, days)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// cleanOldFiles removes regular files under basePath last modified before
// cutoff, then the directories that leaves empty, and returns how many
// files it removed. A path it cannot read or remove is logged and counted
// (ISS-093), never skipped silently.
func cleanOldFiles(basePath string, cutoff time.Time) int {
	cleaned := 0
	walkErr := filepath.WalkDir(basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			retentionFailed("read", path, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			retentionFailed("read", path, err)
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			retentionFailed("remove", path, err)
			return nil
		}
		cleaned++
		return nil
	})
	if walkErr != nil {
		retentionFailed("read", basePath, walkErr)
	}
	removeEmptyDirs(basePath)
	return cleaned
}

// removeEmptyDirs removes the empty directories under basePath, deepest
// first, keeping basePath itself.
func removeEmptyDirs(basePath string) {
	var dirs []string
	walkErr := filepath.WalkDir(basePath, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != basePath {
			dirs = append(dirs, path)
		}
		return nil // read errors were counted by cleanOldFiles
	})
	if walkErr != nil {
		retentionFailed("read", basePath, walkErr)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := os.ReadDir(dirs[i])
		if err != nil {
			retentionFailed("read", dirs[i], err)
			continue
		}
		if len(entries) > 0 {
			continue
		}
		if err := os.Remove(dirs[i]); err != nil {
			retentionFailed("remove", dirs[i], err)
		}
	}
}

func retentionFailed(op, path string, err error) {
	klog.Warningf("retention: cannot %s %s: %v", op, path, err)
	obs.HandlerErrorsTotal.WithLabelValues("retention", op).Inc()
}
