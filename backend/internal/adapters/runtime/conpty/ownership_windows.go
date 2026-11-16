//go:build windows

package conpty

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const nativeHostOwnershipRequired = true

type windowsOwnedHost struct{ handle windows.Handle }

func creationFromHandle(handle windows.Handle) (string, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	stamp := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if stamp == 0 {
		return "", fmt.Errorf("missing process creation time")
	}
	return fmt.Sprintf("%016x", stamp), nil
}

func readHostCreation(pid int) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	return creationFromHandle(handle)
}

// A retained kernel handle, rather than a later PID lookup, is the authority
// for both wait and termination. PID reuse after this open cannot change its
// target. Permission/identity failures never authorize a fallback PID kill.
func openHostProcess(pid int, created string, terminate bool) (ownedHostProcess, error) {
	if pid <= 0 || created == "" {
		return nil, fmt.Errorf("missing host process fence")
	}
	rights := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE)
	if terminate {
		rights |= windows.PROCESS_TERMINATE
	}
	handle, err := windows.OpenProcess(rights, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	actual, err := creationFromHandle(handle)
	if err != nil || actual != created {
		_ = windows.CloseHandle(handle)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("host PID %d creation identity changed", pid)
	}
	return &windowsOwnedHost{handle: handle}, nil
}

func (p *windowsOwnedHost) Alive() (bool, error) {
	result, err := windows.WaitForSingleObject(p.handle, 0)
	if err != nil {
		return false, err
	}
	switch result {
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	case windows.WAIT_OBJECT_0:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait result %d", result)
	}
}
func (p *windowsOwnedHost) Kill() error  { return windows.TerminateProcess(p.handle, 1) }
func (p *windowsOwnedHost) Close() error { return windows.CloseHandle(p.handle) }

func currentHostIdentity(sessionID string) (HostIdentity, error) {
	created, err := readHostCreation(os.Getpid())
	if err != nil {
		return HostIdentity{}, err
	}
	identity := HostIdentity{
		SessionID: sessionID, LaunchID: os.Getenv(hostLaunchIDEnv),
		RuntimeLaunchID: os.Getenv(runtimeLaunchIDEnv),
		PID:             os.Getpid(), CreatedAt: created,
	}
	if !identity.complete() {
		return HostIdentity{}, fmt.Errorf("missing host launch identity")
	}
	return identity, nil
}
