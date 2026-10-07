package metrics

import (
	"context"
	"errors"
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

// PLAN-002 phase 10: every series with its labels; empty is not an error.
func TestQueryVector(t *testing.T) {
	s := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) {
		httpxtest.JSON(w, 200, `{"status":"success","data":{"result":[`+
			`{"metric":{"namespace":"a","pod":"p1"},"value":[1,"0.5"]},`+
			`{"metric":{"namespace":"b","pod":"p2"},"value":[1,"2"]}]}}`)
	})
	defer s.Close()
	p, _ := NewProvider("prometheus", s.URL, s.Client(time.Second))
	v, err := p.QueryVector(context.Background(), "x")
	if err != nil || len(v) != 2 || v[0].Labels["pod"] != "p1" || v[1].Value != 2 {
		t.Fatalf("vector: %+v %v", v, err)
	}
	empty := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) {
		httpxtest.JSON(w, 200, `{"status":"success","data":{"result":[]}}`)
	})
	defer empty.Close()
	p, _ = NewProvider("prometheus", empty.URL, empty.Client(time.Second))
	if v, err := p.QueryVector(context.Background(), "x"); err != nil || len(v) != 0 {
		t.Fatalf("empty: %+v %v", v, err)
	}
	stub, _ := NewProvider("metrics-server", "", nil)
	if _, err := stub.QueryVector(context.Background(), "x"); !errors.Is(err, ErrNoPromQL) {
		t.Fatalf("the stub says no Prometheus: %v", err)
	}
}
