package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// Durable approvals (PLAN-004 phase 1, ISS-078, ADR-003). The queue lives
// in the agent's own ConfigMap, so a restart, a crash or a change of leader
// keeps every pending approval. Each change reads the stored queue, applies
// one step and writes it back with the read resourceVersion, so two
// controllers can never overwrite each other's step (constraint 6).

const (
	// ApprovalsConfigMap holds the queue, in the agent's namespace.
	ApprovalsConfigMap = "auto-agent-approvals"
	approvalsKey       = "queue.json"

	// maxStoredBytes keeps the queue well under the 1 MiB object limit.
	maxStoredBytes = 900 << 10
)

var errApprovalStore = errors.New("approval store")

// approvalRecord is one approval as stored. The change is kept as a
// fixSpec, never as a closure, and rebuilt by buildFix when it is applied.
type approvalRecord struct {
	ID      string    `json:"id"`
	Fix     fixSpec   `json:"fix"`
	Summary string    `json:"summary"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	State   string    `json:"state"`
	By      string    `json:"by,omitempty"`
	Result  string    `json:"result,omitempty"`
}

// approvalStore reads and writes the stored queue. version is the
// object's resourceVersion; "" means the queue is not stored yet. It is a
// set of functions filled in by StoreIn, which only controllers call, so
// node agents never reach the store (the RBAC scan follows StoreIn as a
// controller entry point).
type approvalStore struct {
	loadQueue func(ctx context.Context) (recs []approvalRecord, version string, err error)
	saveQueue func(ctx context.Context, recs []approvalRecord, version string) error
	rebuild   func(deps *Deps, s fixSpec) (*mutation, error)
}

// configMapStore keeps the queue in ApprovalsConfigMap in ns.
type configMapStore struct {
	kc kubernetes.Interface
	ns string
}

func (s configMapStore) readApprovals(ctx context.Context) ([]approvalRecord, string, error) {
	cm, err := s.kc.CoreV1().ConfigMaps(s.ns).Get(ctx, ApprovalsConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		countAPIError(err, "configmaps", s.ns)
		return nil, "", fmt.Errorf("%w: read: %w", errApprovalStore, err)
	}
	if cm.ResourceVersion == "" {
		// The API server always sets one; without it a write could not be
		// a compare and swap, so nothing is written.
		return nil, "", fmt.Errorf("%w: %s/%s has no resourceVersion", errApprovalStore, s.ns, ApprovalsConfigMap)
	}
	raw := cm.Data[approvalsKey]
	if raw == "" {
		return nil, cm.ResourceVersion, nil
	}
	var recs []approvalRecord
	if err := json.Unmarshal([]byte(raw), &recs); err != nil {
		return nil, "", fmt.Errorf("%w: %s/%s does not parse: %w", errApprovalStore, s.ns, ApprovalsConfigMap, err)
	}
	return recs, cm.ResourceVersion, nil
}

func (s configMapStore) writeApprovals(ctx context.Context, recs []approvalRecord, version string) error {
	raw, err := json.Marshal(recs)
	if err != nil {
		return fmt.Errorf("%w: encode: %w", errApprovalStore, err)
	}
	if len(raw) > maxStoredBytes {
		return fmt.Errorf("%w: %d bytes: %w", errApprovalStore, len(raw), errApprovalQueueFull)
	}
	err = writeAgentSetting(ApprovalsConfigMap, func() error {
		if version == "" {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: ApprovalsConfigMap, Namespace: s.ns, Labels: map[string]string{
					"app.kubernetes.io/name": "auto-agent", "app.kubernetes.io/managed-by": "auto-agent",
				}},
				Data: map[string]string{approvalsKey: string(raw)},
			}
			_, err := s.kc.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				// Another controller stored it first: retry from its copy.
				return apierrors.NewConflict(corev1.Resource("configmaps"), ApprovalsConfigMap, err)
			}
			return err
		}
		// The resourceVersion in the patch is a precondition: the API server
		// refuses it with a conflict when the stored queue changed since it
		// was read, so this is a compare and swap, never a blind write.
		patch := mergePatch(map[string]any{
			"metadata": map[string]any{"resourceVersion": version},
			"data":     map[string]any{approvalsKey: string(raw)},
		})
		_, err := s.kc.CoreV1().ConfigMaps(s.ns).Patch(ctx, ApprovalsConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
	if err != nil && !apierrors.IsConflict(err) {
		countAPIError(err, "configmaps", s.ns)
	}
	return err
}

// StoreIn makes the queue durable in ApprovalsConfigMap in ns. Without it
// the queue is held in memory only, which tests use.
func (a *Approvals) StoreIn(kc kubernetes.Interface, ns string) *Approvals {
	if a != nil && kc != nil && ns != "" {
		cs := configMapStore{kc: kc, ns: ns}
		a.store = &approvalStore{loadQueue: cs.readApprovals, saveQueue: cs.writeApprovals, rebuild: buildFix}
	}
	return a
}

// toRecord turns an in-memory item into its stored form.
func (p *pendingApproval) toRecord() approvalRecord {
	r := approvalRecord{ID: p.id, Summary: p.summary, Created: p.created, Expires: p.expires, State: p.state, By: p.by, Result: p.result}
	if p.m.Fix != nil {
		r.Fix = *p.m.Fix
	}
	return r
}

// fromRecord rebuilds an item. A record whose change cannot be rebuilt
// keeps its place in the list but can never be applied.
func (st *approvalStore) fromRecord(deps *Deps, r approvalRecord) *pendingApproval {
	p := &pendingApproval{id: r.ID, summary: r.Summary, created: r.Created, expires: r.Expires, state: r.State, by: r.By, result: r.Result}
	m, err := st.rebuild(deps, r.Fix)
	if err != nil {
		fix := r.Fix
		p.m = mutation{Namespace: fix.Namespace, Workload: fix.Name, ActionType: fix.Action, Fix: &fix,
			SuggestMsg: "unreadable stored change: " + err.Error()}
		p.broken = err
		return p
	}
	p.m = *m
	return p
}

// updateQueue runs one step of the queue against the stored copy: it loads the
// queue into memory, runs step and, when step changed it, writes it back.
// A conflict reloads and runs step again. Callers hold a.mu. Without a
// store, step runs against memory only.
func (a *Approvals) updateQueue(deps *Deps, step func() (changed bool, err error)) error {
	if a.store == nil {
		_, err := step()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		version, err := a.reloadLocked(ctx, deps)
		if err != nil {
			return err
		}
		changed, err := step()
		if err != nil || !changed {
			return err
		}
		return a.store.saveQueue(ctx, a.recordsLocked(), version)
	})
}

// reloadLocked replaces the in-memory queue with the stored one. Callers
// hold a.mu.
func (a *Approvals) reloadLocked(ctx context.Context, deps *Deps) (string, error) {
	recs, version, err := a.store.loadQueue(ctx)
	if err != nil {
		return "", err
	}
	items := make(map[string]*pendingApproval, len(recs))
	for _, r := range recs {
		items[r.ID] = a.store.fromRecord(deps, r)
	}
	a.items = items
	return version, nil
}

// recordsLocked lists the queue in its stored form. Callers hold a.mu.
func (a *Approvals) recordsLocked() []approvalRecord {
	out := make([]approvalRecord, 0, len(a.items))
	for _, p := range a.items {
		out = append(out, p.toRecord())
	}
	return out
}
