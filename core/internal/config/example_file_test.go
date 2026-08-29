package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// exampleFile is the copy checked into the repository, where somebody browsing
// it can read every setting without running anything.
const exampleFile = "config.example.toml"

// The checked-in copy is a derived artifact, the same as the generated
// protobuf code: one source of truth, and a test that catches it drifting.
//
// Without this, adding a setting updates the const, passes every other test,
// and leaves the file people actually open describing an older program.
func TestCheckedInExampleMatchesTheSource(t *testing.T) {
	path := filepath.Join("..", "..", exampleFile)

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", exampleFile, err)
	}

	if string(onDisk) != config.Example {
		t.Errorf("%s no longer matches config.Example; regenerate it with:\n"+
			"\tgo run ./cmd/agentd --print-config > %s", exampleFile, exampleFile)
	}
}

// Creating the file is meant to be a convenience, not a change of behaviour,
// so what it writes has to be the fully commented example.
func TestEnsureFileCreatesTheExample(t *testing.T) {

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	path, created, err := config.EnsureFile()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !created {
		t.Fatal("reported that a file was already there")
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read what was written: %v", err)
	}
	if string(written) != config.Example {
		t.Error("what was written is not the example")
	}

	// It holds no secrets today, but it is the file an operator will paste
	// instructions into, and widening permissions later is harder than
	// starting narrow.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %o, want 600", perm)
	}

	// Loading what was just created must change nothing.
	//
	// Compared against loading an empty file rather than against Defaults, so
	// that the two go through the same decoding and the test is about the
	// settings rather than about how a missing list decodes.
	cfg, used, err := config.Load("")
	if err != nil {
		t.Fatalf("load what was created: %v", err)
	}
	if used != path {
		t.Errorf("loaded %q, want the file that was created at %q", used, path)
	}

	empty, _, err := config.Load(writeConfig(t, "# nothing at all\n"))
	if err != nil {
		t.Fatalf("load an empty config: %v", err)
	}
	if !reflect.DeepEqual(cfg, empty) {
		t.Errorf("the created file changes behaviour despite being fully commented:\n got %+v\nwant %+v",
			cfg, empty)
	}
}

// A file somebody has already edited must never be replaced by a helpful
// default, however long ago it was written.
func TestEnsureFileLeavesAnExistingOneAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	path, _, err := config.EnsureFile()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	const edited = "[agent]\nname = \"江委員\"\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	again, created, err := config.EnsureFile()
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if created {
		t.Error("reported creating a file that was already there")
	}
	if again != path {
		t.Errorf("second call chose %q, want %q", again, path)
	}

	kept, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(kept) != edited {
		t.Errorf("the file was overwritten:\n%s", kept)
	}
}
