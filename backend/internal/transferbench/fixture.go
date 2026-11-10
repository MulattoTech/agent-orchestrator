package transferbench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Fixture is one provider's scripted conversation. Fixtures are reviewed,
// committed files: every path and text in them is dummy content, and their
// provenance field cites the first-party capture each shape follows.
//
// A fixture is a script of normalized provider events, the ports.ChatEvent
// stream a Chat driver hands the daemon. Raw provider wire frames are not
// replayed: the benchmark seam starts after the Codex app-server and ACP
// adapters normalize them.
type Fixture struct {
	SchemaVersion string `json:"schemaVersion"`
	Provider      string `json:"provider"`
	// Harness is the AO agent harness id the session is created with.
	Harness    string `json:"harness"`
	Provenance string `json:"provenance"`
	// ProviderConversationID is the dummy native thread id.
	ProviderConversationID string `json:"providerConversationId"`
	// HistoryTurns is how many completed turns precede the live turn. History
	// turn n uses HistoryTemplates[(n-1) % len].
	HistoryTurns     int            `json:"historyTurns"`
	HistoryTemplates []TurnTemplate `json:"historyTemplates"`
	LiveTurn         TurnTemplate   `json:"liveTurn"`
}

// TurnTemplate is one user message and the provider events that answer it.
// The token {n} in any string, including inside Detail, becomes the turn
// number so every expanded turn carries distinct ids and text.
type TurnTemplate struct {
	User  string `json:"user"`
	Steps []Step `json:"steps"`
}

// Step is one normalized provider event. Kind is a ports.ChatEventKind value.
type Step struct {
	Kind         string          `json:"kind"`
	Item         string          `json:"item,omitempty"`
	Delta        string          `json:"delta,omitempty"`
	Text         string          `json:"text,omitempty"`
	ActivityKind string          `json:"activityKind,omitempty"`
	Status       string          `json:"status,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	Detail       json.RawMessage `json:"detail,omitempty"`
	TurnState    string          `json:"turnState,omitempty"`
	// Repeat replays a delta step this many times, each with " {r}" (the
	// repetition number) available in Delta. Zero means once.
	Repeat int `json:"repeat,omitempty"`
}

// StepKinds are the event kinds a fixture may script.
var StepKinds = []string{
	"turn.started", "turn.completed", "message.delta", "message.completed",
	"reasoning.delta", "activity.started", "activity.completed", "command.output.delta",
}

// Turn is an expanded template.
type Turn struct {
	Number int
	User   string
	Steps  []Step
}

// DecodeFixture decodes a committed fixture, rejecting unknown fields.
func DecodeFixture(data []byte) (Fixture, error) {
	var fixture Fixture
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fixture); err != nil {
		return Fixture{}, err
	}
	if dec.More() {
		return Fixture{}, errors.New("fixture has data after its JSON value")
	}
	if fixture.SchemaVersion != FixtureSchema {
		return Fixture{}, fmt.Errorf("fixture schemaVersion must be %q", FixtureSchema)
	}
	if !slices.Contains(Providers, fixture.Provider) {
		return Fixture{}, errors.New("fixture provider is not allowlisted")
	}
	if fixture.HistoryTurns < 1 || len(fixture.HistoryTemplates) == 0 {
		return Fixture{}, errors.New("fixture needs history turns and templates")
	}
	for _, template := range append(slices.Clone(fixture.HistoryTemplates), fixture.LiveTurn) {
		if err := template.validate(); err != nil {
			return Fixture{}, err
		}
	}
	return fixture, nil
}

func (t TurnTemplate) validate() error {
	if strings.TrimSpace(t.User) == "" || len(t.Steps) < 2 {
		return errors.New("a turn template needs a user message and steps")
	}
	if t.Steps[0].Kind != "turn.started" || t.Steps[len(t.Steps)-1].Kind != "turn.completed" {
		return errors.New("a turn template must start with turn.started and end with turn.completed")
	}
	for _, step := range t.Steps {
		if !slices.Contains(StepKinds, step.Kind) {
			return fmt.Errorf("step kind %q is not supported", step.Kind)
		}
		if step.Repeat < 0 || (step.Repeat > 0 && !strings.HasSuffix(step.Kind, ".delta")) {
			return errors.New("only delta steps repeat")
		}
		if len(step.Detail) > 0 && !json.Valid(step.Detail) {
			return errors.New("step detail is not valid JSON")
		}
	}
	return nil
}

// HistoryTurnList expands the history turns in order.
func (f Fixture) HistoryTurnList() []Turn {
	turns := make([]Turn, 0, f.HistoryTurns)
	for n := 1; n <= f.HistoryTurns; n++ {
		turns = append(turns, expand(f.HistoryTemplates[(n-1)%len(f.HistoryTemplates)], n))
	}
	return turns
}

// Live expands the live turn, numbered after the history.
func (f Fixture) Live() Turn { return expand(f.LiveTurn, f.HistoryTurns+1) }

func expand(t TurnTemplate, n int) Turn {
	number := strconv.Itoa(n)
	sub := func(s string) string { return strings.ReplaceAll(s, "{n}", number) }
	turn := Turn{Number: n, User: sub(t.User)}
	for _, step := range t.Steps {
		count := max(step.Repeat, 1)
		for r := 1; r <= count; r++ {
			expanded := step
			expanded.Repeat = 0
			expanded.Item = sub(step.Item)
			expanded.Delta = strings.ReplaceAll(sub(step.Delta), "{r}", strconv.Itoa(r))
			expanded.Text = sub(step.Text)
			expanded.Summary = sub(step.Summary)
			if len(step.Detail) > 0 {
				expanded.Detail = json.RawMessage(sub(string(step.Detail)))
			}
			turn.Steps = append(turn.Steps, expanded)
		}
	}
	return turn
}

// CanonicalRow is one timeline row reduced to what a reader sees: its kind,
// role or activity kind, status, a digest of its text, and the size of any
// command output.
type CanonicalRow struct {
	Kind         string // "message" or "activity"
	Role         string
	ActivityKind string
	Status       string
	Text         string // message text, or activity summary
	OutputBytes  int64
}

// CanonicalState renders rows (in sequence order) and turn states (in request
// order) as reviewable text. A state digest is the sha256 of this text.
func CanonicalState(rows []CanonicalRow, turnStates []string) string {
	var b strings.Builder
	for _, state := range turnStates {
		b.WriteString("turn\t" + state + "\n")
	}
	for _, row := range rows {
		switch row.Kind {
		case "message":
			fmt.Fprintf(&b, "message\t%s\t%s\t%d\n", row.Role, SHA256Hex([]byte(row.Text)), len(row.Text))
		default:
			fmt.Fprintf(&b, "activity\t%s\t%s\t%s\t%d\n", row.ActivityKind, row.Status, SHA256Hex([]byte(row.Text)), row.OutputBytes)
		}
	}
	return b.String()
}
