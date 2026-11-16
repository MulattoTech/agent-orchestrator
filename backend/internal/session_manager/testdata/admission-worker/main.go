// This deliberately tiny synthetic worker is compiled only by the offline
// fixtures. It has no agent/provider imports or networking capability.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type treeRecord struct {
	Token     string `json:"token"`
	Role      string `json:"role"`
	PID       int    `json:"pid"`
	ParentPID int    `json:"parentPID"`
	Creation  uint64 `json:"creationTicks"`
}

func main() {
	if os.Getenv("AO_ADMISSION_NATIVE_HELPER") != "1" {
		os.Exit(90)
	}
	ready, token := os.Getenv("AO_ADMISSION_HELPER_READY"), os.Getenv("AO_ADMISSION_HELPER_TOKEN")
	if ready == "" || token == "" {
		os.Exit(91)
	}
	tree := os.Getenv("AO_ADMISSION_HELPER_TREE") == "1"
	role := "root"
	if len(os.Args) > 1 {
		if !tree || len(os.Args) != 2 {
			os.Exit(93)
		}
		switch os.Args[1] {
		case "--ao-offline-tree=child":
			role = "child"
		case "--ao-offline-tree=grandchild":
			role = "grandchild"
		default:
			os.Exit(93)
		}
	}
	if tree {
		if err := runTree(ready, token, role); err != nil {
			os.Exit(94)
		}
		if role != "root" {
			// Detached descendants intentionally ignore stdin/ConPTY EOF. The owned
			// Job Object must end them; the hard deadline also bounds a failed test.
			time.Sleep(90 * time.Second)
			return
		}
	}
	if err := os.WriteFile(ready, []byte(token), 0600); err != nil {
		os.Exit(92)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
	}
}

func runTree(ready, token, role string) error {
	if !filepath.IsAbs(ready) || filepath.Base(ready) != "worker.ready" || !regexp.MustCompile("^[0-9a-f]{32}$").MatchString(token) {
		return errors.New("invalid explicit tree fixture identity")
	}
	root := filepath.Dir(ready)
	marker, err := os.ReadFile(filepath.Join(root, "owner-token"))
	if err != nil || string(marker) != token {
		return errors.New("tree fixture owner mismatch")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if role != "root" {
		created, err := processCreation()
		if err != nil {
			return err
		}
		record := treeRecord{token, role, os.Getpid(), os.Getppid(), created}
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(root, "tree-"+role+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(data)
		syncErr := f.Sync()
		closeErr := f.Close()
		if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
			return err
		}
	}
	next := ""
	switch role {
	case "root":
		next = "child"
	case "child":
		next = "grandchild"
	case "grandchild":
		return nil
	}
	command := exec.Command(self, "--ao-offline-tree="+next)
	command.Dir = root
	command.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"), "WINDIR=" + os.Getenv("SystemRoot"),
		"PATH=" + filepath.Join(os.Getenv("SystemRoot"), "System32"), "GOMAXPROCS=1",
		"AO_ADMISSION_NATIVE_HELPER=1", "AO_ADMISSION_HELPER_TREE=1",
		"AO_ADMISSION_HELPER_READY=" + ready, "AO_ADMISSION_HELPER_TOKEN=" + token,
	}
	prepareDescendant(command)
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	// Never accept an executable or a role from external arguments beyond this
	// fixed self path and the two literal choices above.
	if strings.TrimSpace(next) == "" {
		return errors.New("missing descendant role")
	}
	return nil
}
