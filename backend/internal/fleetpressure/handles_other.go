//go:build !windows

package fleetpressure

import (
	"context"
	"errors"
)

// Other platforms keep handle pressure unknown until a native sampler is
// provided. This blocks automatic expansion, but allows the bounded baseline.
func readProcessTreeHandles(context.Context) (int, error) {
	return 0, errors.New("process tree handles unavailable on this platform")
}
