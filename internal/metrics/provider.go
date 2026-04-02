package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/klog/v2"
)

type Provider interface {
	AvgDeploymentCPU(ctx context.Context, d *appsv1.Deployment, window string) (float64, error)
	QueryInstant(ctx context.Context, promQL string) (float64, error)
}

func NewProviderFromEnv(ctx context.Context) (Provider, error) {
	t := envOr("METRICS_PROVIDER", "metrics-server")
	switch t {
	case "prometheus":
		base := envOr("PROMETHEUS_URL", "")
		if base == "" {
			return nil, fmt.Errorf("PROMETHEUS_URL required for prometheus provider")
		}
		return &prom{
			base: base,
			client: &http.Client{
				Timeout: 10 * time.Second,
			},
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
	return 0, fmt.Errorf("prometheus not configured")
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
	u := p.base + "/api/v1/query?query=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, fmt.Errorf("prometheus: create request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("prometheus: query: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return 0, fmt.Errorf("prometheus: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus: status %d: %s", resp.StatusCode, truncBody(b))
	}

	var out struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]interface{} `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return 0, fmt.Errorf("prometheus: unmarshal: %w", err)
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("prometheus: query status: %s", out.Status)
	}
	if len(out.Data.Result) == 0 {
		return 0, fmt.Errorf("prometheus: no data for query")
	}
	s, ok := out.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("prometheus: unexpected value type")
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0, fmt.Errorf("prometheus: parse value %q: %w", s, err)
	}
	return f, nil
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
