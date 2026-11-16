//go:build !windows

package conpty

import "errors"

// The OS creation-handle fence in this change is Windows-specific. Existing
// Unix runtime behavior is preserved; it is not claimed as qualified here.
const nativeHostOwnershipRequired = false

func readHostCreation(int) (string, error) { return "", nil }
func openHostProcess(int, string, bool) (ownedHostProcess, error) {
	return nil, errors.New("conpty: Windows host ownership unavailable on this platform")
}
func currentHostIdentity(string) (HostIdentity, error) { return HostIdentity{}, nil }
