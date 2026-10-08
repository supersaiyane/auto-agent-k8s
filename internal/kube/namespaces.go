package kube

import (
	"context"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// watchedNamespaces lists the namespaces in the watch scope (ADR-002), sorted.
// An explicit WATCH_NAMESPACES list is returned as is; when every namespace
// is watched, the namespaces are listed from the API server and the system
// ones dropped. A failed list is counted and yields no namespaces, so a
// detector reads nothing rather than reading outside the scope.
func watchedNamespaces(ctx context.Context, deps *Deps) []string {
	pol := deps.Policy()
	if names, all := pol.WatchList(); !all {
		return names
	}
	list, err := deps.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "namespaces", "")
		return nil
	}
	out := make([]string, 0, len(list.Items))
	for i := range list.Items {
		if ns := list.Items[i].Name; pol.Watched(ns) {
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out
}
