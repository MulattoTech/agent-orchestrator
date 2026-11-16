//go:build windows

package browserstream

import (
	"net"

	"github.com/Microsoft/go-winio"
	"github.com/aoagents/agent-orchestrator/backend/internal/localipc"
)

func Listen(runFilePath string) (net.Listener, string, error) {
	name, err := localipc.PipeName(runFilePath, "browser-stream")
	if err != nil {
		return nil, "", err
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;OW)"})
	if err != nil {
		return nil, "", err
	}
	return ln, name, nil
}
