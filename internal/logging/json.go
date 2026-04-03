package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"k8s.io/klog/v2"
)

// JSONLogEntry represents a structured log entry.
type JSONLogEntry struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"`
	Message   string `json:"msg"`
	Component string `json:"component,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Workload  string `json:"workload,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Logger provides structured JSON logging.
type Logger struct {
	encoder *json.Encoder
	enabled bool
}

// NewLogger creates a JSON logger if LOG_FORMAT=json, otherwise returns a nop logger.
func NewLogger() *Logger {
	format := os.Getenv("LOG_FORMAT")
	if format != "json" {
		return &Logger{enabled: false}
	}
	klog.Infof("logging: JSON structured output enabled")
	return &Logger{
		encoder: json.NewEncoder(os.Stdout),
		enabled: true,
	}
}

func (l *Logger) Enabled() bool { return l.enabled }

func (l *Logger) Info(component, msg string, fields ...string) {
	if !l.enabled {
		return
	}
	l.log("info", component, msg, fields...)
}

func (l *Logger) Warn(component, msg string, fields ...string) {
	if !l.enabled {
		return
	}
	l.log("warn", component, msg, fields...)
}

func (l *Logger) Error(component, msg, errStr string, fields ...string) {
	if !l.enabled {
		return
	}
	entry := JSONLogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     "error",
		Message:   msg,
		Component: component,
		Error:     errStr,
	}
	applyFields(&entry, fields)
	l.encoder.Encode(entry)
}

func (l *Logger) log(level, component, msg string, fields ...string) {
	entry := JSONLogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     level,
		Message:   msg,
		Component: component,
	}
	applyFields(&entry, fields)
	l.encoder.Encode(entry)
}

func applyFields(e *JSONLogEntry, fields []string) {
	for i := 0; i+1 < len(fields); i += 2 {
		switch fields[i] {
		case "namespace":
			e.Namespace = fields[i+1]
		case "workload":
			e.Workload = fields[i+1]
		case "error":
			e.Error = fields[i+1]
		}
	}
}

// FormatIncident returns a structured JSON string for an incident.
func FormatIncident(reason, namespace, workload, message, severity string) string {
	entry := map[string]string{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"level":     severity,
		"component": "handler",
		"reason":    reason,
		"namespace": namespace,
		"workload":  workload,
		"msg":       message,
	}
	b, _ := json.Marshal(entry)
	return string(b)
}

// Init configures klog output format. Call early in main.
func Init() {
	format := os.Getenv("LOG_FORMAT")
	if format == "json" {
		fmt.Fprintln(os.Stderr, `{"ts":"`+time.Now().UTC().Format(time.RFC3339)+`","level":"info","msg":"structured JSON logging enabled"}`)
	}
}
