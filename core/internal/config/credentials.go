package config

import (
	"regexp"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// CredentialFile is one setting that names a credential file, with the setting
// beside it that lists the environment variables for the same credential.
//
// Eight of them, and the only settings that name a file inside the deployment
// directory. They are listed in one place so that the rule for what such a name
// may be, the check that applies it, and the service that writes the files
// cannot come to disagree about which settings are meant.
type CredentialFile struct {
	// Slot names the credential: provider.gemini, web.search, gateway.discord.
	Slot string

	// Key is the setting that names the file, and Name what it holds. Empty
	// means the credential has no file and can only come from the environment.
	Key  string
	Name string

	// EnvKey is the setting that lists the variables, and Env what it holds, in
	// the order they are looked at.
	EnvKey string
	Env    []string

	// Suggest is the name to offer when Name is empty.
	Suggest string
}

// CredentialFiles are the settings that name a credential file, as this
// configuration has them.
func (c Config) CredentialFiles() []CredentialFile {
	return []CredentialFile{
		{
			Slot: "provider.gemini", Key: "provider.gemini.api_key_file", Name: c.Provider.Gemini.APIKeyFile,
			EnvKey: "provider.gemini.api_key_env", Env: c.Provider.Gemini.APIKeyEnv, Suggest: "gemini.key",
		},
		{
			Slot: "provider.anthropic", Key: "provider.anthropic.api_key_file", Name: c.Provider.Anthropic.APIKeyFile,
			EnvKey: "provider.anthropic.api_key_env", Env: c.Provider.Anthropic.APIKeyEnv, Suggest: "anthropic.key",
		},
		{
			Slot: "provider.ollama", Key: "provider.ollama.api_key_file", Name: c.Provider.Ollama.APIKeyFile,
			EnvKey: "provider.ollama.api_key_env", Env: c.Provider.Ollama.APIKeyEnv, Suggest: "ollama.key",
		},
		{
			Slot: "provider.openai_compat", Key: "provider.openai_compat.api_key_file", Name: c.Provider.OpenAICompat.APIKeyFile,
			EnvKey: "provider.openai_compat.api_key_env", Env: c.Provider.OpenAICompat.APIKeyEnv, Suggest: "openai-compat.key",
		},
		{
			Slot: "memory.embedding", Key: "memory.embedding.api_key_file", Name: c.Memory.Embedding.APIKeyFile,
			EnvKey: "memory.embedding.api_key_env", Env: c.Memory.Embedding.APIKeyEnv, Suggest: "embedding.key",
		},
		{
			Slot: "web.search", Key: "web.search.key_file", Name: c.Web.Search.KeyFile,
			EnvKey: "web.search.key_env", Env: c.Web.Search.KeyEnv, Suggest: "brave-search.token",
		},
		{
			Slot: "gateway.discord", Key: "gateway.discord.token_file", Name: c.Gateway.Discord.TokenFile,
			EnvKey: "gateway.discord.token_env", Env: c.Gateway.Discord.TokenEnv, Suggest: "discord.token",
		},
		{
			Slot: "gateway.telegram", Key: "gateway.telegram.token_file", Name: c.Gateway.Telegram.TokenFile,
			EnvKey: "gateway.telegram.token_env", Env: c.Gateway.Telegram.TokenEnv, Suggest: "telegram.token",
		},
	}
}

// credentialFileName is what a credential file's name looks like: one component,
// starting with something that is not a dot, from characters no platform treats
// as anything but part of a name.
var credentialFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// envName is what the name of an environment variable looks like in a setting
// that lists them.
//
// A pasted key made only of letters, digits and underscores is also a name, and
// is not caught; what this stops is the commoner kind with a dash, a colon or a
// length no variable has, which a setting that is meant to hold names would
// otherwise print back.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsEnvName is whether text can be the name of an environment variable that a
// credential is read from.
func IsEnvName(text string) bool { return envName.MatchString(text) }

// keptNames are names the deployment directory already uses for something else.
//
// Compared without regard to case, because the directory may be on a file
// system that does not tell Config.toml from config.toml. mcp-auth and sandbox
// are the directories the daemon makes there for its own use, written out here
// because this package cannot import the ones that make them.
var keptNames = []string{
	home.ConfigName, InstructionsFile, PersonaFile,
	home.WorkspaceName, home.DataName, home.RunName, home.LogName, home.BinName, home.SkillsName,
	"mcp-auth", "sandbox",
}

// credentialNameFix says what a name may be. The one text for the check at
// start and for the service that is asked to write the file, so that the rule
// is said the same wherever it refuses something.
const credentialNameFix = "Give a plain file name for a file directly inside the deployment directory: " +
	"up to 64 letters, digits, '.', '_' or '-', starting with a letter or digit and not ending in '.', " +
	`such as "gemini.key". It cannot be a path, a name this deployment keeps for itself ` +
	"(config.toml, AGENTS.md, PERSONA.md, workspace, data, run, log, bin, skills, mcp-auth, sandbox), " +
	"a backup name (*.bak.*), or the name of a file that was imported (*.imported)."

// notShown stands for a value that is not quoted back. A problem is printed,
// logged and shown to whoever asks why there is no agent, and a value that is
// not what the setting takes is as often a key pasted into the wrong place as a
// typing mistake.
var notShown = quote("(not shown)")

// CredentialFileProblems is every credential file name the settings are not
// allowed to hold: one that is not a plain name, and one this deployment keeps
// for something else.
//
// Checked for all eight whichever backend or platform is selected. A name is
// joined onto the deployment directory and read, and a name that climbs out of
// it made the daemon read a mode-600 file outside the deployment and send what
// was in it to a provider as a key; refusing it only when it was about to be
// used would leave the next change of backend to find that out.
//
// A name that is not a plain one is not quoted back. A kept name is, because it
// is made of plain characters and is what the person has to change.
func (c Config) CredentialFileProblems() []Problem {
	var problems []Problem

	for _, file := range c.CredentialFiles() {
		switch {
		case file.Name == "":
		case !credentialFileName.MatchString(file.Name) || strings.HasSuffix(file.Name, "."):
			problems = append(problems, Problem{
				Key: file.Key, Value: notShown,
				Why: "is not a name a credential file can have",
				Fix: credentialNameFix,
			})
		case keptName(file.Name):
			problems = append(problems, Problem{
				Key: file.Key, Value: quote(file.Name),
				Why: "is a name this deployment keeps for something else",
				Fix: credentialNameFix,
			})
		}
	}

	return problems
}

// CredentialEnvProblems is every entry in the settings that list the variables
// a credential is read from that is not the name of one.
func (c Config) CredentialEnvProblems() []Problem {
	var problems []Problem

	for _, file := range c.CredentialFiles() {
		for _, name := range file.Env {
			if IsEnvName(name) {
				continue
			}
			problems = append(problems, Problem{
				Key: file.EnvKey, Value: notShown,
				Why: "lists something that is not the name of an environment variable",
				Fix: `List the names of the variables the key is read from, such as "GEMINI_API_KEY": ` +
					"letters, digits and underscores, not starting with a digit. The key itself goes in " +
					"the variable or in the credential file, never in this setting.",
			})
		}
	}

	return problems
}

// SharedCredentialFileProblems is every credential file name that two settings
// give.
//
// Not part of Validate: a deployment that keeps one key for a provider and for
// the embeddings on the same endpoint in one file works, and refusing to start
// over it would turn an upgrade into a repair. What it cannot have is a
// dashboard that writes it, because a save to either credential would be a save
// to both, so the service that writes the files asks.
func (c Config) SharedCredentialFileProblems() []Problem {
	var (
		problems []Problem
		files    = c.CredentialFiles()
		holders  = make(map[string][]string, len(files))
	)

	for _, file := range files {
		if file.Name != "" {
			holders[strings.ToLower(file.Name)] = append(holders[strings.ToLower(file.Name)], file.Key)
		}
	}

	for _, file := range files {
		keys := holders[strings.ToLower(file.Name)]
		if file.Name == "" || len(keys) < 2 || !credentialFileName.MatchString(file.Name) {
			continue
		}

		var others []string
		for _, key := range keys {
			if key != file.Key {
				others = append(others, key)
			}
		}
		problems = append(problems, Problem{
			Key: file.Key, Value: quote(file.Name),
			Why: "names the same file as " + strings.Join(others, " and "),
			Fix: "Two credentials in one file would each overwrite the other, so this cannot save to it. " +
				"Give each its own plain file name, such as " + quote(file.Suggest) + " for this one.",
		})
	}

	return problems
}

// keptName is whether the deployment directory already uses a name for
// something that is not a credential.
func keptName(name string) bool {
	for _, kept := range keptNames {
		if strings.EqualFold(name, kept) {
			return true
		}
	}

	lower := strings.ToLower(name)
	// The backups older versions kept beside the settings files, and the settings
	// files themselves once they were imported: a credential written or deleted
	// there would be the loss of the only copy of what was in them.
	return strings.Contains(lower, ".bak.") || strings.HasSuffix(lower, ".imported")
}
