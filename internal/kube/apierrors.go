package kube

import "github.com/supersaiyane/auto-agent-k8s/internal/obs"

// countAPIError records a failed API read instead of dropping it (ISS-009).
func countAPIError(err error, resource, ns string) { obs.CountAPIError(err, resource, ns) }
