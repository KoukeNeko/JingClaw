package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// The client subcommands work out where the daemon is before they run, from the
// defaults and the environment, and a variable that cannot be read stops them.
// That step must not reach the daemon, which reads the environment itself, in
// its own order, and may be asked only to print the example settings by someone
// whose environment is no use to a client.
func TestTheDaemonDoesNotInheritTheClientsEnvironmentStep(t *testing.T) {
	t.Setenv("JINGCLAW_AGENT_MAX_ITERATIONS", "not-a-number")

	// The precondition: the step does fail on this environment, so the daemon
	// getting past it below is the daemon not running the step.
	asClient := root()
	asClient.SetArgs([]string{"session", "list"})
	if err := asClient.Execute(); err == nil || !strings.Contains(err.Error(), "max_iterations") {
		t.Fatalf("a client command was not stopped by an environment it cannot read: %v", err)
	}

	command := root()
	command.SetArgs([]string{"daemon", "--print-config"})

	printed := capture(t, func() {
		if err := command.Execute(); err != nil {
			t.Errorf("jingclaw daemon --print-config: %v", err)
		}
	})
	if !strings.Contains(printed, "[provider]") {
		t.Errorf("printed something that is not the example configuration:\n%s", printed)
	}
}

// capture returns what run wrote to standard output.
//
// The subcommands print their answers to the real stream rather than to a
// writer the command carries, so a test that wants to read one has to take
// the stream itself.
func capture(t *testing.T, run func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	was := os.Stdout
	os.Stdout = write

	done := make(chan string, 1)
	go func() {
		printed, _ := io.ReadAll(read)
		done <- string(printed)
	}()

	run()

	os.Stdout = was
	if err := write.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return <-done
}
