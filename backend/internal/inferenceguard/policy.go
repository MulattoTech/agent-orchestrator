// Package inferenceguard enforces owner policy immediately before provider writes.
// Its ledger contains identifiers and counters, never prompts or credentials.
package inferenceguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RequirePolicy is set to "1" in the unofficial reliability build.
var RequirePolicy string

const PolicyEnv = "AO_INFERENCE_POLICY"

var (
	ErrPolicy    = errors.New("MODEL_POLICY_DENIED")
	ErrQuota     = errors.New("QUOTA_BLOCKED")
	ErrCapacity  = errors.New("INFERENCE_CAPACITY")
	ErrUncertain = errors.New("DISPATCH_OUTCOME_UNCERTAIN")
)

// Refusal proves that this invocation did not cross the provider write boundary.
type Refusal struct {
	Code   error
	Reason string
}

func (e *Refusal) Error() string           { return e.Code.Error() + ": " + e.Reason }
func (e *Refusal) Unwrap() error           { return e.Code }
func PreDispatchRefusal(err error) bool    { var e *Refusal; return errors.As(err, &e) }
func deny(code error, reason string) error { return &Refusal{code, reason} }

type RolePolicy struct {
	DefaultModel string   `json:"default_model"`
	Models       []string `json:"models"`
}
type Exception struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Session   string    `json:"session"`
	Model     string    `json:"model"`
	Effort    string    `json:"effort"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Policy struct {
	AuthorityPath       string                `json:"authority_path"`
	NativeAccountSHA256 string                `json:"native_account_sha256"`
	Version             int                   `json:"version"`
	AccountKey          string                `json:"account_key"`
	AuthorizationEpoch  string                `json:"authorization_epoch"`
	EmergencyStop       bool                  `json:"emergency_stop"`
	Pause               bool                  `json:"pause"`
	MaximumEffort       string                `json:"maximum_effort"`
	MaximumInference    int                   `json:"maximum_inference"`
	MaximumPerProject   int                   `json:"maximum_per_project"`
	MaximumReviewers    int                   `json:"maximum_reviewers"`
	TurnBudget          int                   `json:"turn_budget"`
	ReserveTurns        int                   `json:"reserve_turns"`
	Roles               map[string]RolePolicy `json:"roles"`
	Exceptions          []Exception           `json:"exceptions,omitempty"`
}
type Selection struct {
	RequestedModel  string `json:"requested_model"`
	RequestedEffort string `json:"requested_effort"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Role            string `json:"role"`
	ExceptionID     string `json:"exception_id,omitempty"`
}

func Enabled() bool {
	return RequirePolicy == "1" || os.Getenv("AO_REQUIRE_INFERENCE_POLICY") == "1" || os.Getenv(PolicyEnv) != ""
}

// CheckLaunch refuses boundaries whose individual dispatches cannot be governed.
func CheckLaunch(harness, mode string) error {
	if !Enabled() {
		return nil
	}
	if harness == "fake" && mode == "tui" && os.Getenv("AO_FAKE_HARNESS") == "1" {
		return nil
	}
	if harness != "codex" || mode != "chat" {
		return deny(ErrPolicy, "strict policy requires the governed Codex Chat boundary; terminal and fallback providers are disabled")
	}
	_, err := ReadPolicy()
	return err
}
func ReadPolicy() (Policy, error) {
	var p Policy
	path := os.Getenv(PolicyEnv)
	if path == "" || !filepath.IsAbs(path) {
		return p, deny(ErrPolicy, "an absolute AO_INFERENCE_POLICY file is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return p, deny(ErrPolicy, "owner policy cannot be read")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&p); err != nil {
		return p, deny(ErrPolicy, "owner policy is invalid: "+err.Error())
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return p, deny(ErrPolicy, "owner policy has trailing content")
	}
	return p, p.Validate()
}
func (p Policy) Validate() error {
	if p.AuthorityPath != "" && !filepath.IsAbs(p.AuthorityPath) {
		return deny(ErrPolicy, "account authority path must be absolute")
	}
	if p.NativeAccountSHA256 != "" {
		b, e := hex.DecodeString(p.NativeAccountSHA256)
		if e != nil || len(b) != 32 {
			return deny(ErrPolicy, "native account fingerprint must be SHA-256")
		}
	}
	if p.Version != 1 || strings.TrimSpace(p.AccountKey) == "" || strings.TrimSpace(p.AuthorizationEpoch) == "" {
		return deny(ErrPolicy, "policy version, account binding, and authorization epoch are required")
	}
	if effortRank(p.MaximumEffort) < 0 || effortRank(p.MaximumEffort) > effortRank("medium") {
		return deny(ErrPolicy, "automatic effort ceiling must be Medium or lower")
	}
	if p.MaximumInference < 1 || p.MaximumInference > 40 || p.MaximumPerProject < 1 || p.MaximumPerProject > p.MaximumInference || p.MaximumReviewers < 1 || p.MaximumReviewers > p.MaximumInference {
		return deny(ErrPolicy, "invalid separate inference/project/reviewer caps")
	}
	if p.TurnBudget < 1 || p.ReserveTurns < 0 || p.ReserveTurns >= p.TurnBudget {
		return deny(ErrPolicy, "an explicit conservative turn budget and reserve are required")
	}
	for _, role := range []string{"orchestrator", "implementation", "complex", "reviewer", "lightweight"} {
		r, ok := p.Roles[role]
		if !ok || r.DefaultModel == "" || !contains(r.Models, r.DefaultModel) {
			return deny(ErrPolicy, "role "+role+" has no allowed default model")
		}
		for _, model := range r.Models {
			if strings.TrimSpace(model) == "" {
				return deny(ErrPolicy, "empty model in role policy")
			}
			if role != "lightweight" && strings.Contains(strings.ToLower(model), "luna") {
				return deny(ErrPolicy, "Luna is restricted to explicitly qualified lightweight tasks")
			}
		}
	}
	return nil
}
func contains(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}
func effortRank(v string) int {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none", "off":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh", "extra_high", "extra-high":
		return 5
	case "max", "ultra":
		return 6
	default:
		return -1
	}
}

// Resolve never downgrades a requested unsupported model/effort silently.
func (p Policy) Resolve(role, session, model, effort string, now time.Time) (Selection, error) {
	if role == "" {
		role = "implementation"
	}
	s := Selection{RequestedModel: model, RequestedEffort: effort, Role: role}
	r, ok := p.Roles[role]
	if !ok {
		return s, deny(ErrPolicy, "unknown role "+role)
	}
	s.Model = model
	if s.Model == "" {
		s.Model = r.DefaultModel
	}
	s.Effort = strings.ToLower(strings.TrimSpace(effort))
	if s.Effort == "" {
		s.Effort = p.MaximumEffort
	}
	if !contains(r.Models, s.Model) {
		return s, deny(ErrPolicy, fmt.Sprintf("model %q is not allowed for %s", s.Model, role))
	}
	rank := effortRank(s.Effort)
	if rank < 0 {
		return s, deny(ErrPolicy, "unsupported reasoning effort")
	}
	if rank > effortRank(p.MaximumEffort) {
		for _, e := range p.Exceptions {
			if e.ID != "" && e.Owner != "" && e.Reason != "" && e.Session == session && e.Model == s.Model && e.Effort == s.Effort && e.ExpiresAt.After(now) && e.ExpiresAt.Sub(now) <= 24*time.Hour {
				s.ExceptionID = e.ID
				return s, nil
			}
		}
		return s, deny(ErrPolicy, "requested reasoning exceeds the owner ceiling; no matching unexpired owner exception")
	}
	return s, nil
}

// AccountFingerprint hashes non-secret native identity without storing its email.
func AccountFingerprint(kind, email string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(h[:])
}
