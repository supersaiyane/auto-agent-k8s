package metrics

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
    appsv1 "k8s.io/api/apps/v1"
)

type Provider interface {
    AvgDeploymentCPU(ctx context.Context, d *appsv1.Deployment, window string) (float64, error)
    QueryInstant(ctx context.Context, promQL string) (float64, error)
}

func NewProviderFromEnv(ctx context.Context) (Provider, error) {
    t := getenv("METRICS_PROVIDER","metrics-server")
    switch t {
    case "prometheus":
        base := getenv("PROMETHEUS_URL","")
        if base == "" { return nil, fmt.Errorf("PROMETHEUS_URL required for prometheus provider") }
        return &prom{base: base}, nil
    default:
        return &stub{}, nil
    }
}

type stub struct{}
func (*stub) AvgDeploymentCPU(ctx context.Context, d *appsv1.Deployment, window string) (float64, error) {
    return 0, fmt.Errorf("metrics provider not implemented; configure prometheus")
}
func (*stub) QueryInstant(ctx context.Context, q string) (float64, error) {
    return 0, fmt.Errorf("prometheus not configured")
}

type prom struct{ base string }

func (p *prom) QueryInstant(ctx context.Context, q string) (float64, error) {
    u := p.base + "/api/v1/query?query=" + url.QueryEscape(q)
    req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return 0, err }
    defer resp.Body.Close()
    b, _ := io.ReadAll(resp.Body)
    var out struct {
        Status string `json:"status"`
        Data struct {
            ResultType string `json:"resultType"`
            Result []struct{ Value [2]interface{} `json:"value"` } `json:"result"`
        } `json:"data"`
    }
    if err := json.Unmarshal(b, &out); err != nil { return 0, err }
    if len(out.Data.Result)==0 { return 0, fmt.Errorf("no data") }
    s, _ := out.Data.Result[0].Value[1].(string)
    var f float64
    fmt.Sscanf(s, "%f", &f)
    return f, nil
}

func (p *prom) AvgDeploymentCPU(ctx context.Context, d *appsv1.Deployment, window string) (float64, error) {
    ns := d.Namespace
    name := d.Name
    // Approximate: avg CPU usage of pods belonging to deployment over window
    q := fmt.Sprintf(`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*",container!="",image!=""}[%s]))`, ns, name, window)
    return p.QueryInstant(ctx, q)
}

func getenv(k, d string) string { if v:=os.Getenv(k); v!="" { return v }; return d }
