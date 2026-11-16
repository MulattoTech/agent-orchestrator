//go:build windows

package supervisor

import (
	"net"

	"github.com/Microsoft/go-winio"
	"github.com/aoagents/agent-orchestrator/backend/internal/localipc"
)

// Listen creates a watchdog listener under the complete physical state root.
// The exact address is published in running.json; no basename/default fallback
// may connect an independently configured AO instance to this supervisor.
func Listen(runFilePath string) (net.Listener, string, error) {
	name, err := localipc.PipeName(runFilePath, "supervise")
	if err != nil {
		return nil, "", err
	}
	ln, err := winio.ListenPipe(name, nil)
	if err != nil {
		return nil, "", err
	}
	return ln, name, nil
}
