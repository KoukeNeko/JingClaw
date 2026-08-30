package supervise

import (
	"errors"
	"strings"
	"testing"
)

// jingclaw stop signals the daemon, so the daemon exiting is the point of the
// command rather than a failure of it.
func TestACleanExitIsNotAnError(t *testing.T) {
	if err := stopped("daemon", nil); err != nil {
		t.Errorf("a clean exit was reported as an error: %v", err)
	}
}

func TestACrashIsStillAnError(t *testing.T) {
	err := stopped("gateway", errors.New("exit status 2"))
	if err == nil {
		t.Fatal("a crash was reported as a clean stop")
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("the message does not say which part: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("the message does not carry the reason: %v", err)
	}
}
