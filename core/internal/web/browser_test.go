package web_test

import (
	"os"
	"path/filepath"
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
	dir := t.TempDir()
	pretend := filepath.Join(dir, "python3")
	// Exits 1 for any argument, which is what an interpreter without the
	// package does when asked to import it.
	if err := os.WriteFile(pretend, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("staging: %v", err)
	}

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
	dir := t.TempDir()
	pretend := filepath.Join(dir, "python3")
	if err := os.WriteFile(pretend, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("staging: %v", err)
	}

	if err := web.CanDriveABrowser(pretend); err != nil {
		t.Errorf("an interpreter that imports it was refused: %v", err)
	}
}
