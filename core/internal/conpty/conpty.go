//go:build windows

// Package conpty runs a program attached to a Windows pseudo console.
//
// It is the Windows answer to a Unix pseudo-terminal: one duplex stream
// carrying a program's input and output, so a REPL, an installer that asks a
// question, or ssh behaves as though a person were at a terminal rather than
// block-buffering into a pipe.
//
// os/exec cannot attach one. A pseudo console is handed to a process through a
// creation attribute that must be in place before the process exists, which
// os/exec does not expose, so this package creates the process itself with a
// STARTUPINFOEX. It is deliberately its own abstraction rather than a patch on
// os/exec, so the process package can own the Windows lifecycle end to end.
package conpty

import (
	"fmt"
	"io"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Console is a program running attached to a pseudo console.
type Console struct {
	pseudoConsole windows.Handle
	process       windows.Handle
	pid           int

	// input carries what we type towards the program; output carries what the
	// program prints. They are the ends the pseudo console left us after taking
	// the other two for itself. Read and written with the Windows calls directly
	// rather than through os.File, which does not read these synchronous pipe
	// handles.
	input  windows.Handle
	output windows.Handle

	// The ends the pseudo console reads input from and writes output to. It does
	// not take a copy, so these are held open for its lifetime; closing the
	// output end is what leaves the output pipe with no writer and so ends the
	// reader, and it must not happen until the console has been told to stop.
	consoleInput  windows.Handle
	consoleOutput windows.Handle
}

// Start opens a pseudo console of the given size and runs the program in it.
//
// A zero size is replaced with something usable rather than refused: a program
// that asks the terminal how wide it is should get an answer, not a zero. A nil
// env inherits this process's environment; a non-nil env is given verbatim,
// which is how a caller keeps the daemon's own secrets out of a program.
func Start(program string, args []string, dir string, env []string, cols, rows int) (*Console, error) {
	if cols <= 0 {
		cols = defaultColumns
	}
	if rows <= 0 {
		rows = defaultRows
	}

	// Two pipes. The console reads the program's input from inRead and writes
	// the program's output to outWrite; we keep inWrite and outRead.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("conpty: input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		closeHandles(inRead, inWrite)
		return nil, fmt.Errorf("conpty: output pipe: %w", err)
	}

	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	var pseudoConsole windows.Handle
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &pseudoConsole); err != nil {
		closeHandles(inRead, inWrite, outRead, outWrite)
		return nil, fmt.Errorf("conpty: create pseudo console: %w", err)
	}

	process, pid, err := startAttached(program, args, dir, env, pseudoConsole)
	if err != nil {
		windows.ClosePseudoConsole(pseudoConsole)
		closeHandles(inRead, inWrite, outRead, outWrite)
		return nil, err
	}

	return &Console{
		pseudoConsole: pseudoConsole,
		process:       process,
		pid:           pid,
		input:         inWrite,
		output:        outRead,
		consoleInput:  inRead,
		consoleOutput: outWrite,
	}, nil
}

// startAttached creates the process with the pseudo console wired in as a
// creation attribute, which is the step os/exec cannot do.
func startAttached(program string, args []string, dir string, env []string, pseudoConsole windows.Handle) (windows.Handle, int, error) {
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, 0, fmt.Errorf("conpty: attribute list: %w", err)
	}
	defer attributes.Delete()

	// The attribute value is the pseudo console handle itself: lpValue carries
	// the handle-sized value, not the address of a handle. handleAsPointer
	// passes the handle's bits where a pointer goes without a uintptr
	// conversion, which is both the value the kernel wants and a form go vet
	// accepts.
	if err := attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		handleAsPointer(pseudoConsole),
		unsafe.Sizeof(pseudoConsole),
	); err != nil {
		return 0, 0, fmt.Errorf("conpty: attach pseudo console: %w", err)
	}

	startup := new(windows.StartupInfoEx)
	startup.Cb = uint32(unsafe.Sizeof(*startup))
	startup.ProcThreadAttributeList = attributes.List()
	// Say the standard handles are given, while leaving them empty. Without this
	// the child inherits the parent's standard handles and its output goes there
	// instead of to the pseudo console; with it, and the handles left unset, the
	// pseudo console is the only thing left to be its console.
	startup.Flags |= windows.STARTF_USESTDHANDLES

	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{program}, args...)))
	if err != nil {
		return 0, 0, fmt.Errorf("conpty: command line: %w", err)
	}
	var directory *uint16
	if dir != "" {
		if directory, err = windows.UTF16PtrFromString(dir); err != nil {
			return 0, 0, fmt.Errorf("conpty: working directory: %w", err)
		}
	}

	environment, err := environmentBlock(env)
	if err != nil {
		return 0, 0, fmt.Errorf("conpty: environment: %w", err)
	}
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT)
	if environment != nil {
		flags |= windows.CREATE_UNICODE_ENVIRONMENT
	}

	var info windows.ProcessInformation
	// No inherited handles and no std-handle fields: the pseudo console, not an
	// inherited pipe, is what the program talks to.
	if err := windows.CreateProcess(
		nil, commandLine, nil, nil, false,
		flags,
		environment, directory, &startup.StartupInfo, &info,
	); err != nil {
		return 0, 0, fmt.Errorf("conpty: start %s: %w", program, err)
	}

	// The thread handle is of no use here; the process handle is kept to wait on.
	closeHandles(info.Thread)
	return info.Process, int(info.ProcessId), nil
}

// Read returns what the program has printed. It includes the terminal's own
// control sequences, the same as a real pseudo-terminal would. The end of the
// program's output arrives as a broken pipe, which is reported as io.EOF.
func (c *Console) Read(into []byte) (int, error) {
	if len(into) == 0 {
		return 0, nil
	}
	var read uint32
	err := windows.ReadFile(c.output, into, &read, nil)
	if err == windows.ERROR_BROKEN_PIPE {
		return int(read), io.EOF
	}
	if err != nil {
		return int(read), fmt.Errorf("conpty: read: %w", err)
	}
	return int(read), nil
}

// Write sends input to the program as though it were typed.
func (c *Console) Write(from []byte) (int, error) {
	var wrote uint32
	if err := windows.WriteFile(c.input, from, &wrote, nil); err != nil {
		return int(wrote), fmt.Errorf("conpty: write: %w", err)
	}
	return int(wrote), nil
}

// Resize tells the program the terminal changed size.
func (c *Console) Resize(cols, rows int) error {
	if err := windows.ResizePseudoConsole(c.pseudoConsole, windows.Coord{X: int16(cols), Y: int16(rows)}); err != nil {
		return fmt.Errorf("conpty: resize: %w", err)
	}
	return nil
}

// PID is the operating system's number for the program.
func (c *Console) PID() int { return c.pid }

// Kill ends the program at once. It is the hard stop for a terminal process,
// which has no signal to be asked with gently.
func (c *Console) Kill() error {
	if c.process == 0 {
		return nil
	}
	if err := windows.TerminateProcess(c.process, 1); err != nil {
		return fmt.Errorf("conpty: kill: %w", err)
	}
	return nil
}

// Wait blocks until the program ends and reports its exit code.
func (c *Console) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(c.process, windows.INFINITE); err != nil {
		return -1, fmt.Errorf("conpty: wait: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.process, &code); err != nil {
		return -1, fmt.Errorf("conpty: exit code: %w", err)
	}
	return int(code), nil
}

// EndOutput closes the pseudo console, which flushes what the program left and
// then closes the console's own end of the output pipe. That is what lets a
// reader drain the last of the output and see end-of-file, so it is done before
// the reader is joined and the handles are closed. A program's exit does not
// close the pipe on its own: the console outlives it until told to stop.
func (c *Console) EndOutput() {
	if c.pseudoConsole != 0 {
		windows.ClosePseudoConsole(c.pseudoConsole)
		c.pseudoConsole = 0
	}
	// Once the console has stopped, releasing its ends of the pipes leaves the
	// output pipe with no writer, which is what a pending Read sees as EOF.
	closeHandles(c.consoleInput, c.consoleOutput)
	c.consoleInput, c.consoleOutput = 0, 0
}

// Close ends the console and releases everything it holds.
func (c *Console) Close() error {
	c.EndOutput()
	if c.process != 0 {
		_ = windows.CloseHandle(c.process)
		c.process = 0
	}
	closeHandles(c.input, c.output)
	c.input, c.output = 0, 0
	return nil
}

// handleAsPointer reinterprets a handle's bits as an unsafe.Pointer, so a
// handle-valued attribute can be passed where lpValue goes without a uintptr
// conversion that go vet would flag.
func handleAsPointer(handle windows.Handle) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&handle))
}

// environmentBlock builds the environment CreateProcess expects: the entries
// run together, each ended by a NUL, with one more NUL to close the block. A
// nil env returns nil, which tells CreateProcess to inherit this process's own;
// a non-nil env is exactly what the program gets, and nothing else.
func environmentBlock(env []string) (*uint16, error) {
	if env == nil {
		return nil, nil
	}
	var block []uint16
	for _, entry := range env {
		// A NUL inside an entry would end it early, so an entry that cannot be
		// encoded is left out rather than allowed to truncate the block.
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			continue
		}
		block = append(block, encoded...)
	}
	block = append(block, 0)
	return &block[0], nil
}

func closeHandles(handles ...windows.Handle) {
	for _, handle := range handles {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
	}
}

const (
	defaultColumns = 120
	defaultRows    = 40
)
