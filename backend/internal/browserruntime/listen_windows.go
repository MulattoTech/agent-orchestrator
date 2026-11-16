//go:build windows

package browserruntime

import (
	"net"

	"github.com/Microsoft/go-winio"
	"github.com/aoagents/agent-orchestrator/backend/internal/localipc"
)

func pipeNameFromRunFile(runFilePath string) (string, error) {
	return localipc.PipeName(runFilePath, "browser-runtime-browser")
}

// Listen creates the local daemon-to-Electron browser bridge listener.
func Listen(runFilePath string) (net.Listener, string, error) {
	return ListenNamed(runFilePath, "browser")
}

// ListenNamed keeps each runtime in a distinct service domain under the complete
// physical state root. Clients consume the exact published locator.
func ListenNamed(runFilePath, runtimeName string) (net.Listener, string, error) {
	purpose := "browser-runtime-" + runtimeName
	name, err := localipc.PipeName(runFilePath, purpose)
	if err != nil {
		return nil, "", err
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{
		// Protected DACL: the creating owner and LocalSystem only.
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;OW)",
	})
	if err != nil {
		return nil, "", err
	}
	return ln, name, nil
}
