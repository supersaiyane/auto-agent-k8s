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

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
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
	if err := NewEmail(h, p, "u", "p", "a@corp.test", "b@corp.test").Send(Incident{}); err == nil {
		t.Fatal("an unreachable SMTP server is an error")
	}
	if NewEmail("h", "", "", "", "", "").port != "587" {
		t.Fatal("default SMTP port is 587")
	}
}
