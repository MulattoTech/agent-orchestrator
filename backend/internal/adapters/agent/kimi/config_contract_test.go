package kimi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/authprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Repeated observations of one unchanged profile expose the old map-order
// dependence without starting Kimi or modifying credentials between probes.
const kimiContractObservations = 128

const kimiContractMixedTOML = "[providers.alpha.oauth]\nkey = \"oauth/broken\"\n[providers.zulu.oauth]\nkey = \"oauth/good\"\n"

func TestKimiConfiguredAuthUsesValidSibling(t *testing.T) {
	for _, tc := range []struct{ name, file, config string }{
		{"TOML", "config.toml", kimiContractMixedTOML},
		{"JSON", "config.json", `{"providers":{"alpha":{"oauth":{"key":"oauth/broken"}},"zulu":{"oauth":{"key":"oauth/good"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKimiContractHomes(t)
			writeKimiContractFile(t, filepath.Join(f.code, tc.file), tc.config)
			writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), map[string]string{
				"broken.json": "malformed synthetic credential",
				"good.json":   `{"refresh_token":"synthetic-good"}`,
			})
			want := authprobe.FileSource(filepath.Join(f.code, "credentials", "good.json")).WithMethod(domain.AgentAccountMethodAccount)
			for observation := 0; observation < kimiContractObservations; observation++ {
				assertKimiContractAuth(t, want, observation)
			}
		})
	}
}

func TestKimiConfiguredAuthEvidenceIsStable(t *testing.T) {
	for _, tc := range []struct{ name, file, config string }{
		{"TOML", "config.toml", "[providers.zulu.oauth]\nkey = \"oauth/a-login\"\n[providers.alpha.oauth]\nkey = \"oauth/z-login\"\n"},
		{"JSON", "config.json", `{"providers":{"zulu":{"oauth":{"key":"oauth/a-login"}},"alpha":{"oauth":{"key":"oauth/z-login"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKimiContractHomes(t)
			writeKimiContractFile(t, filepath.Join(f.code, tc.file), tc.config)
			writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), map[string]string{
				"a-login.json": `{"access_token":"synthetic-zulu"}`,
				"z-login.json": `{"access_token":"synthetic-alpha"}`,
			})
			// Provider-name order, not token bytes or credential filename,
			// determines which usable configured source supplies the evidence.
			want := authprobe.FileSource(filepath.Join(f.code, "credentials", "z-login.json")).WithMethod(domain.AgentAccountMethodAccount)
			for observation := 0; observation < kimiContractObservations; observation++ {
				assertKimiContractAuth(t, want, observation)
			}
		})
	}
}

func TestKimiConfiguredOAuthAllowsValidSiblingForSeeding(t *testing.T) {
	f := newKimiContractHomes(t)
	files := map[string]string{
		"broken.json": "malformed synthetic credential",
		"good.json":   `{"refresh_token":"synthetic-good"}`,
	}
	writeKimiContractFile(t, filepath.Join(f.code, "config.toml"), kimiContractMixedTOML)
	writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
	target := t.TempDir()
	for observation := 0; observation < kimiContractObservations; observation++ {
		authorized, err := kimiSourceOAuthAuthorized(f.code)
		if err != nil || !authorized {
			t.Fatalf("observation %d: configured usable login rejected: authorized=%v, error=%v", observation, authorized, err)
		}
		config, seeded, err := kimiSeedConfig(filepath.Join(target, "config.toml"), nil)
		if err != nil || !seeded || string(config) != kimiContractMixedTOML {
			t.Fatalf("observation %d: configured login did not seed its config: seeded=%v, error=%v", observation, seeded, err)
		}
	}
	linkCalls := 0
	err := seedKimiCredentialsWithOps(target, kimiCredentialStoreOps{
		rename: os.Rename,
		symlink: func(oldPath, newPath string) error {
			linkCalls++
			if oldPath != filepath.Join(f.code, "credentials") || newPath != filepath.Join(target, "credentials") {
				t.Fatal("sharing selected a different fixture store")
			}
			// Operation success only: this is not a native symlink.
			return os.Mkdir(newPath, 0o700)
		},
	})
	if err != nil || linkCalls != 1 {
		t.Fatalf("seed error=%v, link attempts=%d; want one successful operation", err, linkCalls)
	}
	assertKimiRecoveryFiles(t, filepath.Join(target, "credentials"), nil)
	assertKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
}

func TestKimiConfiguredOAuthErrorsRemainDeterministic(t *testing.T) {
	f := newKimiContractHomes(t)
	config := "[providers.zulu.oauth]\nkey = \"oauth/second-broken\"\n[providers.alpha.oauth]\nkey = \"oauth/first-broken\"\n"
	files := map[string]string{"first-broken.json": "malformed first fixture", "second-broken.json": "malformed second fixture"}
	writeKimiContractFile(t, filepath.Join(f.code, "config.toml"), config)
	writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
	first := filepath.Join(f.code, "credentials", "first-broken.json")
	for observation := 0; observation < kimiContractObservations; observation++ {
		authorized, err := kimiSourceOAuthAuthorized(f.code)
		if authorized || err == nil || !strings.Contains(err.Error(), first) {
			t.Fatalf("observation %d: want the stable configured-source error, authorized=%v, error=%v", observation, authorized, err)
		}
		status, found, err := kimiLocalAuthStatus(context.Background())
		if err == nil || found || status != ports.AgentAuthStatusUnknown {
			t.Fatalf("malformed configured credentials lost their error: status=%v, found=%v, error=%v", status, found, err)
		}
	}
	target := t.TempDir()
	err := seedKimiCredentialsWithOps(target, kimiCredentialStoreOps{
		rename:  func(_, _ string) error { t.Fatal("invalid source attempted a store move"); return nil },
		symlink: func(_, _ string) error { t.Fatal("invalid source attempted sharing"); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), first) {
		t.Fatalf("copy fallback lost the configured-source error: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid source created a target store: %v", err)
	}
	assertKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
}

func TestKimiMixedReferenceCopyFallbackReportsErrors(t *testing.T) {
	f := newKimiContractHomes(t)
	files := map[string]string{
		"broken.json":    "malformed synthetic credential",
		"good.json":      `{"refresh_token":"synthetic-good"}`,
		"kimi-code.json": `{"refresh_token":"synthetic-unreferenced-legacy"}`,
		"unrelated.json": `{"refresh_token":"synthetic-unrelated"}`,
	}
	writeKimiContractFile(t, filepath.Join(f.code, "config.toml"), kimiContractMixedTOML)
	writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
	target := t.TempDir()
	linkCalls := 0
	err := seedKimiCredentialsWithOps(target, kimiCredentialStoreOps{
		rename: os.Rename,
		symlink: func(_, _ string) error {
			linkCalls++
			return errors.New("synthetic unavailable link")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "broken.json") || linkCalls != 1 {
		t.Fatalf("fallback must report the referenced error: error=%v, link attempts=%d", err, linkCalls)
	}
	for _, name := range []string{"kimi-code.json", "unrelated.json", "broken.json"} {
		if _, err := os.Lstat(filepath.Join(target, "credentials", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("fallback copied an unreferenced or malformed file %s: %v", name, err)
		}
	}
	// Fallback is not transactional: an earlier usable reference may have
	// been copied before the error. Never turn that partial work into success.
	if data, err := os.ReadFile(filepath.Join(target, "credentials", "good.json")); err == nil {
		if string(data) != files["good.json"] {
			t.Error("partial fallback changed the synthetic credential")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	assertKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
}

func TestKimiLocalAuthHomeAndFormatPrecedence(t *testing.T) {
	const toml = "[providers.only.oauth]\nkey = \"oauth/toml-login\"\n"
	const jsonConfig = `{"providers":{"only":{"oauth":{"key":"oauth/json-login"}}}}`
	const brokenRef = "[providers.only.oauth]\nkey = \"oauth/broken\"\n"
	for _, tc := range []struct {
		name, shareTOML, shareJSON, codeTOML, codeJSON, wantHome, wantFile string
		shareLegacy, codeLegacy, wantError, environmentFallback            bool
	}{
		{name: "share TOML before JSON and code home", shareTOML: toml, shareJSON: jsonConfig, codeTOML: toml, wantHome: "share", wantFile: "toml-login.json"},
		{name: "share JSON before code TOML", shareJSON: jsonConfig, codeTOML: toml, wantHome: "share", wantFile: "json-login.json"},
		{name: "code TOML after an empty share home", codeTOML: toml, wantHome: "code", wantFile: "toml-login.json"},
		{name: "code JSON before legacy credential", codeJSON: jsonConfig, codeLegacy: true, wantHome: "code", wantFile: "json-login.json"},
		{name: "share legacy before code home", shareLegacy: true, codeTOML: toml, wantHome: "share", wantFile: "kimi-code.json"},
		{name: "malformed share TOML stops format and home fallback", shareTOML: "[malformed", shareJSON: jsonConfig, codeTOML: toml, wantError: true},
		{name: "malformed reference stops JSON legacy and home fallback", shareTOML: brokenRef, shareJSON: jsonConfig, shareLegacy: true, codeTOML: toml, wantError: true},
		{name: "environment key remains explicit stored-error fallback", shareTOML: "[malformed", shareJSON: jsonConfig, codeTOML: toml, environmentFallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKimiContractHomes(t)
			for home, configs := range map[string]map[string]string{
				f.share: {"config.toml": tc.shareTOML, "config.json": tc.shareJSON},
				f.code:  {"config.toml": tc.codeTOML, "config.json": tc.codeJSON},
			} {
				for name, body := range configs {
					if body != "" {
						writeKimiContractFile(t, filepath.Join(home, name), body)
					}
				}
				writeKimiRecoveryFiles(t, filepath.Join(home, "credentials"), map[string]string{
					"toml-login.json": `{"access_token":"synthetic-toml"}`,
					"json-login.json": `{"access_token":"synthetic-json"}`,
					"broken.json":     "malformed synthetic credential",
				})
			}
			for home, enabled := range map[string]bool{f.share: tc.shareLegacy, f.code: tc.codeLegacy} {
				if enabled {
					writeKimiContractFile(t, filepath.Join(home, "credentials", "kimi-code.json"), `{"refresh_token":"synthetic-legacy"}`)
				}
			}
			if tc.environmentFallback {
				t.Setenv("KIMI_API_KEY", "synthetic-environment-key")
			}
			ctx, evidence := ports.WithAuthEvidence(context.Background())
			status, found, err := kimiLocalAuthStatus(ctx)
			if tc.wantError {
				if err == nil || found || status != ports.AgentAuthStatusUnknown || evidence.Identity() != (domain.AgentAccountIdentity{}) {
					t.Fatalf("higher-priority error lost its scope boundary: status=%v, found=%v, error=%v", status, found, err)
				}
				return
			}
			wantHome := f.code
			if tc.wantHome == "share" {
				wantHome = f.share
			}
			want := authprobe.FileSource(filepath.Join(wantHome, "credentials", tc.wantFile)).WithMethod(domain.AgentAccountMethodAccount)
			if tc.environmentFallback {
				want = domain.EnvironmentSource("KIMI_API_KEY").WithMethod(domain.AgentAccountMethodAPIKey)
			}
			if err != nil || !found || status != ports.AgentAuthStatusAuthorized || evidence.Identity() != want {
				t.Fatalf("credential precedence changed: status=%v, found=%v, error=%v", status, found, err)
			}
		})
	}
}

func TestKimiHookSourceRemainsCodeHomeTOML(t *testing.T) {
	const codeTOML = "[providers.code.oauth]\nkey = \"oauth/code-login\"\n"
	const codeJSON = `{"providers":{"code":{"oauth":{"key":"oauth/code-login"}}}}`
	const shareJSON = `{"providers":{"share":{"oauth":{"key":"oauth/share-login"}}}}`
	for _, tc := range []struct {
		name, filename, config string
		seeded, wantError      bool
	}{
		{"explicit code TOML", "config.toml", codeTOML, true, false},
		{"code JSON alone is not a hook seed", "config.json", codeJSON, false, false},
		{"invalid code TOML does not fall back to share JSON", "config.toml", "[malformed", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKimiContractHomes(t)
			writeKimiContractFile(t, filepath.Join(f.code, tc.filename), tc.config)
			writeKimiContractFile(t, filepath.Join(f.share, "config.json"), shareJSON)
			codeFiles := map[string]string{"code-login.json": `{"refresh_token":"synthetic-code"}`}
			shareFiles := map[string]string{"share-login.json": `{"refresh_token":"synthetic-share"}`}
			writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), codeFiles)
			writeKimiRecoveryFiles(t, filepath.Join(f.share, "credentials"), shareFiles)
			assertKimiContractAuth(t, authprobe.FileSource(filepath.Join(f.share, "credentials", "share-login.json")).WithMethod(domain.AgentAccountMethodAccount), 0)
			dataDir := t.TempDir()
			env := map[string]string{"KIMI_SHARE_DIR": f.share}
			New().AugmentRuntimeEnv(env, dataDir)
			target := filepath.Join(dataDir, "kimi")
			if env[kimiCodeHomeEnv] != target || env["KIMI_SHARE_DIR"] != f.share {
				t.Fatal("runtime managed-home contract changed")
			}
			linkCalls := 0
			err := seedKimiCredentialsWithOps(target, kimiCredentialStoreOps{
				rename: os.Rename,
				symlink: func(oldPath, _ string) error {
					linkCalls++
					if oldPath != filepath.Join(f.code, "credentials") {
						t.Fatal("hook seeding changed its source home")
					}
					return errors.New("synthetic unavailable link")
				},
			})
			config, seeded, configErr := kimiSeedConfig(filepath.Join(target, "config.toml"), nil)
			if tc.wantError {
				if err == nil || configErr == nil || seeded || linkCalls != 0 {
					t.Fatalf("invalid code config escaped its scope: seed=%v, config=%v, links=%d", err, configErr, linkCalls)
				}
			} else if err != nil || configErr != nil || seeded != tc.seeded {
				t.Fatalf("hook format contract changed: seed=%v, config=%v, seeded=%v", err, configErr, seeded)
			}
			if tc.seeded {
				if string(config) != codeTOML || linkCalls != 1 {
					t.Fatal("hook seeding selected another profile or format")
				}
				assertKimiRecoveryFiles(t, filepath.Join(target, "credentials"), codeFiles)
			} else if _, err := os.Lstat(filepath.Join(target, "credentials")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("non-seed source created a target credential store: %v", err)
			}
			assertKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), codeFiles)
			assertKimiRecoveryFiles(t, filepath.Join(f.share, "credentials"), shareFiles)
		})
	}
}

func TestKimiDefaultHomesKeepAuthAndHookScopes(t *testing.T) {
	f := newKimiContractHomes(t)
	t.Setenv("KIMI_SHARE_DIR", "")
	t.Setenv(kimiCodeHomeEnv, "")
	share, code := filepath.Join(f.profile, ".kimi"), filepath.Join(f.profile, ".kimi-code")
	writeKimiContractFile(t, filepath.Join(share, "config.json"), `{"providers":{"share":{"oauth":{"key":"oauth/shared"}}}}`)
	const config = "[providers.code.oauth]\nkey = \"oauth/coded\"\n"
	writeKimiContractFile(t, filepath.Join(code, "config.toml"), config)
	shareFiles := map[string]string{"shared.json": `{"access_token":"synthetic-default-share"}`}
	codeFiles := map[string]string{"coded.json": `{"access_token":"synthetic-default-code"}`}
	writeKimiRecoveryFiles(t, filepath.Join(share, "credentials"), shareFiles)
	writeKimiRecoveryFiles(t, filepath.Join(code, "credentials"), codeFiles)
	assertKimiContractAuth(t, authprobe.FileSource(filepath.Join(share, "credentials", "shared.json")).WithMethod(domain.AgentAccountMethodAccount), 0)
	target := t.TempDir()
	err := seedKimiCredentialsWithOps(target, kimiCredentialStoreOps{
		rename: os.Rename,
		symlink: func(oldPath, _ string) error {
			if oldPath != filepath.Join(code, "credentials") {
				t.Fatal("default hook source escaped the owned code home")
			}
			return errors.New("synthetic unavailable link")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	seededConfig, seeded, err := kimiSeedConfig(filepath.Join(target, "config.toml"), nil)
	if err != nil || !seeded || string(seededConfig) != config {
		t.Fatalf("default code config was not seeded: seeded=%v, error=%v", seeded, err)
	}
	assertKimiRecoveryFiles(t, filepath.Join(target, "credentials"), codeFiles)
	assertKimiRecoveryFiles(t, filepath.Join(share, "credentials"), shareFiles)
	assertKimiRecoveryFiles(t, filepath.Join(code, "credentials"), codeFiles)
}

type kimiContractHomes struct{ profile, share, code string }

func newKimiContractHomes(t *testing.T) kimiContractHomes {
	t.Helper()
	root := t.TempDir()
	f := kimiContractHomes{
		profile: filepath.Join(root, "profile"),
		share:   filepath.Join(root, "share"),
		code:    filepath.Join(root, "code"),
	}
	for _, dir := range []string{f.profile, f.share, f.code} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", f.profile)
	t.Setenv("USERPROFILE", f.profile)
	t.Setenv("KIMI_SHARE_DIR", f.share)
	t.Setenv(kimiCodeHomeEnv, f.code)
	clearKimiAuthEnv(t)
	if home, err := os.UserHomeDir(); err != nil || home != f.profile {
		t.Fatalf("fixture home was not selected: %v", err)
	}
	return f
}

func writeKimiContractFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertKimiContractAuth(t *testing.T, want domain.AgentAccountIdentity, observation int) {
	t.Helper()
	ctx, evidence := ports.WithAuthEvidence(context.Background())
	status, found, err := kimiLocalAuthStatus(ctx)
	if err != nil || !found || status != ports.AgentAuthStatusAuthorized || evidence.Identity() != want {
		t.Fatalf("observation %d: configured credential selection changed: status=%v, found=%v, error=%v", observation, status, found, err)
	}
}
