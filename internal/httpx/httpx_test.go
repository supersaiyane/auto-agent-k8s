package httpx

import (
	"net/http"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	injected := &http.Client{}
	if Client(injected, time.Second) != injected {
		t.Fatal("an injected client is used as is")
	}
	if c := Client(nil, 3*time.Second); c == nil || c.Timeout != 3*time.Second {
		t.Fatalf("nil gets a client with the caller's timeout: %+v", c)
	}
}
