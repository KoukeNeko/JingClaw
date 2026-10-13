package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// Where a document's first text came from, as the history records it.
const (
	sourceFile    = "file"
	sourceEnv     = "env"
	sourceDefault = "default"
)

// savedSettings is how the saved settings are named to somebody reading a
// report about them.
const savedSettings = "the saved settings"

// legacyFiles are the files older versions kept the documents in, in the
// deployment directory. They are read once, by the first start that finds no
// document, and never written.
var legacyFiles = []struct{ document, file string }{
	{storage.DocumentConfig, home.ConfigName},
	{storage.DocumentAgents, config.InstructionsFile},
	{storage.DocumentPersona, config.PersonaFile},
}

// snapshot is the three documents as this process read them when it started.
//
// Settings are read once. Everything that runs is built from these texts, and
// what ConfigService calls the running hash is the hash of these, so a document
// somebody saves afterwards is "saved, not running yet" by comparison with them
// and not by a guess about what was read.
type snapshot struct {
	Config, Agents, Persona storage.Document
}

// readSnapshot reads the three documents.
//
// One read apiece and not one transaction: the store has no read of all three
// at once. Nothing but a second process on the same database can save between
// them, and the dashboard that could is not serving yet.
func readSnapshot(ctx context.Context, store storage.DocumentStore) (snapshot, error) {
	var docs snapshot
	for _, one := range []struct {
		name string
		into *storage.Document
	}{
		{storage.DocumentConfig, &docs.Config},
		{storage.DocumentAgents, &docs.Agents},
		{storage.DocumentPersona, &docs.Persona},
	} {
		doc, err := store.Document(ctx, one.name)
		if err != nil {
			return snapshot{}, fmt.Errorf("read the %s document: %w", one.name, err)
		}
		*one.into = doc
	}
	return docs, nil
}

// hashes are what the control plane compares a saved document against to say
// whether it is running yet.
func (s snapshot) hashes() control.StartedDocuments {
	return control.StartedDocuments{Config: s.Config.Hash, Agents: s.Agents.Hash, Persona: s.Persona.Hash}
}

// place is somewhere this process keeps a thing the saved settings cannot say
// where to find.
type place struct {
	// key is the setting an older version read it from.
	key string

	// dir is where this process keeps it.
	dir string

	// given is true when a flag or a variable chose dir. Those won over a
	// setting in the file, so the answer for such a place did not change when
	// the file stopped being read.
	given bool
}

// placesOf are the places this process keeps what the saved settings cannot
// say, one for each of config.BootstrapKeys.
func placesOf(bootstrap config.Config, database, discoveryFile string) []place {
	return []place{
		{key: "server.data_dir", dir: filepath.Dir(database), given: bootstrap.Server.DataDir != ""},
		{
			key: "server.runtime_dir", dir: filepath.Dir(discoveryFile),
			// JINGCLAW_RUNTIME_DIR names it as well, and is what the clients
			// that look for the discovery file read.
			given: bootstrap.Server.RuntimeDir != "" || os.Getenv("JINGCLAW_RUNTIME_DIR") != "",
		},
	}
}

// ensureOptions are what a first start has to go on.
type ensureOptions struct {
	// Deployment is the directory older versions kept the files in, empty when
	// this process has none.
	Deployment string

	// Env is what the environment carries, from config.SeedsFromEnvironment.
	Env []config.Seed

	// Places are where this process keeps the database and the discovery file.
	Places []place

	// Asking is a run that was asked a question about the settings, such as
	// what the prompt is, and not started.
	Asking bool
}

// ensureDocuments gives every document that has no text a first one, and says
// where the settings came from for the startup line.
//
// Only a document with no row at all. A cleared document is an empty row, so
// one somebody emptied is not given its text back, and neither is one that was
// edited: the variables are set on the service and arrive again on every
// restart.
//
// The first source that applies wins, in the order a deployment used to resolve
// them: the file an older version kept, then the variable, then for the
// configuration alone the example with every setting at its default. The agents
// and persona documents have no default; without a row they are empty, which
// the prompt already skips.
//
// Everything is saved by one call, so a crash before it changes nothing. A file
// that was imported is set aside only afterwards, and a crash between the two is
// finished by the next start.
func ensureDocuments(
	ctx context.Context,
	store storage.DocumentStore,
	opts ensureOptions,
	logger *slog.Logger,
) (string, error) {
	var (
		rows  = make(map[string]storage.Document, len(legacyFiles))
		paths = make(map[string]string, len(legacyFiles))
		seeds []storage.DocumentSeed
	)

	for _, legacy := range legacyFiles {
		row, err := store.Document(ctx, legacy.document)
		if err != nil {
			return "", fmt.Errorf("read the %s document: %w", legacy.document, err)
		}
		rows[legacy.document] = row

		if opts.Deployment != "" {
			paths[legacy.document] = filepath.Join(opts.Deployment, legacy.file)
		}
		if row.Version != 0 {
			continue
		}

		seed, found, err := firstText(legacy.document, paths[legacy.document], opts.Env)
		if err != nil {
			return "", err
		}
		if !found {
			continue
		}

		if seed.Name == storage.DocumentConfig && seed.Source != sourceDefault {
			from := paths[seed.Name]
			if seed.Source == sourceEnv {
				from = config.FileEnvVar
			}
			if err := checkPlaces(seed.Text, from, opts.Places); err != nil {
				return "", err
			}
		}
		if seed.Name == storage.DocumentConfig && seed.Source == sourceEnv {
			if err := checkRunnable(seed.Text); err != nil {
				return "", err
			}
		}
		seeds = append(seeds, seed)
	}

	written := make(map[string]storage.DocumentSeed, len(seeds))
	if len(seeds) > 0 {
		names, err := store.SeedDocuments(ctx, seeds, time.Now())
		if err != nil {
			return "", fmt.Errorf("save the settings this deployment starts with: %w", err)
		}
		for _, name := range names {
			for _, seed := range seeds {
				if seed.Name == name {
					written[name] = seed
				}
			}
		}
	}

	for name, seed := range written {
		rows[name] = storage.Document{
			Name: name, Text: seed.Text, Hash: storage.DocumentHash(seed.Text),
			Source: seed.Source, Version: 1,
		}
		logger.Info("saved a document the settings start from",
			"document", name, "from", seed.Source, "bytes", len(seed.Text))
	}

	// A row that is still the first version of a file is a file that has not
	// been set aside: just imported by this run, or by one that stopped before
	// it got this far. Any later save has made it something else, and then the
	// file is not looked at.
	//
	// Not by a question. A trial run may have been given a database of its own,
	// and then what it imported is a copy that nothing will open again: setting
	// the files aside would leave the deployment's own database to be made from
	// nothing. The next start that is not a question finds the row and finishes
	// this.
	for _, legacy := range legacyFiles {
		if opts.Asking {
			break
		}
		row := rows[legacy.document]
		if path := paths[legacy.document]; path != "" && row.Version == 1 && row.Source == sourceFile {
			setAside(path, row.Hash, logger)
		}
	}

	return describeSettings(rows, written), nil
}

// firstText is the text a document starts with, and false when nothing gives
// it one.
func firstText(document, path string, env []config.Seed) (storage.DocumentSeed, bool, error) {
	if path != "" {
		text, found, err := readLegacy(path)
		if err != nil {
			return storage.DocumentSeed{}, false, err
		}
		if found {
			return storage.DocumentSeed{Name: document, Text: text, Source: sourceFile}, true, nil
		}
	}

	for _, seed := range env {
		if seed.Document == document {
			return storage.DocumentSeed{Name: document, Text: seed.Text, Source: sourceEnv}, true, nil
		}
	}

	if document == storage.DocumentConfig {
		return storage.DocumentSeed{Name: document, Text: config.Example, Source: sourceDefault}, true, nil
	}
	return storage.DocumentSeed{}, false, nil
}

// readLegacy reads a file an older version kept a document in.
//
// A file that is there and cannot be read stops the start. Ignoring it would
// save a different text in its place, and every later start would find that
// text and never look at the file again.
func readLegacy(path string) (string, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s to keep what is in it: %w", path, err)
	}
	if !utf8.Valid(content) {
		return "", false, fmt.Errorf(
			"%s is not UTF-8 text, so it cannot be kept as a setting; save it as UTF-8, or move it away to start without it", path)
	}
	return string(content), true, nil
}

// checkPlaces refuses to save a configuration that says where the database or
// the discovery file is, when this process would put it somewhere else.
//
// The saved settings cannot hold those lines, so the first start after an
// upgrade would otherwise open an empty database in the default place and carry
// on as though nothing was wrong. That is one deployment quietly becoming two,
// and the sessions in the first are not in the second. A configuration that
// arrives in a variable says the same thing in the same way, and is held to the
// same rule.
//
// Not when a flag or a variable chose the place: those won over the file before
// as well, so the database this run opens is the one the last run did.
//
// from is where the text came from: the file, or the variable.
func checkPlaces(text, from string, places []place) error {
	set, err := config.BootstrapSettings([]byte(text))
	if err != nil {
		// A file that cannot be read is imported as it is, and corrected where
		// the daemon can be reached. It may or may not have said anything here.
		return nil
	}

	for _, p := range places {
		value := set[p.key]
		if value == "" || p.given || sameDirectory(value, p.dir) {
			continue
		}

		var by config.BootstrapKey
		for _, key := range config.BootstrapKeys() {
			if key.Key == p.key {
				by = key
			}
		}
		given := by.Env + "=" + value
		if by.Flag != "" {
			given += " (or " + by.Flag + " " + value + ")"
		}

		return fmt.Errorf(
			"%s sets %s to %s, but the settings are kept in the database now and cannot say that, "+
				"and this process would use %s instead. Starting would open a different database from the one these settings were made for. "+
				"Start with %s, or remove the line to use %s on purpose",
			from, p.key, value, p.dir, given, p.dir)
	}
	return nil
}

// sameDirectory says whether two spellings are the one directory.
func sameDirectory(a, b string) bool {
	clean := func(path string) string {
		if absolute, err := filepath.Abs(path); err == nil {
			return absolute
		}
		return filepath.Clean(path)
	}
	return clean(a) == clean(b)
}

// checkRunnable refuses a configuration the environment carries that no start
// could use, before it is saved.
//
// A file is imported as it is, because somebody has it and can correct it in
// the dashboard. A variable is not like that: the text is saved once, and the
// variable arrives again on every restart without ever replacing it, so a
// mistake saved from one outlasts correcting the variable, and the dashboard it
// would have to be corrected in cannot be reached from a container. Refused
// here, the next start is a first start again, with the corrected variable.
func checkRunnable(text string) error {
	problems, err := config.Check([]byte(text))
	if err != nil {
		return fmt.Errorf("%s cannot be read as settings, so nothing was saved: %w", config.FileEnvVar, err)
	}
	if len(problems) > 0 {
		return &config.InvalidError{Problems: problems}
	}
	return nil
}

// setAside renames an imported file to <name>.imported, so that it is not
// mistaken for the settings it was.
//
// Never deleted, and never when the file is no longer the one that was
// imported: somebody has edited it since, and renaming it away would hide
// that. A file that cannot be renamed, or whose new name is taken, is only
// warned about; the settings are in the database either way, and refusing to
// start over a read-only mount would be refusing over nothing.
func setAside(path, imported string, logger *slog.Logger) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		logger.Warn("could not check an imported settings file before setting it aside",
			"file", path, "error", err)
		return
	}

	if storage.DocumentHash(string(content)) != imported {
		logger.Warn("a settings file is still here and is not what was imported; "+
			"it is not used, and it is left as it is",
			"file", path,
			"hint", "the settings are in the database now; edit them in the dashboard")
		return
	}

	target := path + ".imported"
	if _, err := os.Lstat(target); err == nil {
		logger.Warn("an imported settings file could not be set aside, because its new name is taken",
			"file", path, "name", target)
		return
	}
	if err := os.Rename(path, target); err != nil {
		logger.Warn("an imported settings file could not be set aside", "file", path, "error", err)
		return
	}
	logger.Info("an imported settings file was set aside", "file", path, "now", target)
}

// describeSettings is the startup line's account of where the settings are:
// what this run made them from if it made them, and otherwise how many
// documents there are.
func describeSettings(rows map[string]storage.Document, written map[string]storage.DocumentSeed) string {
	if seed, made := written[storage.DocumentConfig]; made {
		switch seed.Source {
		case sourceEnv:
			return "database (created from " + config.FileEnvVar + ")"
		case sourceFile:
			return "database (imported from " + home.ConfigName + ")"
		}
		return "database (created, all defaults)"
	}

	var count int
	for _, row := range rows {
		if row.Version != 0 {
			count++
		}
	}
	if count == 1 {
		return "database (1 document)"
	}
	return fmt.Sprintf("database (%d documents)", count)
}

// settingsProblems are the reasons the saved settings cannot be used, in the
// form the control plane reports them.
func settingsProblems(err error) []control.AgentProblem {
	var invalid *config.InvalidError
	if errors.As(err, &invalid) {
		problems := make([]control.AgentProblem, len(invalid.Problems))
		for i, problem := range invalid.Problems {
			problems[i] = control.AgentProblem{Problem: problem, Source: "settings"}
		}
		return problems
	}

	return []control.AgentProblem{{
		Problem: config.Problem{
			Why: printable(err.Error()),
			Fix: "Correct the text in the dashboard, or restore an earlier version, then restart: jingclaw stop, then jingclaw.",
		},
		Source: "settings",
	}}
}

// startupProblem is a reason the agent could not be built that is not a setting
// being wrong: a credential that is missing, a provider that cannot be reached,
// a program that is not installed.
//
// The message is what the daemon used to print before it exited, and it already
// says what to do about the cause; the fix added here is only how to try again,
// because there is no restart to ask for.
func startupProblem(err error) control.AgentProblem {
	return control.AgentProblem{
		Problem: config.Problem{
			Why: printable(err.Error()),
			Fix: "Put that right, or change the setting that asks for it, then restart: jingclaw stop, then jingclaw.",
		},
		Source: "startup",
	}
}

// printable is text with what a terminal would act on taken out.
//
// The words of a parse error are the file's own, and the file may have come from
// anywhere. An escape sequence in one would be run by whatever shows the reason
// to somebody: the banner, the log, the supervisor's line, the dashboard command.
// Lines and tabs are words' layout and stay.
func printable(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return '?'
		}
		return r
	}, text)
}

// describeProblem is a problem on one line, for a terminal and for a log.
func describeProblem(problem control.AgentProblem) string {
	if problem.Key == "" {
		return problem.Why
	}
	return fmt.Sprintf("%s %s %s", problem.Key, problem.Value, problem.Why)
}
