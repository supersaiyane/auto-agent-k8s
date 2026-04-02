package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

type Record struct {
	Timestamp time.Time         `json:"timestamp"`
	Namespace string            `json:"namespace"`
	Workload  string            `json:"workload"`
	Pod       string            `json:"pod"`
	Container string            `json:"container"`
	Node      string            `json:"node"`
	Reason    string            `json:"reason"`
	Message   string            `json:"message"`
	LastLogs  string            `json:"lastLogs"`
	Events    []string          `json:"events"`
	Extras    map[string]string `json:"extras,omitempty"`
}

type Sink interface {
	Save(ctx context.Context, key string, rec *Record) (string, error) // returns URL or path
}

// --- Singleton sink (initialized once) ---

var (
	sinkOnce sync.Once
	sinkInst Sink
)

func GlobalSink() Sink {
	sinkOnce.Do(func() {
		sinkInst = newSinkFromEnv()
	})
	return sinkInst
}

func newSinkFromEnv() Sink {
	switch os.Getenv("LOG_STORE") {
	case "s3":
		if b := os.Getenv("LOG_S3_BUCKET"); b != "" {
			s, err := NewS3Real(context.Background(), b, os.Getenv("LOG_S3_PREFIX"))
			if err != nil {
				klog.Warningf("storage: failed to init S3 sink: %v, falling back to filesystem", err)
			} else {
				klog.Infof("storage: using S3 sink (bucket=%s)", b)
				return s
			}
		}
		return newFSSink("/var/log/auto-agent/s3mirror")
	case "efs":
		return newFSSink(os.Getenv("LOG_EFS_PATH"))
	case "none":
		klog.Infof("storage: logging disabled (LOG_STORE=none)")
		return &nopSink{}
	default:
		return newFSSink(os.Getenv("LOG_EFS_PATH"))
	}
}

// --- Filesystem (EFS or local mount) ---

type fsSink struct{ base string }

func newFSSink(base string) *fsSink {
	if base == "" {
		base = "/var/log/auto-agent"
	}
	klog.Infof("storage: using filesystem sink (path=%s)", base)
	return &fsSink{base: base}
}

func (s *fsSink) Save(_ context.Context, key string, rec *Record) (string, error) {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("storage: marshal: %w", err)
	}
	p := filepath.Join(s.base, key+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("storage: mkdir: %w", err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return "", fmt.Errorf("storage: write: %w", err)
	}
	return p, nil
}

// --- Nop sink (disabled) ---

type nopSink struct{}

func (*nopSink) Save(_ context.Context, _ string, _ *Record) (string, error) {
	return "(logging disabled)", nil
}

// --- Key builder ---

func BuildKey(ns, workload, pod, reason string, t time.Time) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", ns, workload, reason, t.UTC().Format("2006-01-02"), pod)
}
