package kube

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"k8s.io/klog/v2"
)

// StartLogRetention periodically cleans up old log files from the filesystem sink.
// Only applicable when LOG_STORE=efs or filesystem fallback.
func StartLogRetention(ctx context.Context) {
	storeType := os.Getenv("LOG_STORE")
	if storeType != "efs" && storeType != "" {
		klog.V(3).Infof("retention: skipping (LOG_STORE=%s, only runs for efs/filesystem)", storeType)
		return
	}

	basePath := os.Getenv("LOG_EFS_PATH")
	if basePath == "" {
		basePath = "/var/log/auto-agent"
	}

	days := 7
	if v := os.Getenv("LOG_RETENTION_DAYS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d > 0 {
			days = d
		}
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	klog.Infof("retention: started (path=%s, retention=%d days)", basePath, days)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleaned := cleanOldFiles(basePath, time.Duration(days)*24*time.Hour)
			if cleaned > 0 {
				klog.Infof("retention: cleaned %d files older than %d days", cleaned, days)
			}
		}
	}
}

func cleanOldFiles(basePath string, maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	cleaned := 0

	filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		if info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				cleaned++
			}
		}
		return nil
	})

	// Clean empty directories
	filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() || path == basePath {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err == nil && len(entries) == 0 {
			os.Remove(path)
		}
		return nil
	})

	return cleaned
}
