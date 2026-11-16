//go:build windows

package conpty

import (
	"errors"
	"fmt"
	"os"
	"sync"

	gopty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"
)

// conptyConn is the real ptyConn implementation backed by go-pty's ConPty
// (Windows ConPTY API). Only compiled on Windows.
type conptyConn struct {
	pty     gopty.ConPty
	process *ownedConPTYProcess

	doneOnce      sync.Once
	doneC         chan struct{}
	consoleMu     sync.RWMutex
	consoleClosed bool
	disposeOnce   sync.Once
	disposeErr    error
	exitCode      int
	exited        bool
	exitMu        sync.Mutex
}

// newConPTY creates a ConPTY session running shellCmd in cwd with shellArgs.
// It starts the process and returns a ptyConn ready for use.
func newConPTY(cwd, shellCmd string, shellArgs []string, cols, rows uint16) (ptyConn, error) {
	// go-pty's New() returns a ConPty on Windows.
	p, err := gopty.New()
	if err != nil {
		return nil, fmt.Errorf("conpty: create pty: %w", err)
	}
	cp, ok := p.(gopty.ConPty)
	if !ok {
		_ = p.Close()
		return nil, fmt.Errorf("conpty: expected ConPty on windows, got %T", p)
	}

	if err := cp.Resize(int(cols), int(rows)); err != nil {
		_ = cp.Close()
		return nil, fmt.Errorf("conpty: initial resize: %w", err)
	}

	// Process creation atomically assigns a private Job Object before the
	// child runs. The host stays outside the job so daemon loss preserves it.
	process, err := startConPTYJob(windows.Handle(cp.Fd()), cwd, shellCmd, shellArgs, os.Environ())
	if err != nil {
		_ = cp.Close()
		return nil, fmt.Errorf("conpty: start command: %w", err)
	}

	c := &conptyConn{
		pty:     cp,
		process: process,
		doneC:   make(chan struct{}),
	}

	go c.wait()
	return c, nil
}

func (c *conptyConn) wait() {
	code, err := c.process.wait()
	if err != nil {
		code = 1
	}
	// Root completion ends its owned tree as well; detached descendants must
	// not survive a completed terminal workload.
	_ = c.process.closeJob()
	c.exitMu.Lock()
	c.exitCode = code
	c.exited = true
	c.exitMu.Unlock()
	c.doneOnce.Do(func() { close(c.doneC) })

	// Unlike Unix PTYs, ConPTY can keep its output pipe open after the child
	// exits. Close only the pseudoconsole handle here so buffered output can
	// still drain before Read returns EOF. The shared host then follows the
	// same exit path as macOS and Linux and broadcasts the completed status.
	c.closeConsole()
}

func (c *conptyConn) Read(b []byte) (int, error)  { return c.pty.Read(b) }
func (c *conptyConn) Write(b []byte) (int, error) { return c.pty.Write(b) }
func (c *conptyConn) Close() error {
	// End only this invocation's job before closing the console, whose close
	// can otherwise wait on children that ignore terminal EOF.
	jobErr := c.process.closeJob()
	c.closeConsole()
	c.disposeOnce.Do(func() {
		c.disposeErr = errors.Join(
			c.pty.InputPipe().Close(),
			c.pty.OutputPipe().Close(),
		)
	})
	return errors.Join(jobErr, c.disposeErr)
}

func (c *conptyConn) closeConsole() {
	c.consoleMu.Lock()
	defer c.consoleMu.Unlock()
	if c.consoleClosed {
		return
	}
	windows.ClosePseudoConsole(windows.Handle(c.pty.Fd()))
	c.consoleClosed = true
}
func (c *conptyConn) Resize(cols, rows int) error {
	c.consoleMu.RLock()
	defer c.consoleMu.RUnlock()
	if c.consoleClosed {
		return fmt.Errorf("conpty: resize: %w", os.ErrClosed)
	}
	return c.pty.Resize(cols, rows)
}
func (c *conptyConn) Done() <-chan struct{} { return c.doneC }
func (c *conptyConn) PID() int {
	return c.process.pid
}
func (c *conptyConn) ExitCode() (int, bool) {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	return c.exitCode, c.exited
}
