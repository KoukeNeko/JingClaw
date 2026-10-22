package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/chatgptauth"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
)

// credentialKeys are the eight settings that name a credential file, written
// out here so that a ninth added to the code and not to this list, or one that
// stops being a setting, is a failure.
var credentialKeys = []string{
	"provider.gemini.api_key_file",
	"provider.anthropic.api_key_file",
	"provider.ollama.api_key_file",
	"provider.openai_compat.api_key_file",
	"memory.embedding.api_key_file",
	"web.search.key_file",
	"gateway.discord.token_file",
	"gateway.telegram.token_file",
}

// problemsFor is what the check says about a config text, for one key.
func problemsFor(t *testing.T, text, key string) []config.Problem {
	t.Helper()

	problems, err := config.Check([]byte(text))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	var found []config.Problem
	for _, problem := range problems {
		if problem.Key == key {
			found = append(found, problem)
		}
	}
	return found
}

// textSetting is a config text that gives one setting a value, by way of the
// edit that the dashboard would make.
func textSetting(t *testing.T, key string, value any) string {
	t.Helper()

	patched, err := config.Patch([]byte(config.Example), []config.Edit{{Key: key, Value: value}})
	if err != nil {
		t.Fatalf("patch %s: %v", key, err)
	}
	return string(patched.Text)
}

func TestTheCredentialFileSettingsAreTheEightThatExist(t *testing.T) {
	flat := config.Flatten(config.Defaults())

	var keys []string
	for _, file := range config.Defaults().CredentialFiles() {
		keys = append(keys, file.Key)
		if _, ok := flat[file.Key]; !ok {
			t.Errorf("%s is not a setting", file.Key)
		}
		if _, ok := flat[file.EnvKey]; !ok {
			t.Errorf("%s is not a setting", file.EnvKey)
		}
		if file.Suggest == "" {
			t.Errorf("%s has no name to suggest", file.Slot)
		}
	}
	if strings.Join(keys, " ") != strings.Join(credentialKeys, " ") {
		t.Errorf("the settings are\n%v\nwant\n%v", keys, credentialKeys)
	}

	// Their names are the ones the defaults and the example have always used, so
	// that a config nobody changed passes.
	if problems := config.Defaults().CredentialFileProblems(); len(problems) != 0 {
		t.Errorf("the defaults are refused: %+v", problems)
	}
}

// A name is joined onto the deployment directory and read. One that climbs out
// of it made the daemon read a file that was never meant for it and send what
// it held to a provider as a key.
func TestAFileNameThatLeavesTheDeploymentIsRefusedForEverySetting(t *testing.T) {
	for _, key := range credentialKeys {
		for _, name := range []string{
			"../outside.key", "../../etc/passwd", "/etc/passwd", "keys/gemini.key", `keys\gemini.key`,
			"gemini.key/", "./gemini.key", "..", ".", ".hidden", "-dash", "trailing.", "has space",
			"tab\tname", "new\nline", "nul\x00name", "ключ.key", strings.Repeat("a", 65), "~root", "a.bak.",
		} {
			problems := problemsFor(t, textSetting(t, key, name), key)
			if len(problems) != 1 {
				t.Errorf("%s = %q: %d problems, want 1", key, name, len(problems))
				continue
			}

			problem := problems[0]
			if problem.Why == "" || !strings.Contains(problem.Fix, "letters, digits") ||
				!strings.Contains(problem.Fix, "64") || !strings.Contains(problem.Fix, "directly inside the deployment directory") {
				t.Errorf("%s = %q is refused without saying what is allowed: %+v", key, name, problem)
			}
		}
	}
}

func TestAFileNameTheDeploymentKeepsForItselfIsRefusedWhateverItsCase(t *testing.T) {
	for _, name := range []string{
		"config.toml", "Config.TOML", "AGENTS.md", "agents.md", "PERSONA.md", "persona.MD",
		"workspace", "WORKSPACE", "data", "run", "log", "bin", "skills", "Skills",
		"mcp-auth", "sandbox", "Sandbox", "chatgpt-auth.json", "ChatGPT-Auth.JSON", "dashboard-auth.json", "Dashboard-Sessions.json",
		"config.toml.bak.1", "x.BAK.2", "gemini.bak.key",
		// What a settings file is renamed to once it has been imported. It is the
		// only copy of what it held, and a credential saved or deleted there would
		// overwrite or remove it.
		"config.toml.imported", "AGENTS.md.imported", "PERSONA.md.IMPORTED", "gemini.key.imported",
	} {
		problems := problemsFor(t, textSetting(t, "provider.gemini.api_key_file", name), "provider.gemini.api_key_file")
		if len(problems) != 1 || !strings.Contains(problems[0].Why, "keeps for something else") {
			t.Errorf("%q is accepted or refused for the wrong reason: %+v", name, problems)
		}
	}

	// What the daemon makes in the deployment directory is on the list because
	// it is made there, so the list follows the code that makes them.
	for _, name := range []string{
		home.ConfigName, config.InstructionsFile, config.PersonaFile,
		home.WorkspaceName, home.DataName, home.RunName, home.LogName, home.BinName, home.SkillsName,
		mcpauth.DirName, chatgptauth.FileName,
	} {
		if problems := problemsFor(t, textSetting(t, "web.search.key_file", name), "web.search.key_file"); len(problems) != 1 {
			t.Errorf("%q, which this deployment uses, is accepted as a credential file", name)
		}
	}
}

func TestPlainFileNamesAreAccepted(t *testing.T) {
	for _, name := range []string{
		"a", "A", "9", "key_2.token", "my-key", "my.key.v2", strings.Repeat("a", 64), "x.bak", "bak.x", "imported", "imported.key", "configtoml",
	} {
		for _, key := range credentialKeys {
			if problems := problemsFor(t, textSetting(t, key, name), key); len(problems) != 0 {
				t.Errorf("%s = %q is refused: %+v", key, name, problems)
			}
		}
	}

	// No name is not a bad name: it is a credential that only the environment
	// can give.
	for _, key := range credentialKeys {
		if problems := problemsFor(t, textSetting(t, key, ""), key); len(problems) != 0 {
			t.Errorf("%s = \"\" is refused: %+v", key, problems)
		}
	}
}

// Two credentials in one file would each overwrite the other, and a save to
// either would be a save to both. So the service that saves says so, and does
// not write.
func TestTwoCredentialsCannotShareAFileToBeSavedTo(t *testing.T) {
	text := `[provider.gemini]
api_key_file = "shared.key"

[provider.anthropic]
api_key_file = "Shared.KEY"

[provider.ollama]
api_key_file = "alone.key"
`
	cfg, err := config.Candidate([]byte(text))
	if err != nil {
		t.Fatal(err)
	}

	problems := map[string][]config.Problem{}
	for _, problem := range cfg.SharedCredentialFileProblems() {
		problems[problem.Key] = append(problems[problem.Key], problem)
	}
	for key, other := range map[string]string{
		"provider.gemini.api_key_file":    "provider.anthropic.api_key_file",
		"provider.anthropic.api_key_file": "provider.gemini.api_key_file",
	} {
		if got := problems[key]; len(got) != 1 || !strings.Contains(got[0].Why, other) {
			t.Errorf("%s: %+v, want a problem naming %s", key, got, other)
		}
	}
	if got := problems["provider.ollama.api_key_file"]; len(got) != 0 {
		t.Errorf("a name nobody else uses is refused: %+v", got)
	}
}

// A deployment that keeps one key for a provider and for the embeddings on the
// same endpoint in a single file works, and has worked. It is not a settings
// error: the daemon starts with it, and only the dashboard declines to save to it.
func TestTwoCredentialsSharingAFileStillPassValidate(t *testing.T) {
	text := `[provider]
backend = "openai_compat"

[provider.openai_compat]
base_url = "https://models.example.com/v1"
api_key_file = "openai.key"

[memory.embedding]
base_url = "https://models.example.com/v1"
model = "text-embedding"
api_key_file = "openai.key"
`
	cfg, err := config.Candidate([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a key kept in one file for two uses fails Validate, so an upgrade of a working deployment ends in repair mode: %v", err)
	}
	if len(cfg.SharedCredentialFileProblems()) != 2 {
		t.Errorf("the service is not told about it: %+v", cfg.SharedCredentialFileProblems())
	}
}

// A problem is printed, logged and shown to whoever asks why there is no agent.
// A name this long is more often a key pasted in the wrong place than a path.
func TestAFileNameTooLongToBeOneIsNotQuotedBack(t *testing.T) {
	pasted := "sk-ant-api03-" + strings.Repeat("Zq9", 30)

	problems := problemsFor(t, textSetting(t, "provider.anthropic.api_key_file", pasted), "provider.anthropic.api_key_file")
	if len(problems) != 1 {
		t.Fatalf("problems: %+v", problems)
	}
	if said := problems[0].Key + problems[0].Value + problems[0].Why + problems[0].Fix; strings.Contains(said, "Zq9Zq9") {
		t.Errorf("the problem repeats the value: %+v", problems[0])
	}
}

// Settings that fail now are reported the way every other setting that will not
// do is: with the line to edit and what to write, and with a daemon left
// running to edit it in.
func TestAFileNameThatIsNotAllowedFailsValidateLikeAnyOtherSetting(t *testing.T) {
	cfg := config.Defaults()
	cfg.Provider.Gemini.APIKeyFile = "../x"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a name that climbs out of the deployment passes Validate")
	}
	report := err.(*config.InvalidError).Report("the saved settings")
	if !strings.Contains(report, `provider.gemini.api_key_file = "(not shown)"`) || !strings.Contains(report, "letters, digits") {
		t.Errorf("the report does not point at the line or say what to write:\n%s", report)
	}
	if strings.Contains(report, "../x") {
		t.Errorf("the report quotes a name that is not allowed:\n%s", report)
	}
}

// The name offered to somebody whose setting is empty has to be one the check
// accepts, and has to leave room for the other seven to be offered theirs.
func TestEverySuggestedNameIsAllowedAndNoTwoAreTheSame(t *testing.T) {
	var edits []config.Edit
	for _, file := range config.Defaults().CredentialFiles() {
		edits = append(edits, config.Edit{Key: file.Key, Value: file.Suggest})
	}

	patched, err := config.Patch([]byte(config.Example), edits)
	if err != nil {
		t.Fatal(err)
	}
	problems, err := config.Check(patched.Text)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Errorf("the names that are suggested are refused: %+v", problems)
	}
}

// A value that is not what the setting takes is as often a key pasted into the
// wrong place as a typing mistake, and a problem is printed, logged, and given
// to whoever asks why there is no agent. A bot token is 46 characters, so
// length alone does not keep it out.
func TestAValueThatIsNotAFileNameIsNotQuotedBackWhateverItsLength(t *testing.T) {
	const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

	for _, key := range credentialKeys {
		for _, pasted := range []string{token, "AIzaSy-pasted/key", "../outside.key", "two words"} {
			problems := problemsFor(t, textSetting(t, key, pasted), key)
			if len(problems) != 1 {
				t.Errorf("%s = %q: %d problems, want 1", key, pasted, len(problems))
				continue
			}
			if said := fmt.Sprintf("%+v", problems[0]); strings.Contains(said, pasted) || strings.Contains(said, "AAHdq") {
				t.Errorf("%s: the problem repeats the value: %s", key, said)
			}
		}
	}
}

func TestAnEntryThatIsNotTheNameOfAVariableIsRefusedAndNotQuotedBack(t *testing.T) {
	const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

	for _, file := range config.Defaults().CredentialFiles() {
		for _, pasted := range []string{token, "AIzaSyD-9tSrke72PouQMnMX-a7eZSW0jkFMBWY", "has space", "9LEADING_DIGIT", ""} {
			problems := problemsFor(t, textSetting(t, file.EnvKey, []any{"GOOD_NAME", pasted}), file.EnvKey)
			if len(problems) != 1 {
				t.Errorf("%s = [%q]: %d problems, want 1", file.EnvKey, pasted, len(problems))
				continue
			}
			if said := fmt.Sprintf("%+v", problems[0]); (pasted != "" && strings.Contains(said, pasted)) || strings.Contains(said, "AAHdq") ||
				!strings.Contains(problems[0].Why, "environment variable") || !strings.Contains(problems[0].Fix, "GEMINI_API_KEY") {
				t.Errorf("%s: the problem repeats the value or does not say what to list: %s", file.EnvKey, said)
			}
		}

		// What the defaults and the example list, and what anybody exports.
		for _, good := range [][]string{{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, {"_PRIVATE", "lower_case", "X9"}, {}} {
			if problems := problemsFor(t, textSetting(t, file.EnvKey, toAny(good)), file.EnvKey); len(problems) != 0 {
				t.Errorf("%s = %v is refused: %+v", file.EnvKey, good, problems)
			}
		}
	}
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, one := range list {
		out[i] = one
	}
	return out
}
