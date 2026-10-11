package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Mirror rewrites image references to a mirror registry as pods are created
// (images.mirror, ISS-012). Only images whose normalized reference starts
// with an allowlist entry are rewritten; everything else is left alone.
type Mirror struct {
	Prefix    string   // e.g. "registry.internal/": the new reference is Prefix + the normalized one
	AllowList []string // prefixes of normalized references, e.g. "docker.io/library/"
}

// Enabled reports whether there is anything to rewrite.
func (m Mirror) Enabled() bool { return m.Prefix != "" && len(m.AllowList) > 0 }

// normalizeImage writes a reference the way the runtime reads it: a name
// without a registry is on Docker Hub, and a single-segment Hub name is in
// library/.
func normalizeImage(img string) string {
	first, _, found := strings.Cut(img, "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return img // already names a registry
	}
	if !found {
		return "docker.io/library/" + img
	}
	return "docker.io/" + img
}

// rewrite returns the mirrored reference and true when img is allowlisted
// and not already under the mirror prefix.
func (m Mirror) rewrite(img string) (string, bool) {
	if strings.HasPrefix(img, m.Prefix) {
		return img, false
	}
	n := normalizeImage(img)
	for _, a := range m.AllowList {
		if a != "" && strings.HasPrefix(n, a) {
			return m.Prefix + n, true
		}
	}
	return img, false
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// patches lists the JSON patch operations that mirror a pod's images, in
// containers, init containers and ephemeral containers.
func (m Mirror) patches(pod *corev1.Pod) []patchOp {
	var ops []patchOp
	add := func(field string, i int, img string) {
		if to, ok := m.rewrite(img); ok {
			ops = append(ops, patchOp{Op: "replace", Path: fmt.Sprintf("/spec/%s/%d/image", field, i), Value: to})
		}
	}
	for i, c := range pod.Spec.InitContainers {
		add("initContainers", i, c.Image)
	}
	for i, c := range pod.Spec.Containers {
		add("containers", i, c.Image)
	}
	for i, c := range pod.Spec.EphemeralContainers {
		add("ephemeralContainers", i, c.Image)
	}
	return ops
}

// mutate answers a pod admission review. It never refuses a pod: a review
// it cannot read is allowed unchanged, so the mirror cannot block workloads.
func (m Mirror) mutate(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	allowed := &admissionv1.AdmissionResponse{Allowed: true}
	if req.Kind.Kind != "Pod" {
		return allowed
	}
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.Warningf("webhook: mirror: pod in %s does not parse, left unchanged: %v", req.Namespace, err)
		return allowed
	}
	ops := m.patches(&pod)
	if len(ops) == 0 {
		return allowed
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		klog.Warningf("webhook: mirror: encoding the patch: %v", err)
		return allowed
	}
	pt := admissionv1.PatchTypeJSONPatch
	allowed.Patch, allowed.PatchType = patch, &pt
	klog.Infof("webhook: mirror: %d image(s) rewritten in a pod in %s", len(ops), req.Namespace)
	return allowed
}

// handleMutate decodes an AdmissionReview and answers it with mutate.
func handleMutate(w http.ResponseWriter, r *http.Request, m Mirror) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if _, _, err := codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil || review.Request == nil {
		http.Error(w, "decode", http.StatusBadRequest)
		return
	}
	review.Response = m.mutate(review.Request)
	review.Response.UID = review.Request.UID
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		klog.Warningf("webhook: writing the mutation response: %v", err)
	}
}
