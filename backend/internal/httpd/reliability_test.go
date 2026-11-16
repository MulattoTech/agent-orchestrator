package httpd

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReliabilityLoopbackControlsAndUnknownMetrics(t *testing.T) {
	writes := 0
	r := chi.NewRouter()
	mountReliability(r, &ReliabilityDeps{
		Snapshot: func(context.Context) (any, error) {
			return map[string]any{"idleWaste": nil, "acceptedRequirements": nil, "inference": map[string]any{"quota": "UNKNOWN"}}, nil
		},
		UpdateWorkers: func(_ context.Context, l domain.SessionAdmissionLimits) error {
			if err := l.Validate(); err != nil {
				return err
			}
			writes++
			return nil
		},
		UpdateInference: func(context.Context, inferenceguard.Controls) error { writes++; return nil },
	})
	request := func(method, path, origin, peer, body string) *httptest.ResponseRecorder {
		q := httptest.NewRequest(method, "http://127.0.0.1:19421"+path, strings.NewReader(body))
		q.RemoteAddr = peer
		if origin != "" {
			q.Header.Set("Origin", origin)
		}
		if body != "" {
			q.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, q)
		return w
	}
	get := request("GET", "/internal/reliability", "", "127.0.0.1:4321", "")
	if get.Code != 200 || !strings.Contains(get.Body.String(), `"idleWaste":null`) {
		t.Fatal(get.Code, get.Body.String())
	}
	for _, tc := range []struct{ origin, peer string }{
		{"https://foreign.example", "127.0.0.1:1"}, {"http://localhost:19421", "127.0.0.1:1"}, {"", "192.0.2.1:1"},
	} {
		w := request("PUT", "/internal/reliability/inference", tc.origin, tc.peer, `{"pause":true}`)
		if w.Code != 403 {
			t.Fatalf("untrusted control allowed: %+v code=%d", tc, w.Code)
		}
	}
	if writes != 0 {
		t.Fatal("untrusted callback ran")
	}
	good := request("PUT", "/internal/reliability/workers", "http://127.0.0.1:19421", "127.0.0.1:1", `{"registeredSlots":40,"maxActiveWorkers":8,"defaultProjectWorkers":4,"queueEnabled":true}`)
	if good.Code != http.StatusNoContent || writes != 1 {
		t.Fatal(good.Code, good.Body.String(), writes)
	}
	bad := request("PUT", "/internal/reliability/inference", "", "127.0.0.1:1", `{"pause":false,"account_key":"foreign"}`)
	if bad.Code != 400 || writes != 1 {
		t.Fatal("control accepted account replacement")
	}
	html := request("GET", "/internal/reliability/dashboard", "", "127.0.0.1:1", "")
	if html.Code != 200 || !strings.Contains(html.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(html.Body.String(), "Unknown") {
		t.Fatal("dashboard boundary or unknown state missing")
	}
}
