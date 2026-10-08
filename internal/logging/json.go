// Package logging sets the agent's log format (ISS-032).
package logging

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/go-logr/logr/funcr"
	"k8s.io/klog/v2"
)

// Init sets the log format from LOG_FORMAT. With "json" every klog line,
// from the agent and from client-go, is written to stderr as one JSON object;
// any other value keeps klog's text format.
func Init(format string) { InitTo(format, os.Stderr) }

// InitTo is Init writing to w; it reports whether JSON output was turned on.
func InitTo(format string, w io.Writer) bool {
	if format != "json" {
		return false
	}
	var mu sync.Mutex // one object per line, even from concurrent callers
	klog.SetLogger(funcr.NewJSON(func(obj string) {
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprintln(w, obj); err != nil {
			fmt.Fprintf(os.Stderr, "logging: write failed: %v\n", err)
		}
	}, funcr.Options{LogTimestamp: true, Verbosity: 10})) // klog's -v decides what reaches here
	klog.Info("structured JSON logging enabled")
	return true
}
