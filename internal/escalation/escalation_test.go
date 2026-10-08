package escalation

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

func reply(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, status, `{}`) }
}

func TestPagerDutyAndOpsGenie(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{"success", reply(202), false},
		{"error status", reply(500), true},
		{"timeout", httpxtest.Slow(time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(tc.handler)
			defer s.Close()
			hc := s.Client(100 * time.Millisecond)
			inc := Incident{Title: "down", Body: "b", Severity: SevCritical, Namespace: "ns", Workload: "api"}
			if err := NewPagerDuty("rk", hc).Trigger(context.Background(), inc); (err != nil) != tc.wantErr {
				t.Fatalf("pagerduty err=%v", err)
			}
			if err := NewOpsGenie("key", hc).Create(context.Background(), inc); (err != nil) != tc.wantErr {
				t.Fatalf("opsgenie err=%v", err)
			}
			r := s.Requests()
			if len(r) != 2 || r[0].Host != "events.pagerduty.com" || r[1].Host != "api.opsgenie.com" {
				t.Fatalf("requests: %+v", r)
			}
		})
	}
}

func TestChainRoutesBySeverity(t *testing.T) {
	s := httpxtest.New(reply(202))
	defer s.Close()
	empty := NewChain(config.Escalation{}, nil)
	if empty.Configured() {
		t.Fatal("nothing configured")
	}
	empty.Escalate(context.Background(), Incident{Severity: SevCritical})

	c := NewChain(config.Escalation{PagerDutyRoutingKey: "rk", OpsGenieAPIKey: "k"}, s.Client(time.Second))
	if !c.Configured() {
		t.Fatal("configured")
	}
	c.Escalate(context.Background(), Incident{Title: "info only", Severity: SevInfo})
	if n := len(s.Requests()); n != 0 {
		t.Fatalf("info incidents do not page, got %d requests", n)
	}
	c.Escalate(context.Background(), Incident{Title: "warn", Severity: SevWarning})
	if n := len(s.Requests()); n != 2 {
		t.Fatalf("a warning pages PagerDuty and OpsGenie, got %d requests", n)
	}
	for _, sev := range []Severity{SevCritical, SevWarning, SevInfo, Severity("other")} {
		if pdSeverity(sev) == "" || ogPriority(sev) == "" {
			t.Fatalf("severity mapping for %q", sev)
		}
	}
}

// fakeSMTP accepts one message and returns everything the client sent.
func fakeSMTP(t *testing.T) (host, port string, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			out <- ""
			return
		}
		defer conn.Close()
		var seen strings.Builder
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake\r\n")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			seen.WriteString(line)
			switch {
			case inData && line == ".\r\n":
				inData = false
				fmt.Fprint(conn, "250 queued\r\n")
			case inData:
			case strings.HasPrefix(line, "DATA"):
				inData = true
				fmt.Fprint(conn, "354 go\r\n")
			case strings.HasPrefix(line, "QUIT"):
				fmt.Fprint(conn, "221 bye\r\n")
				out <- seen.String()
				return
			default:
				fmt.Fprint(conn, "250 ok\r\n")
			}
		}
		out <- seen.String()
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p, out
}

func TestEveryChannelIsRedacted(t *testing.T) {
	const secret = "hunter2xyz"
	s := httpxtest.New(reply(202))
	defer s.Close()
	host, port, mail := fakeSMTP(t)
	c := NewChain(config.Escalation{PagerDutyRoutingKey: "rk", OpsGenieAPIKey: "k",
		SMTPHost: host, SMTPPort: port, SMTPFrom: "agent@corp.test", EmailTo: "oncall@corp.test"}, s.Client(time.Second))
	c.Escalate(context.Background(), Incident{Title: "db password=" + secret, Body: "dsn password=" + secret,
		Severity: SevCritical, Namespace: "ns", Workload: "api"})

	if n := len(s.Requests()); n != 2 {
		t.Fatalf("critical pages PagerDuty and OpsGenie, got %d requests", n)
	}
	for _, r := range s.Requests() {
		if strings.Contains(r.Body, secret) {
			t.Fatalf("%s received the secret: %s", r.Host, r.Body)
		}
	}
	select {
	case m := <-mail:
		if !strings.Contains(m, "Workload: api") || strings.Contains(m, secret) {
			t.Fatalf("email must arrive redacted: %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no email arrived")
	}
}

func TestEmailFailureIsReturned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close() // nothing listens now
	if err := NewEmail(h, p, "u", "p", "a@corp.test", "b@corp.test").Send(context.Background(), Incident{}); err == nil {
		t.Fatal("an unreachable SMTP server is an error")
	}
	if NewEmail("h", "", "", "", "", "").port != "587" {
		t.Fatal("default SMTP port is 587")
	}
}

// ISS-080: failures come back joined and are counted per channel.
func TestEscalate_ReturnsAndCountsFailures(t *testing.T) {
	s := httpxtest.New(reply(500))
	defer s.Close()
	before := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("escalation", "pagerduty"))
	c := NewChain(config.Escalation{PagerDutyRoutingKey: "rk", OpsGenieAPIKey: "k"}, s.Client(time.Second))
	err := c.Escalate(context.Background(), Incident{Title: "x", Severity: SevCritical})
	if err == nil || !strings.Contains(err.Error(), "pagerduty") || !strings.Contains(err.Error(), "opsgenie") {
		t.Fatalf("want both failures, got %v", err)
	}
	if got := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("escalation", "pagerduty")) - before; got != 1 {
		t.Fatalf("pagerduty failures counted %v, want 1", got)
	}
	var none *Chain
	if none.Configured() || none.Escalate(context.Background(), Incident{}) != nil {
		t.Fatal("a nil chain does nothing")
	}
	none.Send(Incident{})
	none.Wait()
}

// ISS-080: Send never blocks the caller; beyond maxInFlight it drops and counts.
func TestSend_BoundedAndAsync(t *testing.T) {
	release := make(chan struct{})
	s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		<-release
		httpxtest.JSON(w, 202, `{}`)
	})
	defer s.Close()
	c := NewChain(config.Escalation{PagerDutyRoutingKey: "rk"}, s.Client(5*time.Second))
	dropped := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("escalation", "dropped"))
	start := time.Now()
	for i := 0; i < maxInFlight+1; i++ {
		c.Send(Incident{Title: fmt.Sprint(i), Severity: SevCritical})
	}
	if time.Since(start) > time.Second {
		t.Fatal("Send blocked the caller")
	}
	if got := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("escalation", "dropped")) - dropped; got != 1 {
		t.Fatalf("dropped %v, want 1", got)
	}
	close(release)
	c.Wait()
	if n := len(s.Requests()); n != maxInFlight {
		t.Fatalf("sent %d, want %d", n, maxInFlight)
	}
}

// ISS-080: an SMTP server that never answers cannot hold the caller past ctx.
func TestEmail_BoundedByContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			time.Sleep(5 * time.Second) // accepts, never greets
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := NewEmail(h, p, "", "", "a@corp.test", "b@corp.test").Send(ctx, Incident{}); err == nil {
		t.Fatal("a silent server is an error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("email took %s, past its deadline", time.Since(start))
	}
}

// AUTH is never sent to a server that does not offer it.
func TestEmail_AuthNeedsServerSupport(t *testing.T) {
	host, port, _ := fakeSMTP(t)
	err := NewEmail(host, port, "user", "pass", "a@corp.test", "b@corp.test").Send(context.Background(), Incident{})
	if err == nil || !strings.Contains(err.Error(), "AUTH") {
		t.Fatalf("want an AUTH refusal, got %v", err)
	}
}
