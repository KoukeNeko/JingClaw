package config_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// clearSeeds makes the environment carry nothing, whatever the shell running
// the tests had in it.
func clearSeeds(t *testing.T) {
	t.Helper()

	for _, variable := range []string{config.FileEnvVar, config.PersonaEnvVar, config.InstructionsEnvVar} {
		t.Setenv(variable, "")
	}
}

func seedOf(t *testing.T, seeds []config.Seed, document string) (string, bool) {
	t.Helper()

	for _, seed := range seeds {
		if seed.Document == document {
			return seed.Text, true
		}
	}
	return "", false
}

// A deployment whose only way in is an environment variable can still be
// configured.
//
// Some container platforms offer exactly two inputs: a mounted volume and a
// list of environment variables. Individual settings do not survive that trip
// — the environment reaches `provider.backend` and stops, because a name like
// `api_key_env` makes any deeper underscore ambiguous — and a list of tables
// like the channel bindings cannot be written as a variable at all. So the
// whole config comes through as one.
func TestTheWholeConfigCanArriveInTheEnvironment(t *testing.T) {
	clearSeeds(t)
	given := "[provider]\nbackend = \"ollama\"\n"
	t.Setenv(config.FileEnvVar, given)

	seeds, err := config.SeedsFromEnvironment()
	if err != nil {
		t.Fatalf("seeds: %v", err)
	}

	if text, ok := seedOf(t, seeds, "config"); !ok || text != given {
		t.Errorf("the config seed is %q (present: %v), not what the environment gave", text, ok)
	}
	if len(seeds) != 1 {
		t.Errorf("it carried %v, and only the config was given", seeds)
	}
}

// Nothing in the environment carries nothing: the caller then saves the
// example, or leaves a document empty.
func TestAVariableThatIsUnsetOrBlankCarriesNothing(t *testing.T) {
	clearSeeds(t)
	t.Setenv(config.PersonaEnvVar, "  \n")

	seeds, err := config.SeedsFromEnvironment()
	if err != nil {
		t.Fatalf("seeds: %v", err)
	}
	if len(seeds) != 0 {
		t.Errorf("it carried %v from variables that say nothing", seeds)
	}
}

// TOML that will not parse is refused, and nothing is returned.
//
// Refusing here means the failure names the variable while somebody is still
// looking at it, rather than saving text the daemon can then only report on.
func TestUnparseableTOMLIsRefusedRatherThanReturned(t *testing.T) {
	clearSeeds(t)
	t.Setenv(config.FileEnvVar, "[provider\nbackend =")

	seeds, err := config.SeedsFromEnvironment()
	if err == nil {
		t.Fatal("it accepted TOML that does not parse")
	}
	if !strings.Contains(err.Error(), config.FileEnvVar) {
		t.Errorf("the failure does not name the variable to fix: %v", err)
	}
	if len(seeds) != 0 {
		t.Errorf("it returned %v despite the content being unusable", seeds)
	}
}

// The documents that say who the agent is arrive the same way.
//
// A configuration is not the whole deployment. The persona and the agents
// document are just as unreachable on a platform whose only inputs are a volume
// and a list of variables — and a persona is the one somebody most wants to
// set, because it is what makes the deployment theirs rather than a default.
func TestTheInstructionDocumentsCanArriveInTheEnvironment(t *testing.T) {
	clearSeeds(t)
	t.Setenv(config.PersonaEnvVar, "# Who you are\n\nBrief, and never chatty.\n")
	t.Setenv(config.InstructionsEnvVar, "# How this project works\n")

	seeds, err := config.SeedsFromEnvironment()
	if err != nil {
		t.Fatalf("seeds: %v", err)
	}

	for document, wanted := range map[string]string{
		"persona": "never chatty",
		"agents":  "How this project works",
	} {
		text, ok := seedOf(t, seeds, document)
		if !ok || !strings.Contains(text, wanted) {
			t.Errorf("%s holds %q (present: %v)", document, text, ok)
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
	clearSeeds(t)
	persona := "# Who you are\n\nOne line, then another.\n"
	t.Setenv(config.PersonaEnvVar, "base64:"+base64.StdEncoding.EncodeToString([]byte(persona)))

	seeds, err := config.SeedsFromEnvironment()
	if err != nil {
		t.Fatalf("seeds: %v", err)
	}
	if text, _ := seedOf(t, seeds, "persona"); text != persona {
		t.Errorf("decoded to %q", text)
	}
}

// And one that says it is encoded and is not says so.
func TestAnEncodedValueThatIsNotIsRefused(t *testing.T) {
	clearSeeds(t)
	t.Setenv(config.PersonaEnvVar, "base64:this is not base64 at all!!")

	seeds, err := config.SeedsFromEnvironment()
	if err == nil {
		t.Fatal("it accepted something that claimed to be encoded and was not")
	}
	if !strings.Contains(err.Error(), config.PersonaEnvVar) {
		t.Errorf("the failure does not name the variable: %v", err)
	}
	if len(seeds) != 0 {
		t.Errorf("it returned %v from a value that could not be decoded", seeds)
	}
}

// One unusable variable returns nothing, not even the usable ones.
//
// The caller saves what it is given in one go. A run that returned the
// configuration and then refused the persona would leave the operator looking
// at the persona error while the config was quietly settled, and fixing the
// variable that names it would not unsettle the config.
func TestARefusedValueReturnsNoneOfTheOthers(t *testing.T) {
	clearSeeds(t)
	t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"ollama\"\n")
	t.Setenv(config.PersonaEnvVar, "base64:not base64 at all !!!")

	seeds, err := config.SeedsFromEnvironment()
	if err == nil {
		t.Fatal("it accepted a value that announces an encoding and has none")
	}
	if len(seeds) != 0 {
		t.Errorf("it returned %v despite the run being refused", seeds)
	}
}

// A document name the store does not know would be refused only when the
// caller came to save it. This keeps the two lists of names from drifting.
func TestEverySeedIsNamedForADocumentTheStoreKeeps(t *testing.T) {
	t.Setenv(config.FileEnvVar, "[provider]\nbackend = \"ollama\"\n")
	t.Setenv(config.PersonaEnvVar, "persona")
	t.Setenv(config.InstructionsEnvVar, "agents")

	seeds, err := config.SeedsFromEnvironment()
	if err != nil {
		t.Fatalf("seeds: %v", err)
	}
	if len(seeds) != 3 {
		t.Fatalf("it carried %v, not all three documents", seeds)
	}
	for _, seed := range seeds {
		if err := storage.CheckDocumentName(seed.Document); err != nil {
			t.Errorf("%v", err)
		}
	}
}
