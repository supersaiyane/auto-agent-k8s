package storage

import (
    "context"
    "fmt"
    "os"
    "path/filepath"
    "time"
    "encoding/json"
)

type Record struct {
    Timestamp   time.Time           `json:"timestamp"`
    Namespace   string              `json:"namespace"`
    Workload    string              `json:"workload"`
    Pod         string              `json:"pod"`
    Container   string              `json:"container"`
    Node        string              `json:"node"`
    Reason      string              `json:"reason"`
    Message     string              `json:"message"`
    LastLogs    string              `json:"lastLogs"`
    Events      []string            `json:"events"`
    Extras      map[string]string   `json:"extras,omitempty"`
}

type Sink interface {
    Save(ctx context.Context, key string, rec *Record) (string, error) // returns URL or path
}

func NewSinkFromEnv() Sink {
    switch os.Getenv("LOG_STORE") {
    case "s3":
        if b := os.Getenv("LOG_S3_BUCKET"); b != "" {
            if s, err := NewS3Real(context.Background(), b, os.Getenv("LOG_S3_PREFIX")); err == nil {
                return s
            }
        }
        // fallback to fs
        return &fsSink{base: "/var/log/auto-agent/s3mirror"}
    case "efs":
        fallthrough
    default:
        return &fsSink{base: os.Getenv("LOG_EFS_PATH")}
    }
}

// -------- Filesystem (EFS or local mount) --------
type fsSink struct{ base string }

func (s *fsSink) Save(ctx context.Context, key string, rec *Record) (string, error) {
    if s.base == "" { s.base = "/var/log/auto-agent" }
    b, err := json.MarshalIndent(rec, "", "  ")
    if err != nil { return "", err }
    p := filepath.Join(s.base, key + ".json")
    if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { return "", err }
    if err := os.WriteFile(p, b, 0o644); err != nil { return "", err }
    return p, nil
}

func BuildKey(ns, workload, pod, reason string, t time.Time) string {
    return fmt.Sprintf("%s/%s/%s/%s/%s", ns, workload, reason, t.UTC().Format("2006-01-02"), pod)
}
