package controllers_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

func TestSessionsAPI_PRCompletionDependenciesPassAndFingerprint(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	request := controllers.SpawnSessionRequest{ProjectID: "ao", ClientRequestID: "fixed-request", AdmissionDependencies: []string{"admit-existing"}, PRCompletionDependencies: []domain.PRCompletionDependency{{SessionID: "ao-1", PRURL: "https://github.com/example/repo/pull/7", HeadSHA: strings.Repeat("a", 40)}}}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions", string(encoded))
	if status != http.StatusCreated || len(svc.lastSpawn.PRCompletionDependencies) != 1 || svc.lastSpawn.PRCompletionDependencies[0] != request.PRCompletionDependencies[0] || len(svc.lastSpawn.AdmissionDependencies) != 1 {
		t.Fatalf("dependency criteria not passed to manager: %d %s %+v", status, body, svc.lastSpawn)
	}
	previous := svc.lastSpawn.ClientRequestHash
	request.PRCompletionDependencies[0].HeadSHA = strings.Repeat("b", 40)
	encoded, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	_, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/sessions", string(encoded))
	if status != http.StatusCreated || previous == "" || previous == svc.lastSpawn.ClientRequestHash {
		t.Fatal("changed required PR head did not change idempotency fingerprint")
	}
}

func TestSessionsAPI_PRCompletionDependenciesRejectInvalid(t *testing.T) {
	valid := domain.PRCompletionDependency{SessionID: "ao-1", PRURL: "https://github.com/example/repo/pull/7", HeadSHA: strings.Repeat("a", 40)}
	for _, scenario := range []string{"session", "url", "head", "duplicate", "too_many"} {
		t.Run(scenario, func(t *testing.T) {
			svc := newFakeSessionService()
			srv := newSessionTestServer(t, svc)
			dependencies := []domain.PRCompletionDependency{valid}
			switch scenario {
			case "session":
				dependencies[0].SessionID = ""
			case "url":
				dependencies[0].PRURL = "file:///local-fake-merge"
			case "head":
				dependencies[0].HeadSHA = "short"
			case "duplicate":
				dependencies = append(dependencies, valid)
			case "too_many":
				dependencies = make([]domain.PRCompletionDependency, 65)
			}
			encoded, err := json.Marshal(controllers.SpawnSessionRequest{PRCompletionDependencies: dependencies})
			if err != nil {
				t.Fatal(err)
			}
			body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions", string(encoded))
			if status != http.StatusBadRequest || !strings.Contains(string(body), "INVALID_PR_COMPLETION_DEPENDENCY") || svc.lastSpawn.Kind != "" {
				t.Fatalf("invalid criterion reached session service: %d %s %+v", status, body, svc.lastSpawn)
			}
		})
	}
}
