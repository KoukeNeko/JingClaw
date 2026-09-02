package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
)

// exampleFile is the copy checked into the repository, where somebody browsing
// it can read every setting without running anything.
//
// Under docs/ rather than beside the source. A "template" sitting where a
// config file could be read is one somebody will eventually edit and wonder
// why nothing changed — which is the whole failure the deployment directory
// was rearranged to prevent.
const exampleFile = "docs/config.example.toml"

// The checked-in copy is a derived artifact, the same as the generated
// protobuf code: one source of truth, and a test that catches it drifting.
//
// Without this, adding a setting updates the const, passes every other test,
// and leaves the file people actually open describing an older program.
func TestCheckedInExampleMatchesTheSource(t *testing.T) {
	path := filepath.Join("..", "..", "..", exampleFile)

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", exampleFile, err)
	}

	if string(onDisk) != config.Example {
		t.Errorf("%s no longer matches config.Example; regenerate it with:\n"+
			"\tgo run ./cmd/jingclaw daemon --print-config > ../%s", exampleFile, exampleFile)
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
	ownerOnly, exposure, err := fsperm.EnsureOwnerOnly(path)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !ownerOnly {
		t.Errorf("the created file %s", exposure)
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

// A deployment whose only way in is an environment variable can still be
// configured.
//
// Some container platforms offer exactly two inputs: a mounted volume and a
// list of environment variables. Individual settings do not survive that trip
// — the environment reaches `provider.backend` and stops, because a name like
// `api_key_env` makes any deeper underscore ambiguous — and a list of tables
// like the channel bindings cannot be written as a variable at all. So the
// whole file comes through as one, and is written where the file goes.
func TestTheWholeFileCanArriveInTheEnvironment(t *testing.T) {
	given := "[provider]\nbackend = \"ollama\"\n"
	t.Setenv(config.FileEnvVar, given)
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)

	path, created, err := config.EnsureFile()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !created {
		t.Fatal("it reported nothing to create")
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(written) != given {
		t.Errorf("the file holds %q, not what the environment gave", string(written))
	}

	// And it is as protected as one somebody wrote by hand, because it is the
	// same file and may hold the same credentials.
	ownerOnly, detail, err := fsperm.EnsureOwnerOnly(path)
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if !ownerOnly {
		t.Errorf("a config file from the environment is readable by others: %s", detail)
	}
}

// What is already in the volume wins.
//
// The variable is set on the service, so it arrives again on every restart. A
// write that overwrote would discard whatever somebody edited in the volume,
// and would do it on the restart after they edited it — which is the moment
// they would least expect to lose it.
func TestTheEnvironmentDoesNotOverwriteWhatIsAlreadyThere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)

	theirs := "[provider]\nbackend = \"gemini\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(theirs), 0o600); err != nil {
		t.Fatalf("staging: %v", err)
	}

	t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"ollama\"\n")

	path, created, err := config.EnsureFile()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if created {
		t.Error("it created a file over one that was already there")
	}

	written, _ := os.ReadFile(path)
	if string(written) != theirs {
		t.Errorf("the file was replaced by the environment's copy: %q", string(written))
	}
}

// TOML that will not parse is refused, and nothing is written.
//
// The file is created with O_EXCL and never overwritten, so a broken one
// written now is a deployment that cannot start and cannot be fixed by
// correcting the variable. Refusing here means the failure names the variable
// while somebody is still looking at it.
func TestUnparseableTOMLIsRefusedRatherThanWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)
	t.Setenv(config.FileEnvVar, "[provider\nbackend =")

	_, _, err := config.EnsureFile()
	if err == nil {
		t.Fatal("it accepted TOML that does not parse")
	}
	if !strings.Contains(err.Error(), config.FileEnvVar) {
		t.Errorf("the failure does not name the variable to fix: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Error("a file was written despite the content being unusable")
	}
}
