package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	utilruntime.Must(admissionv1.AddToScheme(scheme)) // a broken scheme is a build bug: fail at start
}

// Validator is a ValidatingWebhook that rejects deployments with known issues.
type Validator struct {
	srv *http.Server
}

// Config for the webhook validator.
type Config struct {
	CertFile         string // TLS cert path
	KeyFile          string // TLS key path
	Port             int
	RequireLimits    bool     // reject if no resource limits
	RequireReadiness bool     // reject if no readiness probe
	BlockedImages    []string // image prefixes to block
}

func NewValidator(cfg Config) *Validator {
	if cfg.Port == 0 {
		cfg.Port = 8443
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/validate", func(w http.ResponseWriter, r *http.Request) {
		handleValidate(w, r, cfg)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			klog.V(4).Infof("webhook: healthz write: %v", err)
		}
	})

	return &Validator{
		srv: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Port),
			Handler:      mux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
		},
	}
}

// Start begins serving TLS. Call in a goroutine.
func (v *Validator) Start(certFile, keyFile string) {
	klog.Infof("webhook: listening on %s", v.srv.Addr)
	if err := v.srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		klog.Errorf("webhook: server error: %v", err)
	}
}

func (v *Validator) Shutdown(ctx context.Context) error {
	return v.srv.Shutdown(ctx)
}

func handleValidate(w http.ResponseWriter, r *http.Request, cfg Config) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if _, _, err := codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil {
		http.Error(w, "decode", http.StatusBadRequest)
		return
	}

	req := review.Request
	if req == nil {
		http.Error(w, "no request", http.StatusBadRequest)
		return
	}

	response := validate(req, cfg)
	review.Response = response
	review.Response.UID = req.UID

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		klog.Warningf("webhook: writing the admission response: %v", err)
	}
}

func validate(req *admissionv1.AdmissionRequest, cfg Config) *admissionv1.AdmissionResponse {
	allowed := &admissionv1.AdmissionResponse{Allowed: true}

	// Only validate Deployments and StatefulSets
	if req.Kind.Kind != "Deployment" && req.Kind.Kind != "StatefulSet" {
		return allowed
	}

	var podSpec corev1.PodSpec
	switch req.Kind.Kind {
	case "Deployment":
		var deploy appsv1.Deployment
		if err := json.Unmarshal(req.Object.Raw, &deploy); err != nil {
			return allowed // don't block on parse error
		}
		podSpec = deploy.Spec.Template.Spec
	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := json.Unmarshal(req.Object.Raw, &sts); err != nil {
			return allowed
		}
		podSpec = sts.Spec.Template.Spec
	default:
		return allowed
	}

	var warnings []string

	for _, c := range podSpec.Containers {
		// Check resource limits
		if cfg.RequireLimits {
			if c.Resources.Limits == nil || len(c.Resources.Limits) == 0 {
				return deny(fmt.Sprintf("container %q has no resource limits: set CPU and memory limits to prevent node exhaustion", c.Name))
			}
			if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
				return deny(fmt.Sprintf("container %q has no memory limit", c.Name))
			}
			if _, ok := c.Resources.Limits[corev1.ResourceCPU]; !ok {
				warnings = append(warnings, fmt.Sprintf("container %q has no CPU limit (recommended)", c.Name))
			}
		}

		// Check readiness probe
		if cfg.RequireReadiness {
			if c.ReadinessProbe == nil {
				return deny(fmt.Sprintf("container %q has no readiness probe: required for safe rollouts", c.Name))
			}
		}

		// Check blocked images
		for _, blocked := range cfg.BlockedImages {
			if strings.HasPrefix(c.Image, blocked) {
				return deny(fmt.Sprintf("container %q uses blocked image prefix %q", c.Name, blocked))
			}
		}

		// Warn on :latest or no tag
		if !pinnedImage(c.Image) {
			warnings = append(warnings, fmt.Sprintf("container %q uses :latest or untagged image %q: pin to a specific tag", c.Name, c.Image))
		}
	}

	if len(warnings) > 0 {
		return &admissionv1.AdmissionResponse{
			Allowed:  true,
			Warnings: warnings,
		}
	}
	return allowed
}

func deny(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: "[auto-agent] " + message,
			Code:    403,
		},
	}
}

// pinnedImage reports an image with a digest, or a tag other than latest.
// The tag is looked for after the last "/" only, so a registry port
// (registry:5000/app) is not mistaken for one (ISS-084).
func pinnedImage(image string) bool {
	if strings.Contains(image, "@") {
		return true
	}
	name := image[strings.LastIndex(image, "/")+1:]
	i := strings.LastIndex(name, ":")
	return i >= 0 && name[i+1:] != "latest"
}
