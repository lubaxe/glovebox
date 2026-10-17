package system

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Process is a command running in the background, like an SSH master
// connection or an rclone server.
type Process interface {
	// Exited is closed when the process has exited.
	Exited() <-chan struct{}
	// Output is what it has written so far.
	Output() string
	// Stop kills the process and waits for it.
	Stop() error
}

// Starter starts background processes.
type Starter func(name string, args ...string) (Process, error)

// Start starts a background process. It isn't tied to a context: it runs
// until Stop.
func Start(name string, args ...string) (Process, error) {
	p := &process{exited: make(chan struct{})}
	p.cmd = exec.Command(name, args...)
	p.cmd.Stdout = &p.out
	p.cmd.Stderr = &p.out
	// don't wait for children that keep the output open
	p.cmd.WaitDelay = time.Second
	if err := p.cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

type process struct {
	cmd    *exec.Cmd
	out    lockedBuffer
	exited chan struct{}
	err    error
}

func (p *process) Exited() <-chan struct{} { return p.exited }
func (p *process) Output() string          { return strings.TrimSpace(p.out.String()) }

func (p *process) Stop() error {
	select {
	case <-p.exited:
		return nil
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.exited
	return nil
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 64<<10 { // keep the start, which has the errors
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
