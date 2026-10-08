package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
)

func TestFSSink_SaveWritesTheRecord(t *testing.T) {
	base := t.TempDir()
	s := newFSSink(base)
	when := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	key := BuildKey("payments", "replicaset/api-7d9f", "api-7d9f-x", "OOMKilled", when)
	if key != "payments/replicaset/api-7d9f/OOMKilled/2026-10-08/api-7d9f-x" {
		t.Fatalf("key %q", key)
	}
	path, err := s.Save(context.Background(), key, &Record{Namespace: "payments", Reason: "OOMKilled", Events: []string{"e"}})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(base, key+".json") {
		t.Fatalf("path %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Record
	if err := json.Unmarshal(raw, &got); err != nil || got.Reason != "OOMKilled" || got.Events[0] != "e" {
		t.Fatalf("record %+v %v", got, err)
	}
}

func TestFSSink_RefusesKeysOutsideBase(t *testing.T) {
	base := t.TempDir()
	s := newFSSink(filepath.Join(base, "logs"))
	for _, key := range []string{"../escape", "a/../../escape", "../../etc/x"} {
		if _, err := s.Save(context.Background(), key, &Record{}); err == nil || !strings.Contains(err.Error(), "leaves") {
			t.Errorf("key %q: %v", key, err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "escape.json")); err == nil {
		t.Fatal("a file was written outside the base directory")
	}
}

func TestFSSink_ReportsWriteFailures(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newFSSink(blocker).Save(context.Background(), "ns/k", &Record{}); err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("a base that is a file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(base, "d", "k.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newFSSink(filepath.Join(base, "d")).Save(context.Background(), "k", &Record{}); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("a target that is a directory: %v", err)
	}
}

func TestNewSink_ChoosesByStore(t *testing.T) {
	efs := t.TempDir()
	cases := []struct {
		name string
		c    config.Storage
		want string
	}{
		{"none", config.Storage{Store: "none"}, "*storage.nopSink"},
		{"efs", config.Storage{Store: "efs", EFSPath: efs}, "*storage.fsSink"},
		{"default", config.Storage{Store: "", EFSPath: efs}, "*storage.fsSink"},
		{"s3 without a bucket mirrors to disk", config.Storage{Store: "s3"}, "*storage.fsSink"},
		{"s3 with a bucket", config.Storage{Store: "s3", S3Bucket: "logs", S3Prefix: "agent"}, "*storage.s3Real"},
	}
	t.Setenv("AWS_REGION", "us-east-1") // LoadDefaultConfig needs no network, only a region
	for _, c := range cases {
		got := NewSink(c.c)
		if typeName(got) != c.want {
			t.Errorf("%s: %s, want %s", c.name, typeName(got), c.want)
		}
	}
	if p, err := NewSink(config.Storage{Store: "none"}).Save(context.Background(), "k", &Record{}); err != nil || p != "(logging disabled)" {
		t.Fatalf("nop sink: %q %v", p, err)
	}
	if s := newFSSink(""); s.base != "/var/log/auto-agent" {
		t.Fatalf("default base %q", s.base)
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *nopSink:
		return "*storage.nopSink"
	case *fsSink:
		return "*storage.fsSink"
	case *s3Real:
		return "*storage.s3Real"
	}
	return "unknown"
}

// The S3 sink puts one JSON object under the prefix and returns its URL; a
// refused put is an error.
func TestS3Sink_SavePutsTheObject(t *testing.T) {
	var gotPath, gotType string
	var gotBody []byte
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	cli := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("id", "secret", "")})
	s := &s3Real{bucket: "logs", prefix: "agent", cli: cli}
	url, err := s.Save(context.Background(), "ns/k", &Record{Reason: "OOMKilled"})
	if err != nil {
		t.Fatal(err)
	}
	if url != "s3://logs/agent/ns/k.json" || gotPath != "/logs/agent/ns/k.json" || gotType != "application/json" || !strings.Contains(string(gotBody), "OOMKilled") {
		t.Fatalf("url %q path %q type %q body %s", url, gotPath, gotType, gotBody)
	}
	status = 403
	if _, err := s.Save(context.Background(), "ns/k", &Record{}); err == nil || !strings.Contains(err.Error(), "put object") {
		t.Fatalf("a refused put: %v", err)
	}
}
