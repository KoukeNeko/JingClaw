package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// ensuring is a first start's view of the deployment, so that a test says only
// what differs from a plain one.
type ensuring struct {
	t     *testing.T
	store *memory.Store
	root  string
	env   []config.Seed
	logs  *bytes.Buffer

	// places say where this process keeps the database and the discovery file,
	// and that nothing but the default chose either.
	places []place

	// asking is a run that was asked a question rather than started.
	asking bool
}

func newEnsuring(t *testing.T) *ensuring {
	t.Helper()

	root := t.TempDir()
	return &ensuring{
		t: t, store: memory.New(), root: root, logs: &bytes.Buffer{},
		places: []place{
			{key: "server.data_dir", dir: filepath.Join(root, "data")},
			{key: "server.runtime_dir", dir: filepath.Join(root, "run")},
		},
	}
}

// file puts a file where an older version kept a document.
func (e *ensuring) file(name, text string) string {
	e.t.Helper()

	path := filepath.Join(e.root, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// ensure is one start's worth of ensureDocuments.
func (e *ensuring) ensure() (string, error) {
	e.t.Helper()

	return ensureDocuments(context.Background(), e.store, ensureOptions{
		Deployment: e.root, Env: e.env, Places: e.places, Asking: e.asking,
	}, slog.New(slog.NewTextHandler(e.logs, nil)))
}

func (e *ensuring) mustEnsure() string {
	e.t.Helper()

	said, err := e.ensure()
	if err != nil {
		e.t.Fatalf("ensure: %v", err)
	}
	return said
}

func (e *ensuring) doc(name string) storage.Document {
	e.t.Helper()

	doc, err := e.store.Document(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return doc
}

func (e *ensuring) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(e.root, name))
	return err == nil
}

// A deployment with nothing to go on gets the defaults, and no instructions.
//
// The configuration is the example with every setting at its default, so there
// is a text to edit from the first minute. The two instruction documents have
// no row at all: absent is empty, the prompt already skips an empty one, and a
// stub that nobody wrote would be put in front of the model as though somebody
// had.
func TestAFreshDeploymentGetsTheDefaultsAndNoInstructions(t *testing.T) {
	e := newEnsuring(t)

	said := e.mustEnsure()

	saved := e.doc(storage.DocumentConfig)
	if saved.Text != config.Example || saved.Source != sourceDefault || saved.Version != 1 {
		t.Errorf("config is %d bytes from %q at version %d, want the example from %q at version 1",
			len(saved.Text), saved.Source, saved.Version, sourceDefault)
	}
	for _, name := range []string{storage.DocumentAgents, storage.DocumentPersona} {
		if doc := e.doc(name); doc.Version != 0 {
			t.Errorf("%s has a row at version %d, want none", name, doc.Version)
		}
	}
	if said != "database (created, all defaults)" {
		t.Errorf("the startup line says %q", said)
	}
}

// A platform whose only input is variables supplies the documents that way, and
// what it supplied is what runs, not the defaults.
func TestTheEnvironmentSuppliesTheDocumentsItCarries(t *testing.T) {
	e := newEnsuring(t)
	e.env = []config.Seed{
		{Document: storage.DocumentConfig, Text: "[agent]\nmax_iterations = 3\n"},
		{Document: storage.DocumentPersona, Text: "You are 江委員.\n"},
	}

	said := e.mustEnsure()

	for name, want := range map[string]string{
		storage.DocumentConfig:  "[agent]\nmax_iterations = 3\n",
		storage.DocumentPersona: "You are 江委員.\n",
	} {
		if doc := e.doc(name); doc.Text != want || doc.Source != sourceEnv {
			t.Errorf("%s is %q from %q, want %q from %q", name, doc.Text, doc.Source, want, sourceEnv)
		}
	}
	if doc := e.doc(storage.DocumentAgents); doc.Version != 0 {
		t.Errorf("a document the environment did not carry has a row at version %d", doc.Version)
	}
	if !strings.Contains(said, config.FileEnvVar) || strings.Contains(said, "defaults") {
		t.Errorf("the startup line says %q; the settings came from %s, which is not the defaults",
			said, config.FileEnvVar)
	}
}

// The variables are decoded by the helper the config package owns, so a value
// that had to be encoded to survive the trip is stored as what it decodes to.
func TestABase64VariableIsStoredDecoded(t *testing.T) {
	persona := "你是江委員。\nAnswer briefly.\n"
	t.Setenv(config.PersonaEnvVar, "base64:"+base64.StdEncoding.EncodeToString([]byte(persona)))
	t.Setenv(config.FileEnvVar, "")
	t.Setenv(config.InstructionsEnvVar, "")

	seeds, err := config.SeedsFromEnvironment()
	if err != nil {
		t.Fatalf("seeds: %v", err)
	}

	e := newEnsuring(t)
	e.env = seeds
	e.mustEnsure()

	if doc := e.doc(storage.DocumentPersona); doc.Text != persona {
		t.Errorf("the persona is stored as %q, want %q", doc.Text, persona)
	}
	if doc := e.doc(storage.DocumentConfig); doc.Source != sourceDefault {
		t.Errorf("a blank %s made the config come from %q", config.FileEnvVar, doc.Source)
	}
}

// A file an older version kept is the settings, and is kept as they were.
//
// Imported as it is: the comments, the lines this version does not know and a
// value that is wrong stay exactly as they were written. Whether it is usable
// is for the daemon to say once it has read it, where the person can correct it.
func TestALegacyFileIsImportedAsItIsAndSetAside(t *testing.T) {
	e := newEnsuring(t)

	texts := map[string]string{
		storage.DocumentConfig:  "# mine\n[agent]\nmax_iterations = 5\nthis_is_unknown = true\n[provider]\nbackend = \"nonsense\"\n",
		storage.DocumentAgents:  "# AGENTS\n\nPrefer table-driven tests.\n",
		storage.DocumentPersona: "# PERSONA\n\nNever chatty.\n",
	}
	files := map[string]string{
		storage.DocumentConfig:  home.ConfigName,
		storage.DocumentAgents:  config.InstructionsFile,
		storage.DocumentPersona: config.PersonaFile,
	}
	for name, text := range texts {
		e.file(files[name], text)
	}
	backup := e.file(home.ConfigName+".bak.1", "an older one")

	said := e.mustEnsure()

	for name, text := range texts {
		doc := e.doc(name)
		if doc.Text != text || doc.Source != sourceFile || doc.Version != 1 {
			t.Errorf("%s is %q from %q at version %d, want %q from %q at version 1",
				name, doc.Text, doc.Source, doc.Version, text, sourceFile)
		}

		if e.exists(files[name]) {
			t.Errorf("%s is still in the deployment directory", files[name])
		}
		kept, err := os.ReadFile(filepath.Join(e.root, files[name]+".imported"))
		if err != nil || string(kept) != text {
			t.Errorf("%s.imported is %q (%v), want what the file held", files[name], kept, err)
		}
	}

	if _, err := os.Stat(backup); err != nil {
		t.Errorf("a backup was touched: %v", err)
	}
	if said != "database (imported from "+home.ConfigName+")" {
		t.Errorf("the startup line says %q", said)
	}
}

// The file wins over the variable, as the file's existence did before: the
// variable arrives again on every restart of a service, and was only ever for
// a first start that had nothing else.
func TestALegacyFileBeatsTheEnvironment(t *testing.T) {
	e := newEnsuring(t)
	e.env = []config.Seed{
		{Document: storage.DocumentConfig, Text: "[agent]\nmax_iterations = 3\n"},
		{Document: storage.DocumentPersona, Text: "from the environment\n"},
	}
	e.file(home.ConfigName, "[agent]\nmax_iterations = 9\n")

	e.mustEnsure()

	if doc := e.doc(storage.DocumentConfig); doc.Text != "[agent]\nmax_iterations = 9\n" || doc.Source != sourceFile {
		t.Errorf("the config is %q from %q, want the file's", doc.Text, doc.Source)
	}
	// And only for the document that has the file: the others take what is
	// left.
	if doc := e.doc(storage.DocumentPersona); doc.Text != "from the environment\n" || doc.Source != sourceEnv {
		t.Errorf("the persona is %q from %q, want the environment's", doc.Text, doc.Source)
	}
}

// A document that has a row is never given a first text again, however it got
// the row and whatever is set or lying about now.
//
// Including one that was cleared. An empty row is what "I removed it" looks
// like, and the variable that arrives on every restart must not bring it back.
func TestADocumentThatHasARowIsNeverSeededAgain(t *testing.T) {
	e := newEnsuring(t)
	ctx := context.Background()

	if _, err := e.store.PutDocument(ctx, storage.DocumentConfig, "[agent]\nmax_iterations = 4\n", "web", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.PutDocument(ctx, storage.DocumentPersona, "", "web", "", time.Now()); err != nil {
		t.Fatal(err)
	}

	e.env = []config.Seed{
		{Document: storage.DocumentConfig, Text: "from the environment\n"},
		{Document: storage.DocumentPersona, Text: "from the environment\n"},
	}

	// Where the files would be, and such that reading either would fail: a
	// start that has the rows reads nothing from disk, and this is how a test
	// can tell that it did not.
	for _, name := range []string{home.ConfigName, config.PersonaFile} {
		if err := os.Mkdir(filepath.Join(e.root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	said := e.mustEnsure()

	if doc := e.doc(storage.DocumentConfig); doc.Text != "[agent]\nmax_iterations = 4\n" || doc.Version != 1 {
		t.Errorf("the config was replaced: %q at version %d", doc.Text, doc.Version)
	}
	if doc := e.doc(storage.DocumentPersona); doc.Text != "" || doc.Version != 1 {
		t.Errorf("a cleared persona was brought back: %q at version %d", doc.Text, doc.Version)
	}
	if doc := e.doc(storage.DocumentAgents); doc.Version != 0 {
		t.Errorf("agents has a row at version %d, want none", doc.Version)
	}

	// Edited, so not the first version of a file, and nobody looks at the disk.
	for _, name := range []string{home.ConfigName, config.PersonaFile} {
		if !e.exists(name) {
			t.Errorf("%s was moved though its document was not imported from it", name)
		}
	}
	if said != "database (2 documents)" {
		t.Errorf("the startup line says %q", said)
	}
}

// A refused start leaves nothing behind it: the next one is a first start
// again, and can be corrected by fixing what the error names.
func TestAnUndecodableVariableLeavesNothing(t *testing.T) {
	t.Setenv(config.FileEnvVar, "[agent]\nmax_iterations = 3\n")
	t.Setenv(config.PersonaEnvVar, "base64:this is not base64!")
	t.Setenv(config.InstructionsEnvVar, "")

	root := filepath.Join(t.TempDir(), home.DirName)
	t.Setenv(home.EnvVar, root)
	for _, name := range []string{"JINGCLAW_SERVER_DATA_DIR", "JINGCLAW_SERVER_RUNTIME_DIR", "JINGCLAW_RUNTIME_DIR"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}

	err := runWithin(t)
	if err == nil || !strings.Contains(err.Error(), config.PersonaEnvVar) {
		t.Fatalf("a variable that does not decode did not stop the start by name: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		listing, _ := os.ReadDir(root)
		t.Errorf("the refused start created %s (%d entries)", root, len(listing))
	}
}

// A line in the environment's configuration must still be TOML, and the start
// that finds it is not is refused the same way.
func TestAConfigVariableThatIsNotTOMLLeavesNothing(t *testing.T) {
	t.Setenv(config.FileEnvVar, "this is = not [toml")
	t.Setenv(config.PersonaEnvVar, "")
	t.Setenv(config.InstructionsEnvVar, "")

	root := filepath.Join(t.TempDir(), home.DirName)
	t.Setenv(home.EnvVar, root)

	if err := runWithin(t); err == nil || !strings.Contains(err.Error(), config.FileEnvVar) {
		t.Fatalf("the start was not refused by name: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the refused start created %s", root)
	}
}

// The first start after an upgrade must not quietly open a different database.
//
// The saved settings cannot say where the database is, so a file that did is
// not obeyed any more. Without this, the first start would open an empty
// database in the default place and carry on, and the sessions in the other
// would be a deployment nobody was running.
func TestALegacyFileThatNamesWhereTheDatabaseIsIsRefusedWhenThisProcessWouldUseAnother(t *testing.T) {
	e := newEnsuring(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	e.file(home.ConfigName, "[server]\ndata_dir = "+quoted(elsewhere)+"\n")
	e.file(config.PersonaFile, "mine\n")

	_, err := e.ensure()
	if err == nil {
		t.Fatal("it imported a configuration that says the database is somewhere else")
	}
	for _, want := range []string{"JINGCLAW_SERVER_DATA_DIR=" + elsewhere, "--data-dir " + elsewhere} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}

	// Refused before anything is written or moved, or the second attempt would
	// find the documents there and never look at the file again.
	for _, name := range []string{storage.DocumentConfig, storage.DocumentAgents, storage.DocumentPersona} {
		if doc := e.doc(name); doc.Version != 0 {
			t.Errorf("%s was saved by a start that was refused", name)
		}
	}
	if !e.exists(home.ConfigName) || !e.exists(config.PersonaFile) {
		t.Error("a file was set aside by a start that was refused")
	}
}

func TestALegacyFileThatNamesWhereTheDiscoveryFileIsIsRefusedToo(t *testing.T) {
	e := newEnsuring(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	e.file(home.ConfigName, "[server]\nruntime_dir = "+quoted(elsewhere)+"\n")

	_, err := e.ensure()
	if err == nil || !strings.Contains(err.Error(), "JINGCLAW_SERVER_RUNTIME_DIR="+elsewhere) {
		t.Fatalf("the refusal does not say how to give it: %v", err)
	}
}

// And not when it would be the same place, or when something other than the
// file chose it, which is what won before as well. Refusing then would be
// refusing a deployment that works.
func TestALegacyFileIsNotRefusedWhenThePlaceIsTheSameOrWasGivenAnotherWay(t *testing.T) {
	t.Run("the same place", func(t *testing.T) {
		e := newEnsuring(t)
		e.file(home.ConfigName, "[server]\ndata_dir = "+quoted(e.places[0].dir+string(filepath.Separator))+"\n"+
			"runtime_dir = "+quoted(e.places[1].dir)+"\n")

		if _, err := e.ensure(); err != nil {
			t.Fatalf("refused a file that names where this process already is: %v", err)
		}
	})

	t.Run("a flag or a variable chose it", func(t *testing.T) {
		e := newEnsuring(t)
		e.places[0].given = true
		e.places[1].given = true
		e.file(home.ConfigName, "[server]\ndata_dir = \"/somewhere/else\"\nruntime_dir = \"/and/here\"\n")

		if _, err := e.ensure(); err != nil {
			t.Fatalf("refused a file whose line was overridden before as well: %v", err)
		}
	})

	t.Run("a file that cannot be read says nothing", func(t *testing.T) {
		e := newEnsuring(t)
		e.file(home.ConfigName, "this is = not [toml")

		if _, err := e.ensure(); err != nil {
			t.Fatalf("a configuration that does not read was refused rather than imported: %v", err)
		}
		if doc := e.doc(storage.DocumentConfig); doc.Text != "this is = not [toml" {
			t.Errorf("it was not imported as it was: %q", doc.Text)
		}
	})

	t.Run("a file that names neither", func(t *testing.T) {
		e := newEnsuring(t)
		e.file(home.ConfigName, "[agent]\nmax_iterations = 2\n")

		if _, err := e.ensure(); err != nil {
			t.Fatalf("refused a file that names nowhere: %v", err)
		}
	})
}

// Every setting the saved text cannot hold needs a place to be compared with,
// or adding one to the config package would leave the start after an upgrade
// unguarded for it.
func TestEveryBootstrapKeyHasAPlaceToBeCheckedAgainst(t *testing.T) {
	places := placesOf(config.Defaults(), "/data/jingclaw.db", "/run/daemon.json")

	got := map[string]bool{}
	for _, p := range places {
		got[p.key] = true
	}
	for _, key := range config.BootstrapKeys() {
		if !got[key.Key] {
			t.Errorf("%s has no place, so a file that sets it is imported without a check", key.Key)
		}
	}
	if len(places) != len(config.BootstrapKeys()) {
		t.Errorf("%d places for %d bootstrap keys", len(places), len(config.BootstrapKeys()))
	}
}

// A file somebody edited since it was imported is not theirs to hide.
//
// Seen when a start is interrupted between the save and the rename, and the
// file is changed before the next one. The settings are in the database and the
// file is not what they were made from, so it is left where it is and said so.
func TestAFileThatNoLongerMatchesWhatWasImportedIsLeftAloneWithAWarning(t *testing.T) {
	e := newEnsuring(t)
	ctx := context.Background()

	if _, err := e.store.SeedDocuments(ctx, []storage.DocumentSeed{
		{Name: storage.DocumentPersona, Text: "what was imported\n", Source: sourceFile},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	path := e.file(config.PersonaFile, "edited since\n")

	e.mustEnsure()

	if !e.exists(config.PersonaFile) || e.exists(config.PersonaFile+".imported") {
		t.Error("a file that is not the one imported was set aside")
	}
	if got, _ := os.ReadFile(path); string(got) != "edited since\n" {
		t.Errorf("the file was changed: %q", got)
	}
	if !strings.Contains(e.logs.String(), "level=WARN") || !strings.Contains(e.logs.String(), config.PersonaFile) {
		t.Errorf("nothing warned about it:\n%s", e.logs)
	}
	if doc := e.doc(storage.DocumentPersona); doc.Text != "what was imported\n" {
		t.Errorf("the document was replaced by the file: %q", doc.Text)
	}
}

// A second run is a run with rows.
//
// Whatever turns up in the directory afterwards for a document that has one is
// not the settings: it is not imported, not moved, and a document that has been
// edited since is not even looked at.
func TestASecondRunImportsNothingFromDisk(t *testing.T) {
	e := newEnsuring(t)
	e.file(home.ConfigName, "[agent]\nmax_iterations = 5\n")
	e.file(config.PersonaFile, "mine\n")
	e.mustEnsure()

	// Edited in the dashboard since: the second version, from the web.
	ctx := context.Background()
	persona := e.doc(storage.DocumentPersona)
	if _, err := e.store.PutDocument(ctx, storage.DocumentPersona, "edited here\n", "web", persona.Hash, time.Now()); err != nil {
		t.Fatal(err)
	}

	// A file turns up again, as one restored from a backup does.
	e.file(home.ConfigName, "[agent]\nmax_iterations = 11\n")
	e.file(config.PersonaFile, "a stale copy\n")
	e.logs.Reset()

	e.mustEnsure()

	if doc := e.doc(storage.DocumentConfig); doc.Text != "[agent]\nmax_iterations = 5\n" || doc.Version != 1 {
		t.Errorf("the config is %q at version %d, want what was imported", doc.Text, doc.Version)
	}
	if doc := e.doc(storage.DocumentPersona); doc.Text != "edited here\n" || doc.Version != 2 {
		t.Errorf("the persona is %q at version %d, want the edit", doc.Text, doc.Version)
	}

	for _, name := range []string{home.ConfigName, config.PersonaFile} {
		if !e.exists(name) {
			t.Errorf("%s was moved by a run that had nothing to import", name)
		}
	}

	// The persona has been edited, so the stale copy is nothing to do with it.
	if strings.Contains(e.logs.String(), config.PersonaFile) {
		t.Errorf("a document that was edited since had its file looked at:\n%s", e.logs)
	}
}

// A start that stopped between saving and renaming is finished by the next.
func TestAnImportThatStoppedBeforeTheRenameIsFinishedByTheNextStart(t *testing.T) {
	e := newEnsuring(t)
	text := "# PERSONA\n\nNever chatty.\n"
	if _, err := e.store.SeedDocuments(context.Background(), []storage.DocumentSeed{
		{Name: storage.DocumentPersona, Text: text, Source: sourceFile},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.file(config.PersonaFile, text)

	e.mustEnsure()

	if e.exists(config.PersonaFile) {
		t.Error("the file is still there")
	}
	if kept, _ := os.ReadFile(filepath.Join(e.root, config.PersonaFile+".imported")); string(kept) != text {
		t.Errorf("the set-aside file is %q", kept)
	}

	// And a third start has nothing left to finish.
	e.logs.Reset()
	e.mustEnsure()
	if strings.Contains(e.logs.String(), "level=WARN") {
		t.Errorf("a finished import is warned about on every start:\n%s", e.logs)
	}
}

// A name that is taken is a reason to warn and carry on.
func TestAnImportedFileWhoseNewNameIsTakenIsLeftWithAWarning(t *testing.T) {
	e := newEnsuring(t)
	e.file(config.PersonaFile, "mine\n")
	e.file(config.PersonaFile+".imported", "an earlier one\n")

	e.mustEnsure()

	if doc := e.doc(storage.DocumentPersona); doc.Text != "mine\n" {
		t.Errorf("it was not imported: %q", doc.Text)
	}
	if !e.exists(config.PersonaFile) {
		t.Error("the file was moved over the one in its way")
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, config.PersonaFile+".imported")); string(got) != "an earlier one\n" {
		t.Errorf("the file in the way was replaced: %q", got)
	}
	if !strings.Contains(e.logs.String(), "level=WARN") {
		t.Errorf("nothing warned about it:\n%s", e.logs)
	}
}

// A mount nobody can write to is a reason to warn and carry on, not a reason
// not to start: the settings are in the database either way.
func TestAnImportedFileThatCannotBeRenamedDoesNotStopTheStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory modes are not how this is refused there")
	}

	e := newEnsuring(t)
	e.file(config.PersonaFile, "mine\n")
	if err := os.Chmod(e.root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.root, 0o700) })

	// Root writes where a mode says nobody may, and then there is no failure to
	// have.
	if probe, err := os.Create(filepath.Join(e.root, "probe")); err == nil {
		_ = probe.Close()
		t.Skip("this user can write to a directory whose mode forbids it")
	}

	e.mustEnsure()

	if doc := e.doc(storage.DocumentPersona); doc.Text != "mine\n" {
		t.Errorf("it was not imported: %q", doc.Text)
	}
	if !e.exists(config.PersonaFile) {
		t.Error("the file was moved from a directory that cannot be written to")
	}
	if !strings.Contains(e.logs.String(), "could not be set aside") {
		t.Errorf("nothing warned about it:\n%s", e.logs)
	}
}

// A file that is there and cannot be read must not be skipped: the start would
// save the defaults in its place, and no later one would look at the file.
func TestAFileThatCannotBeReadStopsTheStart(t *testing.T) {
	e := newEnsuring(t)
	// A directory where the file should be: unreadable as a file on every
	// platform and for every user, which a permission bit is not for root.
	if err := os.Mkdir(filepath.Join(e.root, home.ConfigName), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := e.ensure()
	if err == nil || !strings.Contains(err.Error(), home.ConfigName) {
		t.Fatalf("a configuration that could not be read did not stop the start by name: %v", err)
	}
	if doc := e.doc(storage.DocumentConfig); doc.Version != 0 {
		t.Errorf("the defaults were saved in its place: %q", doc.Text)
	}
}

// Text that is not UTF-8 cannot be a setting: it would be stored, and then
// could not be sent to the page that has to show it.
func TestAFileThatIsNotTextStopsTheStartAndSaysSo(t *testing.T) {
	e := newEnsuring(t)
	e.file(config.PersonaFile, "caf\xe9\n")

	_, err := e.ensure()
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("a file that is not UTF-8 was not refused as one: %v", err)
	}
	if doc := e.doc(storage.DocumentConfig); doc.Version != 0 {
		t.Error("something was saved by a start that was refused")
	}
}

// The line says what this run did, and a restart did nothing.
//
// The variable stays set on every later run, and only the first created
// anything. A line that reported the variable rather than the run would claim,
// on every restart, to have just made settings it did not touch — while the
// ones being read might be ones somebody edited since.
func TestALaterRunDoesNotClaimToHaveCreatedTheSettings(t *testing.T) {
	e := newEnsuring(t)
	e.env = []config.Seed{{Document: storage.DocumentConfig, Text: "[agent]\nmax_iterations = 3\n"}}

	if first := e.mustEnsure(); !strings.Contains(first, "created from "+config.FileEnvVar) {
		t.Fatalf("the first run says %q", first)
	}
	if later := e.mustEnsure(); strings.Contains(later, "created") || strings.Contains(later, config.FileEnvVar) {
		t.Errorf("a run that created nothing says %q", later)
	}
}

// The words are the ones the settings can come from, said once each.
func TestTheStartupLineNamesWhereTheSettingsCameFrom(t *testing.T) {
	written := func(source string) map[string]storage.DocumentSeed {
		return map[string]storage.DocumentSeed{storage.DocumentConfig: {Name: storage.DocumentConfig, Source: source}}
	}
	one := map[string]storage.Document{storage.DocumentConfig: {Version: 1}}

	for _, test := range []struct {
		name    string
		rows    map[string]storage.Document
		written map[string]storage.DocumentSeed
		want    string
	}{
		{"the environment", one, written(sourceEnv), "database (created from JINGCLAW_CONFIG)"},
		{"a file", one, written(sourceFile), "database (imported from config.toml)"},
		{"the defaults", one, written(sourceDefault), "database (created, all defaults)"},
		{"one document", one, nil, "database (1 document)"},
		{"three documents", map[string]storage.Document{
			storage.DocumentConfig: {Version: 4}, storage.DocumentAgents: {Version: 1}, storage.DocumentPersona: {Version: 2},
		}, nil, "database (3 documents)"},
	} {
		if got := describeSettings(test.rows, test.written); got != test.want {
			t.Errorf("%s: %q, want %q", test.name, got, test.want)
		}
	}
}

// The same guard for a configuration the environment carries.
//
// JINGCLAW_CONFIG is stored the same way a file is, and a line in it for where
// the database is is ignored the same way. A deployment that keeps its data on
// a volume and its configuration in the service's variables would otherwise
// start, say it had created its settings from the variable, and open an empty
// database in the default place: the sessions on the volume are still there and
// nothing is using them.
func TestAConfigVariableThatNamesAPlaceThisProcessWouldNotUseIsRefusedToo(t *testing.T) {
	for _, one := range []struct{ name, key, variable string }{
		{"the database", "data_dir", "JINGCLAW_SERVER_DATA_DIR"},
		{"the discovery file", "runtime_dir", "JINGCLAW_SERVER_RUNTIME_DIR"},
	} {
		t.Run(one.name, func(t *testing.T) {
			e := newEnsuring(t)
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			e.env = []config.Seed{
				{Document: storage.DocumentConfig, Text: "[server]\n" + one.key + " = " + quoted(elsewhere) + "\n"},
				{Document: storage.DocumentPersona, Text: "mine\n"},
			}

			_, err := e.ensure()
			if err == nil {
				t.Fatal("it saved a configuration that says the database is somewhere else")
			}
			for _, want := range []string{one.variable + "=" + elsewhere, config.FileEnvVar} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}

			// Nothing saved, so that fixing the variable and starting again is a first start.
			for _, name := range []string{storage.DocumentConfig, storage.DocumentAgents, storage.DocumentPersona} {
				if doc := e.doc(name); doc.Version != 0 {
					t.Errorf("%s was saved by a start that was refused", name)
				}
			}
		})
	}
}

func TestAConfigVariableThatNamesThePlaceThisProcessUsesIsKept(t *testing.T) {
	e := newEnsuring(t)
	e.env = []config.Seed{{
		Document: storage.DocumentConfig,
		Text:     "[server]\ndata_dir = " + quoted(e.places[0].dir) + "\nruntime_dir = " + quoted(e.places[1].dir) + "\n",
	}}

	if _, err := e.ensure(); err != nil {
		t.Fatalf("refused a variable that names where this process already is: %v", err)
	}
}

// A configuration the environment carries that no start could use is not saved.
//
// A saved row is never replaced by the variable, so a typo saved here would
// outlast the correction of the variable, and the one place to correct it is a
// dashboard that a container cannot be reached from. Refusing it leaves the
// next start a first start, which the corrected variable then is.
func TestAConfigVariableThatCouldNotRunIsRefusedAndSavesNothing(t *testing.T) {
	e := newEnsuring(t)
	e.env = []config.Seed{
		{Document: storage.DocumentConfig, Text: "[agent]\nmax_iterations = -5\n"},
		{Document: storage.DocumentPersona, Text: "mine\n"},
	}

	_, err := e.ensure()
	var invalid *config.InvalidError
	if !errors.As(err, &invalid) || !strings.Contains(invalid.Error(), "agent.max_iterations") {
		t.Fatalf("the refusal does not say which setting: %v", err)
	}
	for _, name := range []string{storage.DocumentConfig, storage.DocumentAgents, storage.DocumentPersona} {
		if doc := e.doc(name); doc.Version != 0 {
			t.Errorf("%s was saved by a start that was refused", name)
		}
	}

	e.env[0].Text = "[agent]\nmax_iterations = 5\n"
	e.mustEnsure()
	if doc := e.doc(storage.DocumentConfig); doc.Text != e.env[0].Text || doc.Source != sourceEnv {
		t.Errorf("the corrected variable made %q from %q", doc.Text, doc.Source)
	}
}

// And a file is not held to it. An older version's file is imported as it is,
// so that the person who has it can correct it where the daemon can be reached;
// they have nothing else to correct it in.
func TestALegacyFileThatCouldNotRunIsStillImported(t *testing.T) {
	e := newEnsuring(t)
	e.file(home.ConfigName, "[agent]\nmax_iterations = -5\n")

	e.mustEnsure()

	if doc := e.doc(storage.DocumentConfig); doc.Text != "[agent]\nmax_iterations = -5\n" || doc.Source != sourceFile {
		t.Errorf("the config is %q from %q, want the file as it was", doc.Text, doc.Source)
	}
}

// A question is answered from the documents, and moves nothing.
//
// What a trial run opens is not necessarily the database the deployment goes on
// to use: with a database of its own for the run, the files it imports are
// copies, and setting them aside would leave the real database to be created
// from nothing. So they stay, and the start that is not a question finishes it.
func TestAQuestionImportsWhatItNeedsButSetsNothingAside(t *testing.T) {
	e := newEnsuring(t)
	e.asking = true
	e.file(home.ConfigName, "[agent]\nmax_iterations = 5\n")
	e.file(config.PersonaFile, "mine\n")

	e.mustEnsure()

	if doc := e.doc(storage.DocumentPersona); doc.Text != "mine\n" || doc.Source != sourceFile {
		t.Errorf("the persona is %q from %q: the question was not answered from the file", doc.Text, doc.Source)
	}
	for _, name := range []string{home.ConfigName, config.PersonaFile} {
		if !e.exists(name) || e.exists(name+".imported") {
			t.Errorf("%s was set aside by a question", name)
		}
	}

	e.asking = false
	e.mustEnsure()

	for _, name := range []string{home.ConfigName, config.PersonaFile} {
		if e.exists(name) || !e.exists(name+".imported") {
			t.Errorf("%s was not set aside by the start after the question", name)
		}
	}
}

// What is said about a problem reaches a terminal in four places: the banner,
// the log, the line the supervisor prints and what the dashboard command prints.
// The text of a parse error carries whatever was in the file, a key included,
// and a file that was imported may have been written by anybody.
func TestAReasonCarriesNoCharacterATerminalWouldActOn(t *testing.T) {
	hostile := errors.New("duplicate key \"a\x1b[31m\x1b]0;pwned\x07\r\u009b\" at line 3\nand a second line\twith a tab")

	for name, problem := range map[string]control.AgentProblem{
		"a setting that does not read": settingsProblems(hostile)[0],
		"an agent that does not start": startupProblem(hostile),
	} {
		for _, r := range problem.Why {
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				t.Errorf("%s: the reason carries %q: %q", name, r, problem.Why)
			}
		}
		if !strings.Contains(problem.Why, "duplicate key") || !strings.Contains(problem.Why, "and a second line\twith a tab") {
			t.Errorf("%s: the reason lost its words: %q", name, problem.Why)
		}
	}
}

// The database holds the settings, and the settings can carry the headers a tool
// server is given. Inside the deployment directory it is hidden from a confined
// command with the rest of it; given a place of its own, it has to be named.
func TestAConfinedCommandIsNotShownADatabaseThatLivesOutsideTheDeployment(t *testing.T) {
	root := filepath.Join(t.TempDir(), home.DirName)

	inside := hiddenFrom(root, filepath.Join(root, home.DataName, "jingclaw.db"), nil)
	if strings.Join(inside, "|") != root {
		t.Errorf("a database inside the deployment adds to what is hidden: %v", inside)
	}

	elsewhere := filepath.Join(t.TempDir(), "srv", "jingclaw.db")
	outside := hiddenFrom(root, elsewhere, []string{"/extra"})
	want := []string{root, elsewhere, elsewhere + "-wal", elsewhere + "-shm", "/extra"}
	if strings.Join(outside, "|") != strings.Join(want, "|") {
		t.Errorf("hidden is %v, want %v", outside, want)
	}

	// A directory that merely starts with the same letters is not inside it.
	neighbour := root + "-data"
	if got := hiddenFrom(root, filepath.Join(neighbour, "jingclaw.db"), nil); len(got) != 4 {
		t.Errorf("a database in %s was taken for one inside %s: %v", neighbour, root, got)
	}
}

// quoted writes a path as a TOML string.
func quoted(path string) string {
	return `"` + strings.ReplaceAll(path, `\`, `\\`) + `"`
}
