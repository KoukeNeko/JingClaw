package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// StartOptions describes a program to run.
type StartOptions struct {
	// SessionID is what the process belongs to. Closing that session ends it.
	SessionID string

	Program string
	Args    []string

	// Dir is where it runs, already resolved and checked by the caller. This
	// package does not know what a workspace is.
	Dir string

	// Env is the environment it receives. Empty means an empty environment,
	// not the daemon's: inheriting would hand every program the API keys.
	Env []string

	// Terminal asks for a pseudo-terminal.
	//
	// Worth asking for when the program decides what to do by looking at what
	// it is attached to: many buffer their output into blocks when they see a
	// pipe, and some refuse to prompt at all. Worth not asking for when the
	// output is going to be read rather than watched, because a terminal
	// brings escape sequences with it.
	Terminal bool

	// Columns and Rows size the terminal. Zero uses a sensible default; a
	// program that draws a table reads these.
	Columns, Rows int
}

// Start runs a program and returns once it is going.
//
// It does not wait for the program to finish — that is the point of it. What
// it does wait for is the program actually starting, so a name that does not
// exist is an error the caller sees rather than a process id that dies a
// moment later.
func (m *Manager) Start(options StartOptions) (State, error) {
	if options.Program == "" {
		return State{}, errors.New("process: no program to run")
	}
	if options.SessionID == "" {
		// A process with no session belongs to nothing, so nothing would ever
		// end it. Refused rather than defaulted.
		return State{}, errors.New("process: a process must belong to a session")
	}

	command := exec.Command(options.Program, options.Args...)
	command.Dir = options.Dir
	command.Env = options.Env
	configureProcessGroup(command)

	one := &handle{
		id:         m.newID(),
		session:    options.SessionID,
		program:    options.Program,
		args:       append([]string{}, options.Args...),
		command:    command,
		buffer:     newRingBuffer(m.bufferBytes()),
		isTerminal: options.Terminal,
		startedAt:  m.now(),
		done:       make(chan struct{}),
	}

	if err := m.launch(one, options); err != nil {
		return State{}, err
	}

	m.mu.Lock()
	m.running[one.id] = one
	m.bySession[options.SessionID] = append(m.bySession[options.SessionID], one.id)
	m.mu.Unlock()

	go m.reap(one)

	return one.state(m.now()), nil
}

// launch starts the program, with a terminal or with pipes.
func (m *Manager) launch(one *handle, options StartOptions) error {
	if options.Terminal {
		terminal, err := startWithTerminal(one.command, options.Columns, options.Rows)
		if err != nil {
			return err
		}
		if terminal != nil {
			one.terminal = terminal
			one.input = terminal
			go func() { _, _ = io.Copy(one.buffer, terminal) }()
			return nil
		}
		// No terminal on this platform. Recorded honestly rather than
		// reported as one: a caller told it has a terminal will wait for a
		// prompt that a block-buffered program is never going to flush.
		one.isTerminal = false
	}

	stdin, err := one.command.StdinPipe()
	if err != nil {
		return fmt.Errorf("process: connect stdin: %w", err)
	}
	one.input = stdin

	// Both streams into one buffer, in the order they arrive. A program's
	// error output belongs beside the line it followed; separated, a reader
	// has to guess where a failure happened.
	one.command.Stdout = one.buffer
	one.command.Stderr = one.buffer

	if err := one.command.Start(); err != nil {
		return fmt.Errorf("process: start %s: %w", options.Program, err)
	}
	return nil
}

// reap waits for the program and records how it ended.
func (m *Manager) reap(one *handle) {
	err := one.command.Wait()

	one.mu.Lock()
	one.finished = true
	one.waitErr = err
	var exit *exec.ExitError
	switch {
	case err == nil:
		one.exitCode = 0
	case errors.As(err, &exit):
		one.exitCode = exit.ExitCode()
	default:
		// Something other than the program failing — the pipe, the wait
		// itself. Not zero, because zero is the one value that means it
		// worked.
		one.exitCode = -1
	}
	one.mu.Unlock()

	if one.terminal != nil {
		_ = one.terminal.Close()
	}
	close(one.done)
}

// Get is the current state of one process.
func (m *Manager) Get(id ID) (State, error) {
	one, err := m.handleFor(id)
	if err != nil {
		return State{}, err
	}
	return one.state(m.now()), nil
}

// List is every process of one session, oldest first.
func (m *Manager) List(sessionID string) []State {
	m.mu.Lock()
	ids := append([]ID{}, m.bySession[sessionID]...)
	handles := make([]*handle, 0, len(ids))
	for _, id := range ids {
		if one, ok := m.running[id]; ok {
			handles = append(handles, one)
		}
	}
	m.mu.Unlock()

	now := m.now()
	states := make([]State, 0, len(handles))
	for _, one := range handles {
		states = append(states, one.state(now))
	}
	return states
}

// Write sends input to a program.
func (m *Manager) Write(id ID, input string) error {
	one, err := m.handleFor(id)
	if err != nil {
		return err
	}

	one.mu.Lock()
	finished := one.finished
	one.mu.Unlock()
	if finished {
		return ErrClosed
	}

	if _, err := io.WriteString(one.input, input); err != nil {
		return fmt.Errorf("process: write to %s: %w", id, err)
	}
	return nil
}

// Read answers with output from offset onwards.
func (m *Manager) Read(id ID, offset int64) (output string, next int64, skipped int64, err error) {
	one, err := m.handleFor(id)
	if err != nil {
		return "", 0, 0, err
	}

	bytes, next, skipped := one.buffer.read(offset)
	return string(bytes), next, skipped, nil
}

// Wait blocks until the program ends or the context does.
//
// Used by a caller that started something expected to finish soon — a
// migration, an installer — and wants its exit code rather than to poll for
// it.
func (m *Manager) Wait(ctx context.Context, id ID) (State, error) {
	one, err := m.handleFor(id)
	if err != nil {
		return State{}, err
	}

	select {
	case <-ctx.Done():
		return one.state(m.now()), ctx.Err()
	case <-one.done:
		return one.state(m.now()), nil
	}
}

// Stop ends a program and waits for it to go.
//
// Asked first, killed after a grace period. A program that is asked politely
// gets to write out what it was holding; one that ignores the request is not
// something to wait on forever, because a caller who said stop has already
// decided.
func (m *Manager) Stop(id ID) (State, error) {
	one, err := m.handleFor(id)
	if err != nil {
		return State{}, err
	}

	one.mu.Lock()
	finished := one.finished
	one.mu.Unlock()
	if finished {
		return one.state(m.now()), nil
	}

	if err := terminateGroup(one.command); err != nil {
		return State{}, fmt.Errorf("process: stop %s: %w", id, err)
	}

	select {
	case <-one.done:
	case <-time.After(stopGrace):
		if one.command.Process != nil {
			_ = one.command.Process.Kill()
		}
		<-one.done
	}

	return one.state(m.now()), nil
}

// CloseSession ends every process belonging to a session.
//
// Called when a session is closed and when the daemon stops. A process nobody
// can name any more is one that keeps a port bound and a file locked until
// somebody notices at the machine.
func (m *Manager) CloseSession(sessionID string) []State {
	m.mu.Lock()
	ids := append([]ID{}, m.bySession[sessionID]...)
	delete(m.bySession, sessionID)
	m.mu.Unlock()

	stopped := make([]State, 0, len(ids))
	for _, id := range ids {
		state, err := m.Stop(id)
		if err != nil {
			continue
		}
		stopped = append(stopped, state)

		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
	}
	return stopped
}

// CloseAll ends everything this manager holds, for daemon shutdown.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]string, 0, len(m.bySession))
	for session := range m.bySession {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()

	for _, session := range sessions {
		m.CloseSession(session)
	}
}

// Resize tells a program its terminal changed shape.
func (m *Manager) Resize(id ID, columns, rows int) error {
	one, err := m.handleFor(id)
	if err != nil {
		return err
	}
	if one.terminal == nil {
		return errors.New("process: this process has no terminal to resize")
	}
	return resizeTerminal(one.terminal, columns, rows)
}

func (m *Manager) handleFor(id ID) (*handle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	one, ok := m.running[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return one, nil
}
