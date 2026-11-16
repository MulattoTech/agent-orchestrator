//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

func processCreation() (uint64, error) { return 0, errors.New("native tree fixture requires Windows") }
func prepareDescendant(*exec.Cmd)      {}
