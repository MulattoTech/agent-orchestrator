package kimi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const kimiScopedTestConfig = "[providers.scoped.oauth]\nstorage = \"file\"\nkey = \"oauth/scoped-login\"\n"

func TestSeedKimiCredentialsUsesConfiguredScopedStore(t *testing.T) {
	for _, withDefault := range []bool{false, true} {
		name := "scoped credential only"
		if withDefault {
			name = "scoped and unrelated legacy credentials"
		}
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"scoped-login.json": "{\"refresh_token\":\"synthetic-scoped-refresh\"}",
			}
			if withDefault {
				files["kimi-code.json"] = "{\"access_token\":\"synthetic-unrelated-default\"}"
			}
			sourceHome, targetHome := newKimiScopedFixture(t, kimiScopedTestConfig, files)
			source := filepath.Join(sourceHome, "credentials")
			target := filepath.Join(targetHome, "credentials")
			linkCalls := 0
			ops := kimiCredentialStoreOps{
				rename: os.Rename,
				symlink: func(oldPath, newPath string) error {
					linkCalls++
					if oldPath != source || newPath != target {
						t.Fatalf("link paths did not identify the fixture stores")
					}
					// Model successful link creation with an empty directory.
					// This tests selection and the caller's early return, not
					// native sharing or rotation through an actual symlink.
					return os.Mkdir(newPath, 0o700)
				},
			}
			if err := seedKimiCredentialsWithOps(targetHome, ops); err != nil {
				t.Fatalf("seed error = %v", err)
			}
			if linkCalls != 1 {
				t.Errorf("link attempts = %d, want 1 for the configured OAuth login", linkCalls)
			}
			assertKimiRecoveryFiles(t, target, nil)
			assertKimiRecoveryFiles(t, source, files)
			assertKimiScopedFile(t, filepath.Join(sourceHome, "config.toml"), kimiScopedTestConfig)
		})
	}
}

func TestShareKimiCredentialStoreConfiguredOAuthEligibility(t *testing.T) {
	scoped := "{\"access_token\":\"synthetic-scoped-access\"}"
	legacy := "{\"refresh_token\":\"synthetic-legacy-refresh\"}"
	for _, tc := range []struct {
		name       string
		config     string
		files      map[string]string
		wantShared bool
	}{
		{
			name:       "legacy credential without config",
			files:      map[string]string{"kimi-code.json": legacy},
			wantShared: true,
		},
		{
			name:       "legacy credential despite malformed config",
			config:     "[malformed",
			files:      map[string]string{"kimi-code.json": legacy},
			wantShared: true,
		},
		{
			name:       "configured scoped credential despite unusable default",
			config:     kimiScopedTestConfig,
			files:      map[string]string{"scoped-login.json": scoped, "kimi-code.json": "malformed synthetic JSON"},
			wantShared: true,
		},
		{
			name:       "one usable reference among missing references",
			config:     kimiScopedTestConfig + "\n[providers.missing.oauth]\nkey = \"oauth/missing\"\n",
			files:      map[string]string{"scoped-login.json": scoped},
			wantShared: true,
		},
		{
			name:  "unreferenced scoped credential",
			files: map[string]string{"scoped-login.json": scoped},
		},
		{
			name:   "missing referenced credential",
			config: kimiScopedTestConfig,
			files:  map[string]string{"unrelated.json": scoped},
		},
		{
			name:   "empty referenced credential",
			config: kimiScopedTestConfig,
			files:  map[string]string{"scoped-login.json": "{\"access_token\":\"\",\"refresh_token\":\"\"}"},
		},
		{
			name:   "API key config without OAuth reference",
			config: "[providers.scoped]\napi_key = \"synthetic-config-key\"\n",
			files:  map[string]string{"scoped-login.json": scoped},
		},
		{
			name:   "malformed config without legacy login",
			config: "[malformed",
			files:  map[string]string{"scoped-login.json": scoped},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceHome, targetHome := newKimiScopedFixture(t, tc.config, tc.files)
			target := filepath.Join(targetHome, "credentials")
			linkCalls := 0
			shared, err := shareKimiCredentialStore(sourceHome, targetHome, kimiCredentialStoreOps{
				rename: os.Rename,
				symlink: func(_, newPath string) error {
					linkCalls++
					return os.Mkdir(newPath, 0o700) // Synthetic success only.
				},
			})
			if err != nil {
				t.Fatalf("share error = %v", err)
			}
			if shared != tc.wantShared {
				t.Errorf("shared = %v, want %v", shared, tc.wantShared)
			}
			wantCalls := 0
			if tc.wantShared {
				wantCalls = 1
			}
			if linkCalls != wantCalls {
				t.Errorf("link attempts = %d, want %d", linkCalls, wantCalls)
			}
			if shared {
				assertKimiRecoveryFiles(t, target, nil)
			} else if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("ineligible source created a target: %v", err)
			}
			assertKimiRecoveryFiles(t, filepath.Join(sourceHome, "credentials"), tc.files)
		})
	}
}

// Copy fallback is deliberately narrower than directory sharing: it copies
// only config references and refreshes them only when the source is newer.
// Fake token changes demonstrate independent stores, not a provider refresh.
func TestSeedKimiCredentialsScopedCopyRotation(t *testing.T) {
	const initial = "{\"refresh_token\":\"synthetic-initial\"}"
	const sourceRotated = "{\"refresh_token\":\"synthetic-source-rotated\"}"
	const targetRotated = "{\"refresh_token\":\"synthetic-target-rotated\"}"
	const equalTimeSource = "{\"refresh_token\":\"synthetic-source-same-time\"}"
	files := map[string]string{
		"scoped-login.json": initial,
		"unrelated.json":    "{\"refresh_token\":\"synthetic-unreferenced\"}",
	}
	sourceHome, targetHome := newKimiScopedFixture(t, kimiScopedTestConfig, files)
	sourcePath := filepath.Join(sourceHome, "credentials", "scoped-login.json")
	targetPath := filepath.Join(targetHome, "credentials", "scoped-login.json")
	linkCalls := 0
	ops := kimiCredentialStoreOps{
		rename: os.Rename,
		symlink: func(_, _ string) error {
			linkCalls++
			return errors.New("synthetic unavailable directory link")
		},
	}
	seed := func() {
		t.Helper()
		if err := seedKimiCredentialsWithOps(targetHome, ops); err != nil {
			t.Fatalf("seed error = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(targetHome, "credentials.seeded")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copy fallback left a recovery sidecar: %v", err)
		}
	}
	writeAt := func(path, body string, stamp time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	assertKimiRecoveryFiles(t, filepath.Join(targetHome, "credentials"), map[string]string{"scoped-login.json": initial})
	stamp := time.Unix(1_700_000_000, 0)
	writeAt(targetPath, initial, stamp)
	writeAt(sourcePath, sourceRotated, stamp.Add(time.Hour))
	assertKimiScopedFile(t, targetPath, initial)
	seed()
	assertKimiScopedFile(t, targetPath, sourceRotated)
	assertKimiScopedFile(t, sourcePath, sourceRotated)

	writeAt(targetPath, targetRotated, stamp.Add(2*time.Hour))
	assertKimiScopedFile(t, sourcePath, sourceRotated)
	seed()
	assertKimiScopedFile(t, targetPath, targetRotated)
	assertKimiScopedFile(t, sourcePath, sourceRotated)

	writeAt(sourcePath, equalTimeSource, stamp.Add(2*time.Hour))
	seed()
	assertKimiScopedFile(t, targetPath, targetRotated)
	assertKimiScopedFile(t, sourcePath, equalTimeSource)
	if linkCalls != 4 {
		t.Errorf("link attempts = %d, want 4 before copy fallback", linkCalls)
	}
	files["scoped-login.json"] = equalTimeSource
	assertKimiRecoveryFiles(t, filepath.Join(sourceHome, "credentials"), files)
	assertKimiRecoveryFiles(t, filepath.Join(targetHome, "credentials"), map[string]string{"scoped-login.json": targetRotated})
}

func TestSeedKimiCredentialsScopedFallbackExcludesUnreferencedDefault(t *testing.T) {
	files := map[string]string{
		"scoped-login.json": "{\"refresh_token\":\"synthetic-scoped-refresh\"}",
		"kimi-code.json":    "{\"access_token\":\"synthetic-unrelated-default\"}",
	}
	sourceHome, targetHome := newKimiScopedFixture(t, kimiScopedTestConfig, files)
	linkCalls := 0
	err := seedKimiCredentialsWithOps(targetHome, kimiCredentialStoreOps{
		rename: os.Rename,
		symlink: func(_, _ string) error {
			linkCalls++
			return errors.New("synthetic unavailable directory link")
		},
	})
	if err != nil {
		t.Fatalf("seed error = %v", err)
	}
	if linkCalls != 1 {
		t.Errorf("link attempts = %d, want 1", linkCalls)
	}
	assertKimiRecoveryFiles(t, filepath.Join(targetHome, "credentials"), map[string]string{
		"scoped-login.json": files["scoped-login.json"],
	})
	assertKimiRecoveryFiles(t, filepath.Join(sourceHome, "credentials"), files)
}

func TestSeedKimiCredentialsScopedConfigErrorsRemainVisible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		files  map[string]string
		want   string
	}{
		{
			name:   "malformed config",
			config: "[malformed",
			files:  map[string]string{"scoped-login.json": "{\"refresh_token\":\"synthetic-refresh\"}"},
			want:   "read source Kimi config",
		},
		{
			name:   "malformed referenced credential",
			config: kimiScopedTestConfig,
			files:  map[string]string{"scoped-login.json": "malformed synthetic JSON"},
			want:   "read source Kimi credentials",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceHome, targetHome := newKimiScopedFixture(t, tc.config, tc.files)
			ops := kimiCredentialStoreOps{
				rename: func(_, _ string) error {
					t.Fatal("ineligible source attempted a store move")
					return nil
				},
				symlink: func(_, _ string) error {
					t.Fatal("ineligible source attempted a link")
					return nil
				},
			}
			err := seedKimiCredentialsWithOps(targetHome, ops)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("seed error = %v, want %q", err, tc.want)
			}
			if _, err := os.Lstat(filepath.Join(targetHome, "credentials")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("source error created target credentials: %v", err)
			}
			assertKimiRecoveryFiles(t, filepath.Join(sourceHome, "credentials"), tc.files)
		})
	}
}

func newKimiScopedFixture(t *testing.T, config string, files map[string]string) (string, string) {
	t.Helper()
	sourceHome, targetHome := t.TempDir(), t.TempDir()
	t.Setenv(kimiCodeHomeEnv, sourceHome)
	t.Setenv("KIMI_SHARE_DIR", sourceHome)
	writeKimiRecoveryFiles(t, filepath.Join(sourceHome, "credentials"), files)
	if config != "" {
		if err := os.WriteFile(filepath.Join(sourceHome, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return sourceHome, targetHome
}

func assertKimiScopedFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("synthetic credential/config contents changed unexpectedly")
	}
}
