package builtin

import (
	"fmt"
	"runtime"
	"testing"
)

// The process tests drive a real child process and read what it prints, feeds
// it input, or watch how it exits. A POSIX shell is not on every machine these
// run on — Windows has PowerShell — so each behaviour is written once for both
// shell families here and rendered for whichever the host has, rather than
// hard-coding sh and skipping Windows.

// scripts is the same behaviour spelled for a POSIX shell and for PowerShell.
type scripts struct {
	unix    string
	windows string
}

// hostShellCommand renders a behaviour for the shell this platform has and
// returns the program and arguments to start it with. It reuses ShellFor so the
// discovery of pwsh, powershell, or a POSIX shell lives in one place.
func hostShellCommand(t *testing.T, s scripts) (program string, args []string) {
	t.Helper()

	program, prefix, ok := ShellFor()
	if !ok {
		t.Skip("no shell on this platform to run a test process")
	}

	body := s.unix
	if runtime.GOOS == "windows" {
		// ShellFor can fall back to cmd.exe, whose syntax is not PowerShell's;
		// the -Command flag marks the PowerShell branch it hands back.
		if !hasArg(prefix, "-Command") {
			t.Skip("the process tests need PowerShell on Windows")
		}
		body = s.windows
	}

	args = append(append([]string(nil), prefix...), body)
	return program, args
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// printThenStay prints a line and then idles, so a caller can read the line
// while the process is still running. PowerShell buffers stdout when it is a
// pipe rather than a console, so the line is written and flushed explicitly or
// the reader would see nothing until the process exited.
func printThenStay(t *testing.T, line string, seconds int) (string, []string) {
	return hostShellCommand(t, scripts{
		unix: fmt.Sprintf("echo %s; sleep %d", line, seconds),
		windows: fmt.Sprintf(
			"[Console]::Out.WriteLine('%s'); [Console]::Out.Flush(); Start-Sleep %d",
			line, seconds),
	})
}

// greetFromStdin reads one line and answers with "hello " in front of it, which
// is what proves input reached the program.
func greetFromStdin(t *testing.T) (string, []string) {
	return hostShellCommand(t, scripts{
		unix: "read name; echo hello $name",
		windows: "$name = [Console]::In.ReadLine(); " +
			"[Console]::Out.WriteLine('hello ' + $name); [Console]::Out.Flush()",
	})
}

// printThenExit prints a line and exits with a code, so a caller learns both
// that it finished and how.
func printThenExit(t *testing.T, line string, code int) (string, []string) {
	return hostShellCommand(t, scripts{
		unix: fmt.Sprintf("echo %s; exit %d", line, code),
		windows: fmt.Sprintf(
			"[Console]::Out.WriteLine('%s'); exit %d", line, code),
	})
}

// sleepCommand idles for the given seconds and also returns the fragment of the
// command line a process listing will show, so a test can assert the listing
// names what is running without knowing which shell ran it.
func sleepCommand(t *testing.T, seconds int) (program string, args []string, shown string) {
	unixShown := fmt.Sprintf("sleep %d", seconds)
	windowsShown := fmt.Sprintf("Start-Sleep %d", seconds)

	program, args = hostShellCommand(t, scripts{unix: unixShown, windows: windowsShown})

	shown = unixShown
	if runtime.GOOS == "windows" {
		shown = windowsShown
	}
	return program, args, shown
}
