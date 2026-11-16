package domain

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// PRCompletionDependency waits for a provider-observed merge of this exact
// session's PR head. It does not assert that product requirements were accepted.
// PRURL must name the canonical PR URL stored by the SCM observer.
type PRCompletionDependency struct {
	SessionID SessionID `json:"sessionId"`
	PRURL     string    `json:"prUrl"`
	HeadSHA   string    `json:"headSha"`
}

// ValidFullCommitSHA rejects abbreviated or non-hex commit identifiers.
func ValidFullCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidatePRCompletionDependencies(dependencies []PRCompletionDependency) error {
	if len(dependencies) > 64 {
		return fmt.Errorf("prCompletionDependencies must contain at most 64 entries")
	}
	seen := make(map[string]bool)
	for i, dependency := range dependencies {
		id := string(dependency.SessionID)
		if id == "" || len(id) > 128 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n\t") {
			return fmt.Errorf("prCompletionDependencies[%d].sessionId is invalid", i)
		}
		u, err := url.Parse(dependency.PRURL)
		if err != nil || len(dependency.PRURL) > 2048 || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.Path == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("prCompletionDependencies[%d].prUrl must be a canonical HTTP(S) PR URL", i)
		}
		if !ValidFullCommitSHA(dependency.HeadSHA) {
			return fmt.Errorf("prCompletionDependencies[%d].headSha must be a full commit SHA", i)
		}
		key := id + "\x00" + dependency.PRURL
		if seen[key] {
			return fmt.Errorf("prCompletionDependencies[%d] duplicates a session PR", i)
		}
		seen[key] = true
	}
	return nil
}
