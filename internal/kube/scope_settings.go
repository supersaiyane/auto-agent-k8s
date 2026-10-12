package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// ErrOutsideCeiling is returned when a fix scope choice names a namespace
// the chart granted no write permissions in (ADR-002).
var ErrOutsideCeiling = errors.New("outside the fix ceiling")

// SaveFixScope stores the dashboard's fix scope choice in the agent's own
// ConfigMap (policy.ScopeConfigMap in ns); names nil clears the choice and
// returns to FIX_NAMESPACES. Every hot reloader, on every controller and node
// agent, applies it within seconds. Every attempt is an audit event.
//
// This is the gate's second entry point (CLAUDE.md constraint 1): it writes
// only that one ConfigMap, never a workload, and refuses any namespace
// outside the ceiling, so the dashboard can never widen what RBAC allows.
func SaveFixScope(ctx context.Context, deps *Deps, ns string, names []string, from string) error {
	pol := deps.Policy()
	after := "Helm default (" + strings.Join(pol.WithFixOverride(nil).FixScope(), ",") + ")"
	if names != nil {
		after = "[" + strings.Join(names, ",") + "]"
	}
	detail := fmt.Sprintf("fix scope [%s] -> %s, from %s", strings.Join(pol.FixScope(), ","), after, from)
	for _, n := range names {
		if !pol.InCeiling(n) {
			err := fmt.Errorf("%s: %w", n, ErrOutsideCeiling)
			auditAction(deps, "set_fix_scope", ns, policy.ScopeConfigMap, "", "FixScopeChanged", "blocked", detail+": "+err.Error())
			return err
		}
	}
	err := writeScopeConfigMap(ctx, deps, ns, names)
	if err != nil {
		countAPIError(err, "configmaps", ns)
		auditAction(deps, "set_fix_scope", ns, policy.ScopeConfigMap, "", "FixScopeChanged", "failed", detail+": "+err.Error())
		return err
	}
	auditAction(deps, "set_fix_scope", ns, policy.ScopeConfigMap, "", "FixScopeChanged", "success", detail)
	return nil
}

// errNotAgentSetting refuses a write to anything but the agent's own
// ConfigMaps.
var errNotAgentSetting = errors.New("not one of the agent's own ConfigMaps")

// agentSettings are the only objects writeAgentSetting may write (ADR-002,
// ADR-003): the fix scope choice and the approval queue.
var agentSettings = map[string]bool{policy.ScopeConfigMap: true, ApprovalsConfigMap: true}

// writeAgentSetting runs a write to one of the agent's own ConfigMaps,
// named by name. It exists so the mutation guard can tell these writes from
// remediation, which must go through applyMutation, and it refuses any
// other name.
func writeAgentSetting(name string, write func() error) error {
	if !agentSettings[name] {
		return fmt.Errorf("%s: %w", name, errNotAgentSetting)
	}
	return write()
}

// writeScopeConfigMap merge-patches the choice into the ConfigMap, creating
// it on first use. A merge patch only touches the one key (constraint 6).
func writeScopeConfigMap(ctx context.Context, deps *Deps, ns string, names []string) error {
	var value any // nil deletes the key: no choice
	if names != nil {
		value = strings.Join(names, ",")
	}
	patch := mergePatch(map[string]any{"data": map[string]any{policy.ScopeKey: value}})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: policy.ScopeConfigMap, Namespace: ns, Labels: map[string]string{
			"app.kubernetes.io/name": "auto-agent", "app.kubernetes.io/managed-by": "auto-agent",
		}},
		Data: map[string]string{policy.ScopeKey: strings.Join(names, ",")},
	}
	return writeAgentSetting(policy.ScopeConfigMap, func() error {
		_, err := deps.Client.CoreV1().ConfigMaps(ns).Patch(ctx, policy.ScopeConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
		if !apierrors.IsNotFound(err) {
			return err
		}
		if names == nil {
			return nil // nothing stored, so nothing to clear
		}
		_, err = deps.Client.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		return err
	})
}
