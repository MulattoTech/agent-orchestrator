package main

import (
	"os/exec"
	"syscall"
)

func processCreation() (uint64, error) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, err
	}
	var created, exited, kernel, user syscall.Filetime
	if err = syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}
func prepareDescendant(command *exec.Cmd) {
	// Deliberately detach from the console; ordinary CreateProcess descendants
	// still inherit their parent's Job Object. No BREAKAWAY flag is allowed.
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x8 | 0x200, HideWindow: true}
}
