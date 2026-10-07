package metrics

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
)

func TestQueryInstant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    float64
		wantErr bool
	}{
		{"success", 200, `{"status":"success","data":{"result":[{"value":[1,"0.75"]}]}}`, 0.75, false},
		{"error status", 500, `boom`, 0, true},
		{"bad json", 200, `{`, 0, true},
		{"query error", 200, `{"status":"error"}`, 0, true},
		{"no data", 200, `{"status":"success","data":{"result":[]}}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, tc.status, tc.body) })
			defer s.Close()
			p, err := NewProvider("prometheus", s.URL, s.Client(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.QueryInstant(context.Background(), "up")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %v err %v", got, err)
			}
		})
	}
}

func TestQueryInstantTimeoutAndCPU(t *testing.T) {
	slow := httpxtest.New(httpxtest.Slow(time.Second))
	defer slow.Close()
	p, _ := NewProvider("prometheus", slow.URL, slow.Client(100*time.Millisecond))
	if _, err := p.QueryInstant(context.Background(), "up"); err == nil {
		t.Fatal("a timeout is an error")
	}

	s := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) {
		httpxtest.JSON(w, 200, `{"status":"success","data":{"result":[{"value":[1,"0.5"]}]}}`)
	})
	defer s.Close()
	p, _ = NewProvider("prometheus", s.URL, s.Client(time.Second))
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: `api"x`, Namespace: "default"}}
	if v, err := p.AvgDeploymentCPU(context.Background(), d, "5m"); err != nil || v != 0.5 {
		t.Fatalf("cpu: %v %v", v, err)
	}
	if q := s.Requests()[0].Path; !strings.Contains(q, "default") || strings.Contains(q, `api"x`) || strings.Contains(q, `api%22x`) {
		t.Fatalf("label values are sanitised in the query: %s", q)
	}
}

func TestNewProviderKinds(t *testing.T) {
	if _, err := NewProvider("prometheus", "", nil); err == nil {
		t.Fatal("prometheus without a URL is an error")
	}
	p, err := NewProvider("metrics-server", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.QueryInstant(context.Background(), "x"); err == nil {
		t.Fatal("the stub cannot answer PromQL")
	}
	if _, err := p.AvgDeploymentCPU(context.Background(), &appsv1.Deployment{}, "5m"); err == nil {
		t.Fatal("the stub has no CPU data")
	}
}
