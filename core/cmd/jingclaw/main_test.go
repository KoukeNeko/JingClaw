package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// The client subcommands read the configuration before they run, so they can
// find a daemon that was told to publish itself somewhere other than the
// default. That step must not reach the daemon, which is told where its
// configuration is by a flag of its own and may be running on a machine with
// no default location at all.
func TestTheDaemonDoesNotInheritTheClientsConfigStep(t *testing.T) {
	t.Setenv(home.EnvVar, home.None)

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
