package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFixtureCompositionHasNoMissingDependency(t *testing.T) {
	e := &fixtureEffectRecorder{}
	deps := fixtureBootDependencies(t.TempDir(), "synthetic-run", "crash", e)
	if err := deps.validate(); err != nil {
		t.Fatal(err)
	}
	if len(e.entries) != 0 {
		t.Fatal("constructing dependency set caused an effect")
	}
}
func TestFixtureHTTPMiddlewareDeniesUnconfiguredEffectsBeforeController(t *testing.T) {
	e := &fixtureEffectRecorder{}
	deps := fixtureBootDependencies(t.TempDir(), "synthetic-run", "crash", e)
	calls := 0
	handler := deps.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	for _, route := range []string{"/api/v1/agents/codex/auth/login", "/api/v1/import", "/api/v1/sessions/fixture-worker-01/conversation/compact", "/internal/reliability/allowance/verify"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", route, nil))
		if response.Code != 403 {
			t.Fatalf("route %s status=%d", route, response.Code)
		}
	}
	if calls != 0 || len(e.entries) != 4 {
		t.Fatalf("controller calls=%d denied=%d", calls, len(e.entries))
	}
	for _, entry := range e.entries {
		if entry.Kind != "forbidden" {
			t.Fatal("denial was not counted")
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if calls != 1 || response.Code != 204 {
		t.Fatal("expected health route was denied")
	}
}
