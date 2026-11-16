package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestReviewPublishUsesSavedRunWithoutTriggeringReviewer(t *testing.T) {
	cfg := setReviewEnv(t)
	srv, capture := reviewServer(t, http.StatusOK, `{"review":{"id":"saved","verdict":"approved","targetSha":"1111111111111111111111111111111111111111","githubReviewId":"991"},"providerState":"COMMENTED","reused":true}`)
	writeRunFileFor(t, cfg, srv)
	out, errOut, err := executeCLI(t, aliveDeps(), "review", "publish", "worker-1", "--run", "saved")
	if err != nil {
		t.Fatalf("error=%v stderr=%s", err, errOut)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/sessions/worker-1/reviews/publish" {
		t.Fatalf("request=%+v", capture)
	}
	var request publishReviewRequest
	if err := json.Unmarshal([]byte(capture.body), &request); err != nil {
		t.Fatal(err)
	}
	if request.RunID != "saved" || !strings.Contains(out, "GitHub review state COMMENTED") || !strings.Contains(out, "saved AO verdict approved") {
		t.Fatalf("request=%+v output=%s", request, out)
	}
}
func TestReviewPublishRejectsIncompleteReceipt(t *testing.T) {
	cfg := setReviewEnv(t)
	srv, _ := reviewServer(t, http.StatusOK, `{"review":{"id":"saved","verdict":"approved"}}`)
	writeRunFileFor(t, cfg, srv)
	if _, _, err := executeCLI(t, aliveDeps(), "review", "publish", "worker-1", "--run", "saved"); err == nil {
		t.Fatal("incomplete receipt accepted")
	}
}

func TestReviewSubmitDirectJSONNeedsNoShellOrInputFile(t *testing.T) {
	cfg := setReviewEnv(t)
	srv, capture := reviewServer(t, http.StatusOK, `{"reviews":[{"id":"run-1","verdict":"approved"}]}`)
	writeRunFileFor(t, cfg, srv)
	payload := `{"reviews":[{"runId":"run-1","verdict":"approved","body":"apostrophe ' and backslash \\ preserved"}]}`
	if _, _, err := executeCLI(t, aliveDeps(), "review", "submit", "worker-1", "--reviews-json", payload); err != nil {
		t.Fatal(err)
	}
	var got submitReviewRequest
	if err := json.Unmarshal([]byte(capture.body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Reviews) != 1 || got.Reviews[0].Body != "apostrophe ' and backslash \\ preserved" {
		t.Fatalf("altered JSON=%+v", got)
	}
	if _, _, err := executeCLI(t, aliveDeps(), "review", "submit", "worker-1", "--reviews-json", payload, "--reviews", "-"); err == nil {
		t.Fatal("ambiguous JSON sources accepted")
	}
}
