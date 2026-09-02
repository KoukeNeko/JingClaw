package config_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

	seeded, err := config.SeedFromEnvironment()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !slices.Contains(seeded, "config.toml") {
		t.Fatalf("it reported writing %v, not the config file", seeded)
	}

	// And the example does not then land on top of it.
	path, created, err := config.EnsureFile()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if created {
		t.Error("the example was written over the deployment's own file")
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

	seeded, err := config.SeedFromEnvironment()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(seeded) != 0 {
		t.Errorf("it wrote %v over what was already there", seeded)
	}

	written, _ := os.ReadFile(filepath.Join(home, "config.toml"))
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

	_, err := config.SeedFromEnvironment()
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

// The files that say who the agent is arrive the same way.
//
// A configuration is not the whole deployment. PERSONA.md and AGENTS.md are
// read from the same directory and are just as unreachable on a platform
// whose only inputs are a volume and a list of variables — and a persona is
// the one somebody most wants to set, because it is what makes the deployment
// theirs rather than a default.
func TestTheInstructionFilesCanArriveInTheEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)
	t.Setenv(config.PersonaEnvVar, "# Who you are\n\nBrief, and never chatty.\n")
	t.Setenv(config.InstructionsEnvVar, "# How this project works\n")

	if _, err := config.SeedFromEnvironment(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for name, wanted := range map[string]string{
		config.PersonaFile:      "never chatty",
		config.InstructionsFile: "How this project works",
	} {
		written, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(string(written), wanted) {
			t.Errorf("%s holds %q", name, string(written))
		}
	}
}

// A value that will not survive a single-line field can be encoded.
//
// Some platforms take environment variables through a web form whose field is
// one line. A persona is many. Rather than guessing whether a value has been
// encoded — a short markdown file is valid base64 often enough to be a
// problem — the value says so itself.
func TestAnEncodedValueIsDecoded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)

	persona := "# Who you are\n\nOne line, then another.\n"
	t.Setenv(config.PersonaEnvVar, "base64:"+base64.StdEncoding.EncodeToString([]byte(persona)))

	if _, err := config.SeedFromEnvironment(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(home, config.PersonaFile))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(written) != persona {
		t.Errorf("decoded to %q", string(written))
	}
}

// And one that says it is encoded and is not says so.
func TestAnEncodedValueThatIsNotIsRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)
	t.Setenv(config.PersonaEnvVar, "base64:this is not base64 at all!!")

	_, err := config.SeedFromEnvironment()
	if err == nil {
		t.Fatal("it accepted something that claimed to be encoded and was not")
	}
	if !strings.Contains(err.Error(), config.PersonaEnvVar) {
		t.Errorf("the failure does not name the variable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, config.PersonaFile)); !os.IsNotExist(err) {
		t.Error("a file was written from a value that could not be decoded")
	}
}

// What is already in the volume wins, here too.
func TestSeedingDoesNotOverwriteAnInstructionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)

	theirs := "# Mine, edited in the volume\n"
	if err := os.WriteFile(filepath.Join(home, config.PersonaFile), []byte(theirs), 0o600); err != nil {
		t.Fatalf("staging: %v", err)
	}
	t.Setenv(config.PersonaEnvVar, "# From the variable\n")

	if _, err := config.SeedFromEnvironment(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	written, _ := os.ReadFile(filepath.Join(home, config.PersonaFile))
	if string(written) != theirs {
		t.Errorf("the file was replaced by the environment's copy: %q", string(written))
	}
}

// One unusable variable leaves nothing behind, not even the usable ones.
//
// These files are created once and never replaced. A run that wrote the
// configuration and then refused the persona would leave a deployment holding
// a file it can no longer change by fixing the variable that names it, and
// the operator would be looking at the persona error while the config was
// quietly settled.
func TestARefusedValueWritesNoneOfTheOthers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JINGCLAW_HOME", home)
	t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"ollama\"\n")
	t.Setenv(config.PersonaEnvVar, "base64:not base64 at all !!!")

	if _, err := config.SeedFromEnvironment(); err == nil {
		t.Fatal("it accepted a value that announces an encoding and has none")
	}

	for _, name := range []string{"config.toml", config.PersonaFile} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written despite the run being refused", name)
		}
	}
}
