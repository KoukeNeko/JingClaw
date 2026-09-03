package web_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/web"
)

// An interpreter without the package is a deployment that starts and then
// cannot read anything.
//
// PythonPath existed so a misconfiguration was reported at startup rather
// than at the first page, and it only ever looked for the interpreter. A
// container with python3 and no cloakbrowser passed it, came up, validated,
// and failed on the first fetch — which from the room looks like a model
// refusing to use the tool.
func TestAnInterpreterWithoutThePackageIsRefused(t *testing.T) {
	// Exits 1 for any argument, which is what an interpreter without the
	// package does when asked to import it.
	pretend := stubInterpreter(t, 1)

	err := web.CanDriveABrowser(pretend)
	if err == nil {
		t.Fatal("an interpreter that cannot import the package was accepted")
	}
	if !strings.Contains(err.Error(), "cloakbrowser") {
		t.Errorf("the failure does not name the package: %v", err)
	}
}

// And one that can is accepted.
func TestAnInterpreterThatCanImportIsAccepted(t *testing.T) {
	pretend := stubInterpreter(t, 0)

	if err := web.CanDriveABrowser(pretend); err != nil {
		t.Errorf("an interpreter that imports it was refused: %v", err)
	}
}

// stubInterpreter writes something that can be run and exits how it is told.
//
// Per platform, because a shebang is a Unix thing: Windows runs neither a
// #! line nor an extensionless file, and a check written for one machine that
// cannot run on the other is how a platform stops being tested.
func stubInterpreter(t *testing.T, exitCode int) string {
	t.Helper()

	name, body := "python3", fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	if runtime.GOOS == "windows" {
		name, body = "python3.cmd", fmt.Sprintf("@exit /b %d\r\n", exitCode)
	}

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("staging: %v", err)
	}
	return path
}
