package redact

import (
	"strings"
	"testing"
)

// Every value below is synthetic. Each case names what must disappear and
// what must survive, because over-redaction destroys the diagnosis.
func TestString(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		gone  []string
		stays []string
	}{
		{"bearer header", "Authorization: Bearer eyJhbGciOi.payload.sig123",
			[]string{"eyJhbGciOi.payload.sig123"}, []string{"Authorization"}},
		{"password assignment", `db connect failed password=hunter2xyz user=app`,
			[]string{"hunter2xyz"}, []string{"db connect failed", "user=app"}},
		{"api key in json", `{"api_key": "abcd1234efgh5678", "region": "eu-west-1"}`,
			[]string{"abcd1234efgh5678"}, []string{"eu-west-1"}},
		{"aws access key id", "using key AKIAIOSFODNN7EXAMPLE for s3",
			[]string{"AKIAIOSFODNN7EXAMPLE"}, []string{"for s3"}},
		{"github token", "token ghp_0123456789abcdefghijABCDEFGHIJ012345 rejected",
			[]string{"ghp_0123456789abcdefghijABCDEFGHIJ012345"}, []string{"rejected"}},
		{"slack token", "xoxb-123456789012-abcdefABCDEF posted",
			[]string{"xoxb-123456789012-abcdefABCDEF"}, []string{"posted"}},
		{"openai style key", "sk-proj-AbCdEf0123456789XyZaBcDeF failed",
			[]string{"sk-proj-AbCdEf0123456789XyZaBcDeF"}, []string{"failed"}},
		{"jwt", "session eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlX3Rlc3Q expired",
			[]string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlX3Rlc3Q"}, []string{"expired"}},
		{"url credentials", "dial postgres://app:s3cr3tpw@db.internal:5432/orders",
			[]string{"s3cr3tpw", "app:"}, []string{"db.internal:5432/orders"}},
		{"email", "user alice.smith@example.com not found",
			[]string{"alice.smith@example.com"}, []string{"not found"}},
		{"ipv4 keeps port", "connection refused 10.42.0.17:6379",
			[]string{"10.42.0.17"}, []string{":6379", "connection refused"}},
		{"private key block", "key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nabc\n-----END RSA PRIVATE KEY-----\nloaded",
			[]string{"MIIEow", "BEGIN RSA PRIVATE KEY"}, []string{"loaded"}},
		{"plain log untouched", "CrashLoopBackOff: exit code 137 (OOMKilled) after 3 restarts",
			nil, []string{"CrashLoopBackOff: exit code 137 (OOMKilled) after 3 restarts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := String(tc.in)
			for _, g := range tc.gone {
				if strings.Contains(out, g) {
					t.Errorf("%q survived redaction: %q", g, out)
				}
			}
			for _, s := range tc.stays {
				if !strings.Contains(out, s) {
					t.Errorf("%q was lost: %q", s, out)
				}
			}
		})
	}
}

func TestAnnotationsAndNestedValues(t *testing.T) {
	m := Map(map[string]string{"description": "password=hunter2xyz", "summary": "ok"})
	if strings.Contains(m["description"], "hunter2xyz") || m["summary"] != "ok" {
		t.Fatalf("Map: %v", m)
	}
	v := Value(map[string]any{"text": "token ghp_0123456789abcdefghijABCDEFGHIJ012345",
		"list": []any{"alice.smith@example.com", 7}})
	b := v.(map[string]any)
	if strings.Contains(b["text"].(string), "ghp_") || strings.Contains(b["list"].([]any)[0].(string), "alice") {
		t.Fatalf("Value: %v", v)
	}
	if b["list"].([]any)[1] != 7 {
		t.Fatalf("Value changed a non-string: %v", v)
	}
}

func TestMapNil(t *testing.T) {
	if Map(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}
