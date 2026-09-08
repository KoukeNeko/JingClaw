//go:build windows

package conpty

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// drain reads everything the console prints into a buffer until it ends, so the
// program is never blocked writing to a pipe nobody is reading.
func drain(console *Console) (*bytes.Buffer, *sync.Mutex, <-chan struct{}) {
	var output bytes.Buffer
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			read, err := console.Read(buffer)
			if read > 0 {
				mu.Lock()
				output.Write(buffer[:read])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	return &output, &mu, done
}

// The proof the whole spike turns on: a program started under a pseudo console
// runs, and what it prints comes back through the console rather than leaking to
// the real one. The program is kept alive a moment after it prints, because the
// console renders on its own cadence and a program that exits at once can be
// gone before its line is ever drawn.
func TestAProgramRunsUnderAPseudoConsole(t *testing.T) {
	console, err := Start("cmd.exe",
		[]string{"/c", "echo hello-conpty & ping -n 3 127.0.0.1 >nul"}, "", nil, 80, 25)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer console.Close()

	output, mu, done := drain(console)

	code, err := console.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	// Ending the output flushes the last of it and lets the reader see EOF; the
	// program's exit alone does not, because the console outlives it.
	console.EndOutput()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader did not stop after the console ended")
	}

	mu.Lock()
	text := output.String()
	mu.Unlock()
	if !strings.Contains(text, "hello-conpty") {
		t.Errorf("the program's output never arrived:\n%q", text)
	}
}

// The program is handed exactly the environment it is given and nothing else,
// so the daemon's own secrets in this process's environment never reach it.
func TestTheProgramGetsOnlyTheEnvironmentItIsGiven(t *testing.T) {
	t.Setenv("CONPTY_SECRET", "must-not-leak")

	shell := os.Getenv("ComSpec")
	if shell == "" {
		t.Skip("no command interpreter to run")
	}

	console, err := Start(shell,
		[]string{"/c", "echo given=[%CONPTY_GIVEN%] secret=[%CONPTY_SECRET%] & ping -n 3 127.0.0.1 >nul"},
		"",
		[]string{"CONPTY_GIVEN=handed-in", "SystemRoot=" + os.Getenv("SystemRoot")},
		80, 25)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer console.Close()

	output, mu, done := drain(console)
	if _, err := console.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	console.EndOutput()
	<-done

	mu.Lock()
	text := output.String()
	mu.Unlock()

	if !strings.Contains(text, "given=[handed-in]") {
		t.Errorf("the given environment did not reach the program:\n%q", text)
	}
	if strings.Contains(text, "must-not-leak") {
		t.Error("a secret from this process's environment leaked to the program")
	}
}

// A terminal that cannot be resized is not one the interactive tools can use.
func TestAPseudoConsoleCanBeResized(t *testing.T) {
	console, err := Start("cmd.exe", []string{"/c", "echo sized"}, "", nil, 80, 25)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer console.Close()

	_, _, done := drain(console)

	if err := console.Resize(100, 40); err != nil {
		t.Errorf("resize: %v", err)
	}

	if _, err := console.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	_ = console.Close()
	<-done
}
