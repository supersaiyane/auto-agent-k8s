package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

func tlsSecret(t *testing.T, name string, notAfter time.Time) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name + ".example.test"},
		NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}}
}

func ev(name, reason, kind, obj, msg string, age time.Duration) *corev1.Event {
	return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Reason: reason, Message: msg,
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: obj}, LastTimestamp: metav1.NewTime(testNow.Add(-age))}
}

// Phase 12 audit of CheckSecurityIssues.
//
// Claims: TLS certificates expired or expiring within 30 days; pods a
// LimitRange refused.
// Bugs found: (1) expiry used the real clock and repeated every dedup window
// for up to 30 days; now the injected clock and once a day; (2) the
// LimitRange check matched any FailedCreate saying "forbidden", "limit",
// "minimum" or similar, so quota, webhook and RBAC refusals were reported as
// LimitRange violations; now the LimitRange wording only; (3) a failed
// LimitRange list was dropped; (4) an API throttling check read events in
// every namespace for a reason nothing emits; removed. Rung R1.
func TestAudit_SecurityIssues(t *testing.T) {
	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "default"}}
	h := newFindingHarness(t, lr,
		tlsSecret(t, "expired", testNow.Add(-48*time.Hour)),
		tlsSecret(t, "soon", testNow.Add(10*24*time.Hour)),
		tlsSecret(t, "fine", testNow.Add(200*24*time.Hour)),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "garbage", Namespace: "default"}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("nope")}},
		ev("lr", "FailedCreate", "ReplicaSet", "api-1", `pods "api-1-x" is forbidden: minimum cpu usage per Container is 100m, but request is 50m`, time.Minute),
		ev("quota", "FailedCreate", "ReplicaSet", "api-2", `pods "api-2-x" is forbidden: exceeded quota: compute, requested: limits.cpu=2`, time.Minute),
		ev("old", "FailedCreate", "ReplicaSet", "api-3", `minimum memory usage per Pod is 1Gi`, time.Hour))
	h.deps.TLSCertCheck = true
	p := *h.deps.Policy()
	p.FixCeiling = policy.NamespaceSet("default")
	h.deps.Policies = policy.Static(&p)
	CheckSecurityIssues(context.Background(), h.deps)
	CheckSecurityIssues(context.Background(), h.deps)
	if m := h.expect(t, "CertExpired", RungGuided, 1)[0]; !strings.Contains(m, "expired 2 days ago") || !strings.Contains(m, "expired.example.test") {
		t.Fatalf("expired: %s", m)
	}
	if m := h.expect(t, "CertExpiringSoon", RungGuided, 1)[0]; !strings.Contains(m, "expires in 10 days") {
		t.Fatalf("soon: %s", m)
	}
	if m := h.expect(t, "LimitRangeViolation", RungGuided, 1)[0]; !strings.Contains(m, "replicaset/api-1") || strings.Contains(m, "quota") {
		t.Fatalf("limit range: %s", m)
	}
	expectCounted(t, "limitranges", func(d *Deps) { checkLimitRangeViolations(context.Background(), d) })
}

// Phase 12 audit of CheckWebhookBlocking.
//
// Claims: creates and updates refused by an admission webhook.
// Bugs found: it matched "webhook", "admission" or "denied" anywhere, and
// missed an unreachable webhook ("failed calling webhook"), which refuses
// every request under failurePolicy Fail. Now both, with the webhook named.
// Rung R1.
func TestAudit_WebhookBlocking(t *testing.T) {
	h := newFindingHarness(t,
		ev("deny", "FailedCreate", "ReplicaSet", "api-1", `admission webhook "policy.example.test" denied the request: image tag latest is not allowed`, time.Minute),
		ev("down", "FailedCreate", "ReplicaSet", "api-2", `Internal error occurred: failed calling webhook "inject.example.test": connection refused`, time.Minute),
		ev("rbac", "FailedCreate", "ReplicaSet", "api-3", `User "x" cannot create resource "pods": access denied`, time.Minute),
		ev("old", "FailedCreate", "ReplicaSet", "api-4", `admission webhook "w" denied the request`, time.Hour))
	CheckWebhookBlocking(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "WebhookBlocking", RungGuided, 2), "\n")
	for _, want := range []string{"webhook `policy.example.test` denied", "webhook `inject.example.test` could not be reached", "failurePolicy Fail"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q", want)
		}
	}
	expectCounted(t, "events", func(d *Deps) { CheckWebhookBlocking(context.Background(), d) })
}

// Phase 12 audit of CheckRBACDenied.
//
// Claims: workloads or controllers refused by RBAC.
// Bug found: any event saying "forbidden", "RBAC" or "cannot" matched, so
// quota refusals, LimitRange refusals and messages such as "cannot allocate
// memory" were reported as RBAC denials. Now the API server's RBAC wording
// only, with the resource named. Rung R1.
func TestAudit_RBACDenied(t *testing.T) {
	h := newFindingHarness(t,
		ev("rbac", "FailedCreate", "ReplicaSet", "api-1", `pods "api-1-x" is forbidden: User "system:serviceaccount:default:api" cannot create resource "pods" in API group "" in the namespace "default"`, time.Minute),
		ev("quota", "FailedCreate", "ReplicaSet", "api-2", `pods "x" is forbidden: exceeded quota: compute`, time.Minute),
		ev("mem", "Failed", "Pod", "api-3", `failed to start container: cannot allocate memory`, time.Minute))
	CheckRBACDenied(context.Background(), h.deps)
	if m := h.expect(t, "RBACDenied", RungGuided, 1)[0]; !strings.Contains(m, "refused by RBAC on `pods`") || !strings.Contains(m, "replicaset/api-1") {
		t.Fatalf("rbac: %s", m)
	}
	expectCounted(t, "events", func(d *Deps) { CheckRBACDenied(context.Background(), d) })
}

// Phase 12 audit of CheckResourceQuotas.
//
// Claims: quota resources at 90% or more.
// Bugs found: it posted to Slack only, with no event, so the dashboard never
// showed it and it had no rung; a failed list was dropped; it repeated
// every dedup window. Now a finding per resource, once an hour (R1, R3
// with the approval queue).
func TestAudit_ResourceQuotas(t *testing.T) {
	q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "default"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{"limits.cpu": resource.MustParse("10"), "pods": resource.MustParse("20"), "requests.memory": resource.MustParse("10Gi"), "services": resource.MustParse("0")},
			Used: corev1.ResourceList{"limits.cpu": resource.MustParse("10"), "pods": resource.MustParse("19"), "requests.memory": resource.MustParse("1Gi")}}}
	h := newFindingHarness(t, q)
	CheckResourceQuotas(context.Background(), h.deps)
	CheckResourceQuotas(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "QuotaExhaustion", RungGuided, 2), "\n")
	for _, want := range []string{"`limits.cpu` at 100%: 10 of 10 used, exhausted", "`pods` at 95%"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	if strings.Contains(msgs, "requests.memory") {
		t.Error("a resource at 10% is quiet")
	}
	expectCounted(t, "resourcequotas", func(d *Deps) { CheckResourceQuotas(context.Background(), d) })
}
