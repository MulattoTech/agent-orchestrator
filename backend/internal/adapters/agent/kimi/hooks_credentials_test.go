package kimi

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestSeedKimiCredentialsStopsWhenStoreRestoreFails(t *testing.T) {
	restoreFailure := errors.New("synthetic restore failure")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "restore error", err: restoreFailure},
		{name: "moved store missing", err: os.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKimiRecoveryFixture(t, true)
			target := filepath.Join(f.targetHome, "credentials")
			aside := target + ".seeded"
			restoreCalls := 0
			ops := kimiCredentialStoreOps{
				rename: func(oldPath, newPath string) error {
					if oldPath == aside && newPath == target {
						restoreCalls++
						return tc.err
					}
					return os.Rename(oldPath, newPath)
				},
				symlink: func(_, _ string) error {
					return errors.New("synthetic link failure")
				},
			}

			err := seedKimiCredentialsWithOps(f.targetHome, ops)
			if !errors.Is(err, tc.err) {
				t.Errorf("seed error = %v, want wrapped restore error %v", err, tc.err)
			}
			if restoreCalls != 1 {
				t.Errorf("restore calls = %d, want 1", restoreCalls)
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("copy fallback recreated the target after failed restoration: %v", err)
			}
			assertKimiRecoveryFiles(t, aside, f.targetFiles)
			assertKimiRecoveryFiles(t, filepath.Join(f.sourceHome, "credentials"), f.sourceFiles)
		})
	}
}

func TestSeedKimiCredentialsCopiesAfterUnavailableLink(t *testing.T) {
	f := newKimiRecoveryFixture(t, false)
	ops := kimiCredentialStoreOps{
		rename: os.Rename,
		symlink: func(_, _ string) error {
			return errors.New("synthetic link failure")
		},
	}

	if err := seedKimiCredentialsWithOps(f.targetHome, ops); err != nil {
		t.Fatalf("seed error = %v, want copy fallback", err)
	}
	assertKimiRecoveryFiles(t, filepath.Join(f.targetHome, "credentials"), f.sourceFiles)
	assertKimiRecoveryFiles(t, filepath.Join(f.sourceHome, "credentials"), f.sourceFiles)
	if _, err := os.Stat(filepath.Join(f.targetHome, "credentials.seeded")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unexpected set-aside store after copy fallback: %v", err)
	}
}

func TestShareKimiCredentialStorePreservesRecoveryPaths(t *testing.T) {
	moveFailure := errors.New("synthetic set-aside failure")
	for _, tc := range []struct {
		name           string
		hasTarget      bool
		moveErr        error
		linkSucceeds   bool
		wantShared     bool
		wantTargetData bool
	}{
		{name: "failed link restores existing store", hasTarget: true, wantTargetData: true},
		{name: "failed link without existing store"},
		{name: "failed set-aside keeps existing store", hasTarget: true, moveErr: moveFailure, wantTargetData: true},
		{name: "synthetic successful link", hasTarget: true, linkSucceeds: true, wantShared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKimiRecoveryFixture(t, tc.hasTarget)
			target := filepath.Join(f.targetHome, "credentials")
			aside := target + ".seeded"
			linkCalls := 0
			ops := kimiCredentialStoreOps{
				rename: func(oldPath, newPath string) error {
					if oldPath == target && newPath == aside && tc.moveErr != nil {
						return tc.moveErr
					}
					return os.Rename(oldPath, newPath)
				},
				symlink: func(_, newPath string) error {
					linkCalls++
					if !tc.linkSucceeds {
						return errors.New("synthetic link failure")
					}
					// Model only a successful link operation; this is a plain
					// fixture directory, not evidence of native symlink support.
					return os.Mkdir(newPath, 0o700)
				},
			}

			shared, err := shareKimiCredentialStore(f.sourceHome, f.targetHome, ops)
			if shared != tc.wantShared {
				t.Errorf("shared = %v, want %v", shared, tc.wantShared)
			}
			if !errors.Is(err, tc.moveErr) {
				t.Errorf("share error = %v, want %v", err, tc.moveErr)
			}
			wantLinkCalls := 1
			if tc.moveErr != nil {
				wantLinkCalls = 0
			}
			if linkCalls != wantLinkCalls {
				t.Errorf("link calls = %d, want %d", linkCalls, wantLinkCalls)
			}
			switch {
			case tc.wantTargetData:
				assertKimiRecoveryFiles(t, target, f.targetFiles)
			case tc.linkSucceeds:
				assertKimiRecoveryFiles(t, target, nil)
			default:
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("unexpected target store: %v", err)
				}
			}
			if _, err := os.Stat(aside); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unexpected set-aside store: %v", err)
			}
			assertKimiRecoveryFiles(t, filepath.Join(f.sourceHome, "credentials"), f.sourceFiles)
		})
	}
}

func TestSeedKimiCredentialsPreservesPendingRecoveryOnRetry(t *testing.T) {
	for _, scopedSource := range []bool{false, true} {
		name := "eligible source"
		if scopedSource {
			name = "source changed to scoped configuration"
		}
		t.Run(name, func(t *testing.T) {
			f := newKimiRecoveryFixture(t, true)
			target := filepath.Join(f.targetHome, "credentials")
			aside := target + ".seeded"
			restoreFailure := errors.New("synthetic restore failure")
			ops := kimiCredentialStoreOps{
				rename: func(oldPath, newPath string) error {
					if oldPath == aside && newPath == target {
						return restoreFailure
					}
					return os.Rename(oldPath, newPath)
				},
				symlink: func(_, _ string) error {
					return errors.New("synthetic link failure")
				},
			}
			if err := seedKimiCredentialsWithOps(f.targetHome, ops); !errors.Is(err, restoreFailure) {
				t.Fatalf("first seed error = %v, want wrapped restore failure", err)
			}
			assertKimiRecoveryFiles(t, aside, f.targetFiles)

			if scopedSource {
				// Switch from a legacy login to a configured scoped login.
				// Pending recovery must still block another seed attempt.
				if err := os.Remove(filepath.Join(f.sourceHome, "credentials", "kimi-code.json")); err != nil {
					t.Fatal(err)
				}
				f.sourceFiles = map[string]string{
					"scoped-source.json": "{\"access_token\":\"synthetic-scoped-source\"}",
				}
				writeKimiRecoveryFiles(t, filepath.Join(f.sourceHome, "credentials"), f.sourceFiles)
				config := "[providers.kimi.oauth]\nstorage = \"file\"\nkey = \"oauth/scoped-source\"\n"
				if err := os.WriteFile(filepath.Join(f.sourceHome, "config.toml"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := seedKimiCredentialsWithOps(f.targetHome, ops)
			if err == nil || !strings.Contains(err.Error(), "credential recovery is pending") {
				t.Errorf("retry error = %v, want pending-recovery error", err)
			} else if strings.Contains(err.Error(), f.sourceHome) || strings.Contains(err.Error(), f.targetHome) {
				t.Errorf("pending-recovery error disclosed a fixture path")
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("retry recreated the target before recovery: %v", err)
			}
			assertKimiRecoveryFiles(t, filepath.Join(f.sourceHome, "credentials"), f.sourceFiles)
			assertKimiRecoveryFiles(t, aside, f.targetFiles)
		})
	}
}

func TestSeedKimiCredentialsHandlesPreexistingRecoverySidecar(t *testing.T) {
	for _, kind := range []string{"directory", "file", "dangling link", "same home"} {
		t.Run(kind, func(t *testing.T) {
			f := newKimiRecoveryFixture(t, false)
			if kind == "same home" {
				f.targetHome = f.sourceHome
			}
			target := filepath.Join(f.targetHome, "credentials")
			aside := target + ".seeded"
			const retained = "synthetic retained recovery data"
			linkTarget := filepath.Join(t.TempDir(), "missing")
			switch kind {
			case "directory":
				writeKimiRecoveryFiles(t, aside, f.targetFiles)
			case "dangling link":
				if err := os.Symlink(linkTarget, aside); err != nil {
					if runtime.GOOS == "windows" &&
						(errors.Is(err, syscall.Errno(1314)) || errors.Is(err, syscall.Errno(50))) {
						t.Skipf("temporary dangling-link fixture is unavailable: %v", err)
					}
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(aside, []byte(retained), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ops := kimiCredentialStoreOps{
				rename: os.Rename,
				symlink: func(_, _ string) error {
					return errors.New("synthetic link failure")
				},
			}

			err := seedKimiCredentialsWithOps(f.targetHome, ops)
			if kind == "same home" {
				if err != nil {
					t.Errorf("same-home seed error = %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "credential recovery is pending") {
					t.Errorf("seed error = %v, want pending-recovery error", err)
				}
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("seed created a target with pending recovery: %v", err)
				}
			}
			switch kind {
			case "directory":
				assertKimiRecoveryFiles(t, aside, f.targetFiles)
			case "dangling link":
				if got, err := os.Readlink(aside); err != nil || got != linkTarget {
					t.Errorf("saved dangling link changed: target = %q, error = %v", got, err)
				}
			default:
				data, err := os.ReadFile(aside)
				if err != nil || string(data) != retained {
					t.Errorf("saved recovery file changed: %v", err)
				}
			}
			assertKimiRecoveryFiles(t, filepath.Join(f.sourceHome, "credentials"), f.sourceFiles)
		})
	}
}

type kimiRecoveryFixture struct {
	sourceHome  string
	targetHome  string
	sourceFiles map[string]string
	targetFiles map[string]string
}

func newKimiRecoveryFixture(t *testing.T, withTarget bool) kimiRecoveryFixture {
	t.Helper()
	f := kimiRecoveryFixture{
		sourceHome: t.TempDir(),
		targetHome: t.TempDir(),
		sourceFiles: map[string]string{
			"kimi-code.json": "{\"access_token\":\"synthetic-source-token\"}",
		},
		targetFiles: map[string]string{
			"kimi-code.json": "{\"refresh_token\":\"synthetic-retained-token\"}",
			"scoped.json":    "{\"refresh_token\":\"synthetic-scoped-token\"}",
		},
	}
	t.Setenv(kimiCodeHomeEnv, f.sourceHome)
	t.Setenv("KIMI_SHARE_DIR", f.sourceHome)
	writeKimiRecoveryFiles(t, filepath.Join(f.sourceHome, "credentials"), f.sourceFiles)
	if withTarget {
		writeKimiRecoveryFiles(t, filepath.Join(f.targetHome, "credentials"), f.targetFiles)
	}
	return f
}

func writeKimiRecoveryFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertKimiRecoveryFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture store: %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("fixture store has %d entries, want %d", len(entries), len(want))
	}
	for name, expected := range want {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("read fixture %s: %v", name, err)
			continue
		}
		if string(data) != expected {
			t.Errorf("fixture %s changed", name)
		}
	}
}
