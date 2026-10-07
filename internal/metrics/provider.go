package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx"
)

type Provider interface {
	AvgDeploymentCPU(ctx context.Context, d *appsv1.Deployment, window string) (float64, error)
	QueryInstant(ctx context.Context, promQL string) (float64, error)
	// QueryVector returns every series of an instant query with its labels
	// (PLAN-002 phase 10). An empty result is not an error.
	QueryVector(ctx context.Context, promQL string) ([]Sample, error)
}

// Sample is one series of an instant vector.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// ErrNoPromQL means no Prometheus is configured. Detectors that need one
// treat it as "nothing to check", not as a failure.
var ErrNoPromQL = errors.New("prometheus not configured")

// NewProvider returns the CPU metrics source: "prometheus" (needs url) or the
// metrics-server stub for anything else.
func NewProvider(kind, url string, hc *http.Client) (Provider, error) {
	t := kind
	switch t {
	case "prometheus":
		base := url
		if base == "" {
			return nil, fmt.Errorf("PROMETHEUS_URL required for prometheus provider")
		}
		return &prom{
			base:   base,
			client: httpx.Client(hc, 10*time.Second),
		}, nil
	default:
		klog.Warningf("metrics: using stub provider (METRICS_PROVIDER=%s)", t)
		return &stub{}, nil
	}
}

// --- Stub provider ---

type stub struct{}

func (*stub) AvgDeploymentCPU(_ context.Context, _ *appsv1.Deployment, _ string) (float64, error) {
	return 0, fmt.Errorf("metrics provider not implemented; configure prometheus")
}

func (*stub) QueryInstant(_ context.Context, _ string) (float64, error) {
	return 0, ErrNoPromQL
}

func (*stub) QueryVector(_ context.Context, _ string) ([]Sample, error) {
	return nil, ErrNoPromQL
}

// --- Prometheus provider ---

type prom struct {
	base   string
	client *http.Client
}

// sanitizeLabelValue removes characters unsafe for PromQL label values.
var unsafePromQL = regexp.MustCompile(`[^a-zA-Z0-9_\-.]`)

func sanitizeLabelValue(s string) string {
	return unsafePromQL.ReplaceAllString(s, "_")
}

func (p *prom) QueryInstant(ctx context.Context, q string) (float64, error) {
	v, err := p.QueryVector(ctx, q)
	if err != nil {
		return 0, err
	}
	if len(v) == 0 {
		return 0, fmt.Errorf("prometheus: no data for query")
	}
	return v[0].Value, nil
}

func (p *prom) QueryVector(ctx context.Context, q string) ([]Sample, error) {
	u := p.base + "/api/v1/query?query=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus: create request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: query: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return nil, fmt.Errorf("prometheus: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: status %d: %s", resp.StatusCode, truncBody(b))
	}

	var out struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]interface{}    `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("prometheus: unmarshal: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus: query status: %s", out.Status)
	}
	samples := make([]Sample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		s, ok := r.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("prometheus: unexpected value type")
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
			return nil, fmt.Errorf("prometheus: parse value %q: %w", s, err)
		}
		samples = append(samples, Sample{Labels: r.Metric, Value: f})
	}
	return samples, nil
}

func (p *prom) AvgDeploymentCPU(ctx context.Context, d *appsv1.Deployment, window string) (float64, error) {
	ns := sanitizeLabelValue(d.Namespace)
	name := sanitizeLabelValue(d.Name)
	if window == "" {
		window = "5m"
	}
	q := fmt.Sprintf(
		`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*",container!="",image!=""}[%s]))`,
		ns, name, window,
	)
	return p.QueryInstant(ctx, q)
}

func truncBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
