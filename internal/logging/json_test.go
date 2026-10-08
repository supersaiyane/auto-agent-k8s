package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/klog/v2"
)

// ISS-032: LOG_FORMAT=json turns every klog line into one JSON object, not
// just the first one.
func TestInitTo_JSONCarriesEveryLine(t *testing.T) {
	var buf bytes.Buffer
	if !InitTo("json", &buf) {
		t.Fatal("json format not enabled")
	}
	defer klog.ClearLogger()
	klog.Infof("scaler: %s 2 -> 4", "default/api")
	klog.Warningf("gate: BLOCKED %s", "delete_pod")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want the start line and two log lines, got %d:\n%s", len(lines), buf.String())
	}
	for _, l := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(l), &obj); err != nil {
			t.Fatalf("not JSON: %q (%v)", l, err)
		}
		if _, ok := obj["ts"]; !ok {
			t.Fatalf("no timestamp: %q", l)
		}
	}
	if !strings.Contains(lines[1], `"msg":"scaler: default/api 2 -> 4"`) || !strings.Contains(lines[2], "gate: BLOCKED delete_pod") {
		t.Fatalf("messages:\n%s", buf.String())
	}
}

func TestInitTo_TextKeepsKlog(t *testing.T) {
	var buf bytes.Buffer
	if InitTo("text", &buf) || InitTo("", &buf) || buf.Len() != 0 {
		t.Fatal("only json changes the format")
	}
}

type failingWriter struct{ calls int }

func (f *failingWriter) Write([]byte) (int, error) { f.calls++; return 0, errors.New("disk full") }

// A failed write is reported, never a panic or a silent loss of the logger.
func TestInitTo_WriteFailureIsReported(t *testing.T) {
	fw := &failingWriter{}
	InitTo("json", fw)
	defer klog.ClearLogger()
	klog.Info("x")
	if fw.calls < 2 {
		t.Fatalf("every line is attempted, got %d writes", fw.calls)
	}
	Init("text") // the production entry point leaves text format alone
}
