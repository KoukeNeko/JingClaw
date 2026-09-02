// Package process owns programs that outlive the run that started them.
//
// exec_command waits, which covers a build and a test suite and most of what a
// coding agent does. What it cannot do is a dev server, a REPL, an installer
// that asks a question, or an ssh session — anything where the useful part is
// that the program is still there afterwards, and where the next thing the
// agent does depends on what it printed a moment ago.
//
// Bound to a session rather than to a run. A run that starts a server and then
// ends must not take the server with it, or starting one is pointless; a
// process that outlives the session it belongs to is a leak nobody can see, so
// closing the session ends them and so does stopping the daemon.
package process

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// terminalFile is a pseudo-terminal: one connection carrying both directions.
type terminalFile interface {
	io.ReadWriteCloser
}

// ID identifies one running program within a session.
type ID string

// ErrNotFound is returned for an id this manager does not have.
//
// A distinct error rather than a nil result: "which process" is a thing a
// model gets wrong, and it should be told so rather than be handed an empty
// answer that reads like a program producing nothing.
var ErrNotFound = errors.New("process: no such process")

// ErrClosed is returned once a process has ended.
var ErrClosed = errors.New("process: the program has ended")

// State is what a caller is told about a process.
type State struct {
	ID      ID
	Program string
	Args    []string

	// PID is the operating system's number for it, so somebody at the machine
	// can find it themselves.
	PID int

	StartedAt time.Time

	// Running is false once it has ended, whatever the reason.
	Running bool

	// ExitCode is meaningful only once it has ended. Negative means it was
	// killed by a signal rather than returning.
	ExitCode int

	// Terminal says whether it was given a pseudo-terminal, because it
	// changes what the program does: many buffer their output when they
	// decide they are talking to a pipe, and some refuse to prompt at all.
	Terminal bool

	// OutputDropped is how many bytes were lost to the buffer's limit.
	// Reported rather than hidden: output silently missing its middle is how
	// a reader concludes a build succeeded.
	OutputDropped int64
}

// Manager owns the running processes of one daemon.
type Manager struct {
	// BufferBytes is how much output is kept per process. Zero uses the
	// default.
	BufferBytes int

	// NewID names a process. Injected so a test can predict them.
	NewID func() ID

	// Now is the clock, injected for the same reason.
	Now func() time.Time

	mu      sync.Mutex
	running map[ID]*handle

	// bySession is what makes closing a session able to end its processes
	// without walking every one this daemon holds.
	bySession map[string][]ID
}

const (
	// defaultBufferBytes is enough for a long build log and small enough that
	// a process nobody is reading cannot grow without bound.
	defaultBufferBytes = 256 << 10

	// stopGrace is how long a program is given to end on its own after being
	// asked. Past this it is killed: a caller asked for it to stop, and a
	// process that ignores the request is not one to wait on forever.
	stopGrace = 3 * time.Second
)

func NewManager() *Manager {
	return &Manager{
		running:   map[ID]*handle{},
		bySession: map[string][]ID{},
	}
}

// handle is one running program.
type handle struct {
	id      ID
	session string

	program string
	args    []string

	command *exec.Cmd

	// group contains the process and its descendants, so that stopping it
	// stops the tree rather than orphaning whatever it started. Its shape is
	// per-platform: a process group on Unix, a job object on Windows.
	group *procGroup

	// input writes to the program. For a terminal it is the same file as
	// output; for pipes it is stdin.
	input io.WriteCloser

	// terminal is the pty, kept so it can be resized and closed. Read and
	// written through the same file, which is what a terminal is: what the
	// program prints and what is typed at it are one connection.
	terminal terminalFile

	buffer     *ringBuffer
	isTerminal bool

	startedAt time.Time

	// done closes when the program has ended, so a caller can wait without
	// polling.
	done chan struct{}

	mu       sync.Mutex
	finished bool
	exitCode int
	waitErr  error
}

func (h *handle) state(now time.Time) State {
	h.mu.Lock()
	defer h.mu.Unlock()

	pid := 0
	if h.command.Process != nil {
		pid = h.command.Process.Pid
	}

	return State{
		ID:            h.id,
		Program:       h.program,
		Args:          append([]string{}, h.args...),
		PID:           pid,
		StartedAt:     h.startedAt,
		Running:       !h.finished,
		ExitCode:      h.exitCode,
		Terminal:      h.isTerminal,
		OutputDropped: h.buffer.dropped(),
	}
}

func (m *Manager) newID() ID {
	if m.NewID != nil {
		return m.NewID()
	}
	return ID(fmt.Sprintf("prc_%d", time.Now().UnixNano()))
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) bufferBytes() int {
	if m.BufferBytes > 0 {
		return m.BufferBytes
	}
	return defaultBufferBytes
}
