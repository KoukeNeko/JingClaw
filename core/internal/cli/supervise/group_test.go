//go:build !windows

package supervise

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Stopping a part takes what the part started with it.
//
// A part is not a process, it is a tree: the daemon runs whatever somebody
// approved, and those run their own children. Signalling the one process this
// program started leaves the rest holding whatever they held — a port, a
// database, a lock — and the next start fails for a reason that looks nothing
// like the cause.
func TestStoppingAPartTakesItsChildrenWithIt(t *testing.T) {
	// A process that starts another and then waits, so there is a grandchild
	// to be left behind.
	marker := t.TempDir() + "/grandchild"
	part := exec.Command("sh", "-c",
		"sh -c 'echo $$ > "+marker+"; while true; do sleep 1; done' & wait")
	ownProcessGroup(part)

	if err := part.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	grandchild := 0
	for range 100 {
		if raw, err := os.ReadFile(marker); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				grandchild = pid
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if grandchild == 0 {
		_ = part.Process.Kill()
		t.Fatal("the grandchild never said which pid it was")
	}

	terminate(part)

	// It may take a moment to die; it may not take four seconds.
	for range 80 {
		if syscall.Kill(grandchild, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = syscall.Kill(-grandchild, syscall.SIGKILL)
	t.Errorf("pid %d outlived the part that started it", grandchild)
}
