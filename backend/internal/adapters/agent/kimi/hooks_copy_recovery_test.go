package kimi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKimiCopyFallbackRejectsNonFileCredentialTarget(t *testing.T) {
	stamp := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name string
		time time.Time
	}{
		{"older directory", stamp.Add(-time.Hour)},
		{"same-time directory", stamp},
		{"newer directory", stamp.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKimiContractHomes(t)
			const config = "[providers.only.oauth]\nkey = \"oauth/login\"\n"
			const source = "{\n  \"refresh_token\": \"synthetic-source\", \"metadata\": {\"keep\": true}\n}\n"
			writeKimiContractFile(t, filepath.Join(f.code, "config.toml"), config)
			writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), map[string]string{"login.json": source})
			setKimiCopyTime(t, filepath.Join(f.code, "credentials", "login.json"), stamp)

			targetHome := t.TempDir()
			targetStore := filepath.Join(targetHome, "credentials")
			targetPath := filepath.Join(targetStore, "login.json")
			retained := map[string]string{"retained.json": "{\"refresh_token\":\"synthetic-retained\"}\n"}
			writeKimiRecoveryFiles(t, targetPath, retained)
			setKimiCopyTime(t, targetPath, tc.time)
			linkCalls := 0
			err := seedKimiCredentialsWithOps(targetHome, kimiCredentialStoreOps{
				rename: os.Rename,
				symlink: func(oldPath, newPath string) error {
					linkCalls++
					if oldPath != filepath.Join(f.code, "credentials") || newPath != targetStore {
						t.Fatal("sharing escaped the fixture stores")
					}
					return errors.New("synthetic unavailable link")
				},
			})
			if err == nil {
				t.Errorf("copy fallback accepted a %s as a credential file", tc.name)
			}
			if linkCalls != 1 {
				t.Errorf("link attempts = %d, want one", linkCalls)
			}
			info, statErr := os.Stat(targetPath)
			if statErr != nil || !info.IsDir() || !info.ModTime().Equal(tc.time) {
				t.Fatalf("blocked target directory changed: %v", statErr)
			}
			assertKimiRecoveryFiles(t, targetPath, retained)
			entries, err := os.ReadDir(targetStore)
			if err != nil || len(entries) != 1 || entries[0].Name() != "login.json" || !entries[0].IsDir() {
				t.Fatalf("blocked copy left unexpected target entries: %v", err)
			}
			assertKimiCopyNoSidecar(t, targetHome)
			assertKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), map[string]string{"login.json": source})
			assertKimiScopedFile(t, filepath.Join(f.code, "config.toml"), config)
		})
	}
}

func TestKimiMixedCopyRetriesAfterPartialFailure(t *testing.T) {
	for _, failure := range []string{"malformed later reference", "blocked later target"} {
		t.Run(failure, func(t *testing.T) {
			f := newKimiContractHomes(t)
			const config = "[providers.alpha.oauth]\nkey = \"oauth/first\"\n[providers.beta.oauth]\nkey = \"oauth/second\"\n"
			const first = "{\n \"refresh_token\": \"synthetic-first\", \"extra\": [1, 2]\n}\n"
			const second = "{\n \"access_token\": \"synthetic-second\", \"extra\": {\"kept\": true}\n}\n"
			const older = "{\"refresh_token\":\"synthetic-older-target\"}\n"
			const renewed = "{\n \"refresh_token\": \"synthetic-target-renewed\", \"local\": true\n}\n"
			const interrupted = "{\"refresh_token\":\"synthetic-incomplete"
			files := map[string]string{
				"first.json":     first,
				"second.json":    second,
				"kimi-code.json": "{\"refresh_token\":\"synthetic-unreferenced-default\"}",
				"unrelated.json": "{\"refresh_token\":\"synthetic-unrelated\"}",
			}
			if failure == "malformed later reference" {
				files["second.json"] = "malformed synthetic referenced credential"
			}
			writeKimiContractFile(t, filepath.Join(f.code, "config.toml"), config)
			writeKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
			stamp := time.Unix(1_700_000_000, 0)
			for _, name := range []string{"first.json", "second.json"} {
				setKimiCopyTime(t, filepath.Join(f.code, "credentials", name), stamp)
			}

			targetHome := t.TempDir()
			targetStore := filepath.Join(targetHome, "credentials")
			firstTarget := filepath.Join(targetStore, "first.json")
			secondTarget := filepath.Join(targetStore, "second.json")
			writeKimiContractFile(t, firstTarget, older)
			setKimiCopyTime(t, firstTarget, stamp.Add(-time.Hour))
			if failure == "blocked later target" {
				writeKimiContractFile(t, filepath.Join(secondTarget, "retained.json"), older)
			} else {
				writeKimiContractFile(t, secondTarget, older)
			}
			setKimiCopyTime(t, secondTarget, stamp.Add(-time.Hour))
			linkCalls := 0
			ops := kimiCredentialStoreOps{
				rename: os.Rename,
				symlink: func(oldPath, newPath string) error {
					linkCalls++
					if oldPath != filepath.Join(f.code, "credentials") || newPath != targetStore {
						t.Fatal("sharing escaped the fixture stores")
					}
					return errors.New("synthetic unavailable link")
				},
			}

			err := seedKimiCredentialsWithOps(targetHome, ops)
			if err == nil || !strings.Contains(err.Error(), "second.json") {
				t.Fatalf("later copy failure was not reported: %v", err)
			}
			// This is a failed multi-file copy, not a transactional rollback.
			// The completed first copy and the preexisting second target survive.
			assertKimiScopedFile(t, firstTarget, first)
			if failure == "blocked later target" {
				assertKimiRecoveryFiles(t, secondTarget, map[string]string{"retained.json": older})
			} else {
				assertKimiScopedFile(t, secondTarget, older)
			}
			entries, err := os.ReadDir(targetStore)
			if err != nil || len(entries) != 2 {
				t.Fatalf("failed copy left unexpected temporary/default files: %v", err)
			}
			assertKimiCopyNoSidecar(t, targetHome)
			assertKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)

			// Model a retained temp artifact from an interrupted write and a
			// target login renewed between attempts. No process is interrupted.
			writeKimiContractFile(t, filepath.Join(targetStore, ".ao-tmp-interrupted"), interrupted)
			writeKimiContractFile(t, firstTarget, renewed)
			setKimiCopyTime(t, firstTarget, stamp.Add(2*time.Hour))
			firstInfo, err := os.Stat(firstTarget)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "blocked later target" {
				// Reconcile only the owned blocking fixture after proving that
				// the failed attempt retained all of its bytes.
				if err := os.Remove(filepath.Join(secondTarget, "retained.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(secondTarget); err != nil {
					t.Fatal(err)
				}
			} else {
				files["second.json"] = second
				writeKimiContractFile(t, filepath.Join(f.code, "credentials", "second.json"), second)
				setKimiCopyTime(t, filepath.Join(f.code, "credentials", "second.json"), stamp.Add(time.Hour))
			}
			if err := seedKimiCredentialsWithOps(targetHome, ops); err != nil {
				t.Fatalf("retry after fixture repair failed: %v", err)
			}
			after, err := os.Stat(firstTarget)
			if err != nil || !os.SameFile(firstInfo, after) || !after.ModTime().Equal(firstInfo.ModTime()) {
				t.Fatalf("retry replaced the newer first target: %v", err)
			}
			assertKimiRecoveryFiles(t, targetStore, map[string]string{
				"first.json":          renewed,
				"second.json":         second,
				".ao-tmp-interrupted": interrupted,
			})
			assertKimiCopyNoSidecar(t, targetHome)
			assertKimiRecoveryFiles(t, filepath.Join(f.code, "credentials"), files)
			assertKimiScopedFile(t, filepath.Join(f.code, "config.toml"), config)
			if linkCalls != 2 {
				t.Errorf("link attempts = %d, want one per copy attempt", linkCalls)
			}
		})
	}
}

func setKimiCopyTime(t *testing.T, path string, stamp time.Time) {
	t.Helper()
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("fixture timestamp was not applied: %v", err)
	}
}

func assertKimiCopyNoSidecar(t *testing.T, targetHome string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(targetHome, "credentials.seeded")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy fallback left or changed a recovery sidecar: %v", err)
	}
}
