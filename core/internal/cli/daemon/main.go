// Package daemon is the agent daemon: the process that owns the workspace,
// the event log and the tools.
//
// It owns every piece of durable state: sessions, runs, the event log and, in
// later milestones, tools and permissions. Control clients (GUI, CLI, web) are
// projections of it, so closing one never stops work that is already running.
package daemon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
	"github.com/KoukeNeko/JingClaw/core/internal/prompt"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/anthropic"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/gemini"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/ollama"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/sandbox"
	"github.com/KoukeNeko/JingClaw/core/internal/secret"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/web"
)

const (
	// shutdownGrace is how long run goroutines get to finish what they are
	// doing. This is the one that protects the log.
	shutdownGrace = 10 * time.Second

	// httpDrainGrace is how long ordinary requests get to finish.
	//
	// Short on purpose. A subscription is an in-flight request that will not
	// end on its own, so waiting for one is waiting for the whole window —
	// which is what used to leave the runtime none of it at all.
	httpDrainGrace = 2 * time.Second
)

// Main runs the daemon. Args are the arguments after the subcommand name.
func Main(args []string) error {
	if err := run(args); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func run(args []string) error { return runContext(context.Background(), args) }

// runContext is run, ending when parent does as well as on a signal. For a
// test, which has no other way to stop a daemon that is serving.
func runContext(parent context.Context, args []string) error {
	// Before anything is read, so that a credential file modified after this is
	// one this process cannot have read.
	begun := time.Now()

	flags := flag.NewFlagSet("jingclaw daemon", flag.ContinueOnError)
	var (
		printPrompt = flags.Bool("print-prompt", false, "print the assembled system prompt with its sources and exit")
		printConfig = flags.Bool("print-config", false, "print the example settings, every one at its default, and exit")
		listModels  = flags.Bool("list-models", false, "print the provider's available models and exit")
		initHere    = flags.Bool("init", false, "create the deployment directory (~/"+home.DirName+", or where "+home.EnvVar+" names) and exit")
		printPaths  = flags.Bool("print-paths", false, "print where this deployment keeps things and exit")

		// Every flag below has a setting of the same meaning in the saved
		// settings, and exists only so one run can differ from them. They carry
		// no defaults of their own: the settings already hold those, and a second
		// copy here would be one more place for them to disagree.
		providerName = flags.String("provider", "", "model provider: gemini, anthropic, ollama, openai_compat, or fake")
		model        = flags.String("model", "", "model to use")
		addr         = flags.String("addr", "", "loopback address to listen on; port 0 picks a free one")
		dataDir      = flags.String("data-dir", "", "directory for the database")
		maxIters     = flags.Int("max-iterations", 0, "cap on tool iterations per run")
	)
	// Retained so existing invocations keep working.
	devFake := flags.Bool("dev-fake", false, "alias for --provider=fake")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *printConfig {
		fmt.Print(config.Example)
		return nil
	}

	if *initHere {
		return initialise()
	}

	// The settings as this run has them: the defaults, then the text, then the
	// environment, and over all of it the flags. Only a flag the operator
	// actually typed counts, so an unset flag does not overwrite a configured
	// value with a default.
	withFlags := func(text []byte) (config.Config, error) {
		cfg, err := config.Candidate(text)
		if err != nil {
			return config.Config{}, err
		}
		applyFlagOverrides(flags, &cfg, providerName, model, dataDir, addr, maxIters)
		if *devFake {
			cfg.Provider.Backend = "fake"
		}
		return cfg, nil
	}

	// The settings are in the database, and the database is somewhere the
	// settings cannot say. So the first thing read is only what the process is
	// given: no text, which is the defaults and the environment, and the flags.
	// This is where the database and the discovery file are, and nothing is
	// created to find that out. A variable that cannot be read is an error here,
	// before there is anywhere to keep anything.
	bootstrap, err := withFlags(nil)
	if err != nil {
		return err
	}

	if *printPaths {
		return reportPaths(os.Stdout, bootstrap)
	}

	// Decoded before anything is created, so that a refused start leaves nothing
	// behind it. Nothing is saved yet.
	fromEnvironment, err := config.SeedsFromEnvironment()
	if err != nil {
		return err
	}

	discoveryPath, err := discovery.PathIn(bootstrap.Server.RuntimeDir)
	if err != nil {
		return err
	}

	// Signal-aware root context. Everything the daemon owns descends from it,
	// so one Ctrl+C unwinds the whole tree.
	rootCtx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Until the settings are read there is no level to log at.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dbPath, err := databasePath(bootstrap.Server.DataDir)
	if err != nil {
		return err
	}

	if where, temporary := isTemporary(dbPath); temporary {
		// Said once, loudly, because the failure is silent and total: the
		// database holds every conversation, every approval and everything
		// the agent has been told to remember, and a directory the system
		// clears takes all of it without an error anywhere.
		logger.Warn("the database is somewhere the system clears",
			"database", dbPath,
			"under", where,
			"holds", "sessions, approvals, memories and the settings",
			"fix", "give JINGCLAW_SERVER_DATA_DIR, or --data-dir, somewhere that survives a reboot",
		)
	}

	// A database that cannot be opened ends the start. Every other reason not
	// to run an agent leaves a daemon that can say what it is; without the
	// database there is nothing to correct the settings in.
	store, err := sqlite.Open(rootCtx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	var deployment string
	if dir, found := home.Resolve(); found {
		deployment = dir.Root
	}

	// The two ways of asking a question of the settings rather than running
	// them. Nothing is served for either, so there is nothing to repair.
	asking := *printPrompt || *listModels

	settings, err := ensureDocuments(rootCtx, store, ensureOptions{
		Deployment: deployment,
		Env:        fromEnvironment,
		Places:     placesOf(bootstrap, dbPath, discoveryPath),
		Asking:     asking,
	}, logger)
	if err != nil {
		// A variable that gives settings no start could use is reported the way
		// a saved text is: what to write instead, and not a line of text.
		if config.Report(os.Stderr, err, config.FileEnvVar) {
			return errors.New(config.FileEnvVar + " cannot be used, so nothing was saved")
		}
		return err
	}

	docs, err := readSnapshot(rootCtx, store)
	if err != nil {
		return err
	}

	// Checked once, here. Making something configurable is exactly when it
	// needs validating: a value nobody could previously write is now one
	// somebody can. Text that does not read, or does not pass, leaves the
	// process with the settings it was given and a reason, where it used to
	// have nothing and exit.
	cfg, settingsErr := withFlags([]byte(docs.Config.Text))
	if settingsErr != nil {
		cfg = bootstrap
	} else {
		settingsErr = cfg.Validate()
	}

	logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	slog.SetDefault(logger)

	var (
		problems []control.AgentProblem
		built    *agent
	)
	if settingsErr != nil {
		// Reported here as well as carried: a mistyped setting is something a
		// person is about to go and edit, and a JSON log line is not what they
		// need to do that.
		said := config.Report(os.Stderr, settingsErr, savedSettings)
		if asking {
			if said {
				return errors.New("the saved settings cannot be used")
			}
			return settingsErr
		}
		problems = settingsProblems(settingsErr)
	} else {
		built, err = startAgent(rootCtx, agentSpec{
			cfg: cfg, store: store, dbPath: dbPath, logger: logger,
			agents: docs.Agents.Text, persona: docs.Persona.Text,
			listModels: *listModels, printPrompt: *printPrompt,
		})
		switch {
		case err != nil && (asking || rootCtx.Err() != nil):
			return err
		case err != nil:
			// The settings read, and the machine or the provider would not do
			// what they ask. Still a daemon that can be reached, so that what
			// is wrong can be seen where the settings are.
			problems = []control.AgentProblem{startupProblem(err)}
		case built == nil:
			return nil
		default:
			defer built.close()
		}
	}

	return serve(rootCtx, serving{
		cfg: cfg, logger: logger, store: store,
		dbPath: dbPath, discoveryPath: discoveryPath, settings: settings,
		overrides: settingsHeldFromTheSavedText(flags, cfg),
		started:   docs.hashes(),
		begun:     begun,
		candidate: withFlags,
		problems:  problems,
		agent:     built,
	})
}

// reportPaths says where this deployment keeps things.
//
// Everything that resolves from a directory, a setting or a platform default
// resolves in one place; a script or an operator asking where the discovery
// file is should not have to reimplement that and then drift from it.
func reportPaths(w io.Writer, cfg config.Config) error {
	database, err := resolveDatabase(cfg.Server.DataDir)
	if err != nil {
		return err
	}

	discoveryPath, err := discovery.PathIn(cfg.Server.RuntimeDir)
	if err != nil {
		return err
	}

	root := "(none)"
	if dir, found := home.Resolve(); found {
		root = dir.Root
	}

	for _, line := range [][2]string{
		{"home", root},
		// Not a file. Said here because this is where somebody looking for the
		// settings looks, and a line that is not there reads as not knowing.
		{"settings", "in the database, edited in the dashboard"},
		{"workspace", workspaceRoot()},
		{"database", database},
		{"artifacts", artifactDir(database)},
		{"discovery", discoveryPath},
	} {
		fmt.Fprintf(w, "%-10s %s\n", line[0], line[1])
	}
	return nil
}

// initialise creates this deployment's directory and stops.
//
// For setting one up somewhere other than the default, which means the
// environment named it: creating one is not a question about the working
// directory any more than finding one is.
func initialise() error {
	// Where a deployment lives is not a question about this directory, so
	// neither is creating one. The environment can name somewhere else; the
	// working directory cannot.
	at, found := home.Resolve()
	if !found {
		return fmt.Errorf("%s is set to none, so there is nowhere to create", home.EnvVar)
	}

	dir, err := home.Create(at.Root)
	if err != nil {
		return err
	}

	// Directories and nothing else. The settings are not files: the first start
	// saves them in the database, so there is nothing to put here for somebody
	// to edit, and a file that is not read is one that misleads.
	fmt.Printf("Created %s\n\n", dir.Root)
	fmt.Printf("  %-12s what the agent may read and change\n", home.WorkspaceName+"/")
	fmt.Printf("  %-12s the database, which holds the settings, and stored output\n", home.DataName+"/")
	fmt.Printf("  %-12s how clients find this daemon\n", home.RunName+"/")
	fmt.Printf("\nThe settings are made on the first start, every one at its default, and are\n")
	fmt.Printf("edited in the dashboard: run jingclaw, then run: jingclaw dashboard\n")
	fmt.Printf("Credentials go in %s, mode 600.\n", dir.Root)
	return nil
}

// databasePath resolves where state is kept and makes the directory for it. An
// explicit --data-dir wins so a test or a second instance can be fully
// isolated.
func databasePath(dataDir string) (string, error) {
	path, err := resolveDatabase(dataDir)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	return path, nil
}

// resolveDatabase says where the database would be, without making anything.
//
// Separate from databasePath because asking where a deployment keeps its
// things should not create them: a --print-paths that leaves directories
// behind is one nobody can run to find out.
func resolveDatabase(dataDir string) (string, error) {
	if dataDir == "" {
		dir, found := home.Resolve()
		if !found {
			return "", fmt.Errorf("%s is set to none, so there is no database", home.EnvVar)
		}
		dataDir = dir.Data()
	}
	return filepath.Join(dataDir, "jingclaw.db"), nil
}

// buildProvider constructs the configured provider. Real providers are wrapped
// in retry here rather than inside each adapter, so backoff policy is one
// decision instead of one per vendor.
// standingDirections puts what the agent was told to remember in front of it.
//
// Per run rather than per process, because a turn from a chat account and one
// typed at this machine are not owed the same recollections. Nil when memory
// is off, so the prompt is exactly what it was before the feature existed.
func standingDirections(
	cfg config.Config,
	options memorytool.Options,
	logger *slog.Logger,
) func(context.Context, domain.Run) string {
	return func(ctx context.Context, run domain.Run) string {
		// A worker is not having the conversation, and gets none of what the
		// conversation is told. Not filtering but replacing: the operator's
		// standing memories are how they want their work done, and a search
		// that has been told how somebody likes their commits written is a
		// search carrying instructions it has no business acting on.
		if run.Kind == domain.RunWorker {
			return prompt.ForDelegatedSearch()
		}

		parts := make([]string, 0, 2)

		// Where this turn is going, when that changes what an answer has to
		// look like. Said per run rather than in the standing prompt because
		// it is only true of some of them: a turn typed at this machine goes
		// to a terminal, which has none of these limits.
		if run.Origin.Kind == domain.OriginGateway {
			parts = append(parts, prompt.ForChatChannel())
		}

		if cfg.Memory.Enabled {
			directions, err := memorytool.Instructions(ctx, options.Store, options, run,
				cfg.Memory.MaxInstructionBytes)
			if err != nil {
				// A run that cannot read memory is still a run. Failing it
				// would make an unavailable database into a broken agent.
				logger.Warn("could not read standing directions",
					"run_id", string(run.ID), "error", err)
			} else if directions != "" {
				parts = append(parts, directions)
			}
		}

		return strings.Join(parts, "\n\n")
	}
}

// artifactDir settles where stored output lives.
//
// Beside the database by default, because that is where this daemon's other
// durable state already is and splitting the two across directories makes a
// backup something an operator can half do.
// isTemporary reports whether a path is somewhere the system empties.
//
// Not a guess about intent: a daemon pointed at a scratch directory during an
// experiment is a reasonable thing, and this does not refuse it. It says so,
// because the alternative is finding out when the conversations are gone.
func isTemporary(path string) (string, bool) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	// Symlinks matter here: /tmp is a link to /private/tmp on macOS, and a
	// path spelled either way is the same doomed directory.
	if evaluated, err := filepath.EvalSymlinks(filepath.Dir(resolved)); err == nil {
		resolved = filepath.Join(evaluated, filepath.Base(resolved))
	}

	// What this system itself calls temporary, first. On Windows that is the
	// only one of these that means anything — the literals below are Unix
	// paths, and a database under %TEMP% would have gone unmentioned.
	roots := []string{os.TempDir()}
	if evaluated, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		roots = append(roots, evaluated)
	}
	roots = append(roots, "/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/dev/shm")
	if fromEnv := os.Getenv("TMPDIR"); fromEnv != "" {
		if cleaned, err := filepath.EvalSymlinks(fromEnv); err == nil {
			roots = append(roots, cleaned)
		}
		roots = append(roots, fromEnv)
	}

	for _, root := range roots {
		cleaned := filepath.Clean(root)
		if cleaned == "/" || cleaned == "." {
			continue
		}
		if resolved == cleaned || strings.HasPrefix(resolved, cleaned+string(filepath.Separator)) {
			return cleaned, true
		}
	}
	return "", false
}

// artifactDir is where stored output goes: beside the database.
//
// Not a setting. The database and the output it refers to are one body of
// state, and letting them be put in different places makes backing up or
// moving a deployment a thing you can do half of.
func artifactDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "artifacts")
}

// workspaceRoot is what the agent may read and write.
//
// The deployment's own directory, always. Never the working directory —
// "whatever you happened to be standing in" is not a setting, and as a
// default it hands a fresh install the contents of the first project
// somebody starts it from.
func workspaceRoot() string {
	if dir, found := home.Resolve(); found {
		return dir.Workspace()
	}
	return ""
}

// ensureWorkspace makes the directory the agent works in, and returns it.
//
// Somebody has to. It stopped being a setting an operator points at something
// that already exists, and became a fact about the deployment — so a fresh
// install has a workspace for the same reason it has settings: because
// starting created them.
//
// Opening still refuses a path that is not there. That check is about a
// deployment whose directory was moved or removed while it was not running,
// which is worth failing over rather than quietly making a new empty one.
func ensureWorkspace() (string, error) {
	root := workspaceRoot()
	if root == "" {
		return "", fmt.Errorf("%s is set to none, so there is no workspace", home.EnvVar)
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create the workspace at %s: %w", root, err)
	}
	return root, nil
}

// mcpServers turns the configured servers into what the mcp package needs.
//
// The level is resolved here rather than in the config package so that there
// is one definition of what the names mean, in the package that owns them.
// An unknown name was already refused by validation; execute is the floor
// anyway, so falling back to it cannot make anything more permissive.
// webFetcher builds the page fetcher an operator configured, or nil when they
// did not ask for one.
func webFetcher(cfg config.Config) (web.Fetcher, error) {
	if !cfg.Web.Enabled || cfg.Web.Backend == "none" {
		return nil, nil
	}

	switch cfg.Web.Backend {
	case "browser":
		python, err := web.PythonPath(cfg.Web.Python)
		if err == nil {
			err = web.CanDriveABrowser(python)
		}
		if err != nil {
			// The cause, and then the remedy. Whoever reads this is often
			// somewhere with nothing to install into — an image carries what
			// it carries — so the line has to name the setting that runs
			// without a browser rather than only the thing that is missing.
			return nil, fmt.Errorf(
				"web.backend is \"browser\": %w\n"+
					"Reading pages drives a real browser, which needs python3 with the "+
					"cloakbrowser package. Set web.enabled = false to run without it.", err)
		}
		return &web.BrowserFetcher{
			Python:   python,
			Timeout:  cfg.Web.Timeout,
			MaxLinks: cfg.Web.MaxLinks,
		}, nil
	default:
		// Validation has already refused anything else; reaching here means
		// the two lists disagree, which is worth saying out loud.
		return nil, fmt.Errorf("web.backend %q has no implementation", cfg.Web.Backend)
	}
}

// webSearcher builds the search backend, if one is configured.
//
// The key is resolved now rather than at the first call, for the same reason
// the browser interpreter is: an agent that advertises a tool it cannot run
// wastes a turn discovering that, and the operator finds out from a log line
// rather than from a model apologising.
func webSearcher(cfg config.Config) (web.Searcher, error) {
	if !cfg.Web.Enabled || cfg.Web.Search.Backend == "none" || cfg.Web.Search.Backend == "" {
		return nil, nil
	}

	switch cfg.Web.Search.Backend {
	case "brave":
		key, err := secret.Find(cfg.Web.Search.KeyEnv, cfg.Web.Search.KeyFile, "web.search.key_file")
		if err != nil {
			return nil, fmt.Errorf("web.search.backend is \"brave\": %w", err)
		}
		return &web.Brave{Key: key.Reveal(), Endpoint: cfg.Web.Search.Endpoint}, nil

	default:
		// Validation has already refused anything else; reaching here means
		// the two lists disagree, which is worth saying out loud.
		return nil, fmt.Errorf("web.search.backend %q has no implementation", cfg.Web.Search.Backend)
	}
}

func mcpServers(cfg config.Config, sessions *mcpauth.Store) []mcp.ServerConfig {
	servers := make([]mcp.ServerConfig, 0, len(cfg.MCP.Servers))

	for _, configured := range cfg.MCP.Servers {
		level := tool.LevelExecute
		if configured.Level != "" {
			if resolved, ok := tool.LevelByName(configured.Level); ok {
				level = resolved
			}
		}

		servers = append(servers, mcp.ServerConfig{
			Name:    configured.Name,
			Command: configured.Command,
			Args:    configured.Args,
			URL:     configured.URL,
			Headers: configured.Headers,
			Env:     configured.Env,
			PassEnv: configured.PassEnv,
			Level:   level,

			OAuth:    configured.OAuth,
			Sessions: sessions,
			Defer:    configured.Defer,
		})
	}

	return servers
}

// describeTools says what the model can reach, and says when a server that was
// asked for is not there.
//
// A tool that is quietly absent looks exactly like one the model chose not to
// use, which is the hardest kind of missing thing to notice.
func describeTools(registry *tool.Registry, servers *mcp.Manager, cfg config.Config) string {
	described := fmt.Sprintf("%d", len(registry.Specs()))

	wanted := len(cfg.MCP.Servers)
	if wanted == 0 {
		return described
	}

	said := fmt.Sprintf("%s (%d from %d of %d mcp servers",
		described, servers.ToolCount(), servers.Connected(), wanted)

	// On the startup line rather than only in a warning above it. This is the
	// line somebody reads to know what the agent can do, and a server missing
	// because nobody has signed in belongs in the same sentence as the ones
	// that are there.
	if waiting := servers.NeedLogin(); len(waiting) > 0 {
		said += fmt.Sprintf("; %s need: jingclaw mcp login <name>", strings.Join(waiting, ", "))
	}
	return said + ")"
}

// contextWindow settles how much room a run has.
//
// The provider is asked first because it knows; the setting exists for a local
// model served by something that does not report one. Zero from both leaves
// compaction off, which is the honest outcome: summarising against a guessed
// window would either throw history away early or fail to save the session
// that needed saving.
func contextWindow(cfg config.Config, model provider.ModelInfo) (int64, provider.ContextSource) {
	if cfg.Context.Window > 0 {
		return cfg.Context.Window, provider.ContextOperator
	}
	if model.ContextWindow > 0 {
		source := model.ContextSource
		if source == provider.ContextUnknown {
			source = provider.ContextCatalog
		}
		return model.ContextWindow, source
	}
	return 0, provider.ContextUnknown
}

func buildProvider(ctx context.Context, cfg config.Config) (provider.Provider, error) {
	switch cfg.Provider.Backend {
	case "fake":
		offline := fake.New(cfg.Provider.FakeDelay)
		offline.Reasoning = cfg.Provider.FakeReasoning
		for _, turn := range cfg.Provider.FakeScript {
			offline.Script = append(offline.Script, fake.Turn{
				Text: turn.Text, Tool: turn.Tool, Args: turn.Args,
			})
		}
		return offline, nil

	case "gemini":
		keyFiles, err := secret.DefaultFiles(cfg.Provider.Gemini.APIKeyFile)
		if err != nil {
			return nil, err
		}

		apiKey, err := secret.Load(secret.LoadOptions{
			EnvVars: cfg.Provider.Gemini.APIKeyEnv,
			Files:   keyFiles,
		})
		if err != nil {
			return nil, err
		}
		if !apiKey.IsSet() {
			return nil, secret.Absent("Gemini API key", cfg.Provider.Gemini.APIKeyEnv, "provider.gemini.api_key_file", cfg.Provider.Gemini.APIKeyFile)
		}

		p, err := gemini.New(ctx, gemini.Config{APIKey: apiKey.Reveal()})
		if err != nil {
			return nil, err
		}

		return provider.WithRetry(p, retryPolicy(cfg)), nil

	case "anthropic":
		keyFiles, err := secret.DefaultFiles(cfg.Provider.Anthropic.APIKeyFile)
		if err != nil {
			return nil, err
		}

		apiKey, err := secret.Load(secret.LoadOptions{
			EnvVars: cfg.Provider.Anthropic.APIKeyEnv,
			Files:   keyFiles,
		})
		if err != nil {
			return nil, err
		}
		if !apiKey.IsSet() {
			return nil, secret.Absent("Anthropic API key", cfg.Provider.Anthropic.APIKeyEnv, "provider.anthropic.api_key_file", cfg.Provider.Anthropic.APIKeyFile)
		}

		p, err := anthropic.New(anthropic.Config{
			APIKey:  apiKey.Reveal(),
			Model:   cfg.Provider.Anthropic.Model,
			BaseURL: cfg.Provider.Anthropic.BaseURL,
		})
		if err != nil {
			return nil, err
		}

		return provider.WithRetry(p, retryPolicy(cfg)), nil

	case "ollama":
		// A credential only if one was supplied: the hosted service needs one
		// and a local daemon has nothing to check it against.
		key, err := optionalKey(cfg.Provider.Ollama.APIKeyEnv, cfg.Provider.Ollama.APIKeyFile)
		if err != nil {
			return nil, err
		}

		p, err := ollama.New(ollama.Config{
			BaseURL:   cfg.Provider.Ollama.BaseURL,
			APIKey:    key,
			KeepAlive: cfg.Provider.Ollama.KeepAlive,
			NumCtx:    cfg.Provider.Ollama.NumCtx,
			Think:     cfg.Provider.Ollama.Think,
		})
		if err != nil {
			return nil, err
		}
		return provider.WithRetry(p, retryPolicy(cfg)), nil

	case "openai_compat":
		key, err := optionalKey(
			cfg.Provider.OpenAICompat.APIKeyEnv, cfg.Provider.OpenAICompat.APIKeyFile)
		if err != nil {
			return nil, err
		}

		p, err := openaicompat.New(openaicompat.Config{
			BaseURL: cfg.Provider.OpenAICompat.BaseURL,
			APIKey:  key,
			Profile: cfg.Provider.OpenAICompat.Profile,
			Name:    cfg.Provider.OpenAICompat.Name,
		})
		if err != nil {
			return nil, err
		}
		return provider.WithRetry(p, retryPolicy(cfg)), nil

	default:
		return nil, fmt.Errorf(
			"unknown provider %q; use gemini, anthropic, ollama, openai_compat, or fake", cfg.Provider.Backend)
	}
}

// retryPolicy is shared by every provider that reaches the network, so a
// deployment cannot end up with one of them retrying on different terms from
// the rest.
func retryPolicy(cfg config.Config) provider.RetryPolicy {
	return provider.RetryPolicy{
		MaxAttempts: cfg.Provider.Retry.MaxAttempts,
		BaseDelay:   cfg.Provider.Retry.BaseDelay,
		MaxDelay:    cfg.Provider.Retry.MaxDelay,
		Jitter:      cfg.Provider.Retry.Jitter,
		Budget:      cfg.Provider.Retry.Budget,
	}
}

// optionalKey loads a credential if one was configured, and is content when
// none was.
//
// A local model server usually has no credential at all, so a missing one is
// an ordinary state here rather than the startup failure it is for a hosted
// provider.
func optionalKey(envVars []string, keyFile string) (string, error) {
	files, err := secret.DefaultFiles(keyFile)
	if err != nil {
		return "", err
	}

	key, err := secret.Load(secret.LoadOptions{EnvVars: envVars, Files: files})
	if err != nil {
		return "", err
	}
	if !key.IsSet() {
		return "", nil
	}
	return key.Reveal(), nil
}

// resolveModel picks the model to run with. An explicit choice is verified
// against what the provider actually serves, because a typo should fail at
// startup rather than on the user's first message.
// resolveModel returns the model to use and what the provider says about it.
//
// The whole ModelInfo rather than the name, because the context window comes
// from here: the runtime has to know how much room it has before it can decide
// when a session needs compacting, and asking the provider is better than
// keeping a table of window sizes that goes stale.
func resolveModel(ctx context.Context, p provider.Provider, requested string) (provider.ModelInfo, error) {
	models, err := p.Models(ctx)
	if err != nil {
		return provider.ModelInfo{}, fmt.Errorf("list models: %w", err)
	}

	if requested != "" {
		for _, m := range models {
			if m.ID == requested {
				return m, nil
			}
		}
		if len(models) == 0 {
			// The provider could not enumerate; trust the operator rather
			// than refuse to start. Nothing is known about the model, so the
			// window is unknown too and compaction stays off unless the
			// configuration says otherwise.
			return provider.ModelInfo{ID: requested}, nil
		}
		return provider.ModelInfo{}, fmt.Errorf(
			"provider %s does not serve model %q; run --list-models to see the options",
			p.Name(), requested)
	}

	if len(models) == 1 {
		return models[0], nil
	}
	return provider.ModelInfo{}, fmt.Errorf(
		"provider %s serves %d models; pick one with --model (see --list-models)",
		p.Name(), len(models))
}

func printModels(ctx context.Context, p provider.Provider) error {
	models, err := p.Models(ctx)
	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}

	for _, m := range models {
		fmt.Printf("%-40s  ctx=%-9d out=%-7d  %s\n",
			m.ID, m.ContextWindow, m.MaxOutputTokens, m.DisplayName)
	}
	return nil
}

// applyFlagOverrides lets a typed flag win over the saved settings.
//
// Only flags the operator actually passed are considered: taking an unset
// flag's zero value would silently replace a configured setting with a
// default, which looks exactly like the configuration being ignored.
func applyFlagOverrides(flags *flag.FlagSet, cfg *config.Config, providerName, model, dataDir, addr *string, maxIters *int) {
	passed := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	if passed["provider"] {
		cfg.Provider.Backend = *providerName
	}
	if passed["model"] {
		cfg.Provider.SetModel(*model)
	}
	if passed["data-dir"] {
		cfg.Server.DataDir = *dataDir
	}
	if passed["addr"] {
		cfg.Server.Addr = *addr
	}
	if passed["max-iterations"] {
		cfg.Agent.MaxIterations = *maxIters
	}
}

// planItemIDs names the steps of a plan.
//
// Short and countable rather than shaped like every other id here. These are
// put in front of the model and typed back by it, and a model asked to repeat
// "todo_01M167Z4RHHNVESFHHM7H6VNZY" gets it wrong often enough to matter.
//
// Per process rather than per session, so two sessions planning at once
// cannot be handed the same name for different steps.
func planItemIDs() runtime.IDGenerator {
	var counter atomic.Uint64
	return func() string { return fmt.Sprintf("todo_%d", counter.Add(1)) }
}

// installedSkills reads what the deployment has, when it is asked rather than
// once at startup.
//
// So that adding a skill is copying a directory. The catalogue in the prompt
// is from startup, since the prompt is assembled once — but a skill added
// while the daemon runs is still readable by name, and the next restart puts
// it in the catalogue.
type installedSkills struct{}

func (installedSkills) Installed() ([]skill.Skill, error) {
	dir, found := home.Resolve()
	if !found {
		return nil, nil
	}
	found2, _, err := skill.Installed(dir.Skills())
	return found2, err
}

// deploymentSkills fetches, stages and activates skills in the deployment's
// own skills directory, for skill_stage and skill_activate.
//
// The same directory installedSkills reads and the CLI installs into, resolved
// per call rather than kept, so the daemon has one answer for where a skill
// lives.
type deploymentSkills struct{}

func (deploymentSkills) installer() (*skill.Installer, error) {
	dir, found := home.Resolve()
	if !found {
		return nil, fmt.Errorf("skill: there is no home directory to install into")
	}
	return &skill.Installer{Root: dir.Skills(), Now: time.Now}, nil
}

func (d deploymentSkills) Stage(ctx context.Context, source skill.Source) (skill.Staged, error) {
	installer, err := d.installer()
	if err != nil {
		return skill.Staged{}, err
	}
	return installer.Stage(ctx, source)
}

func (d deploymentSkills) Activate(name string) (skill.Locked, error) {
	installer, err := d.installer()
	if err != nil {
		return skill.Locked{}, err
	}
	return installer.Activate(name)
}

func (deploymentSkills) Staged(name string) (skill.Staged, skill.Skill, error) {
	dir, found := home.Resolve()
	if !found {
		return skill.Staged{}, skill.Skill{}, fmt.Errorf("skill: there is no home directory")
	}
	return skill.StagedSkill(dir.Skills(), name)
}

// confinement is the sandbox this deployment asked for, or nil for none.
//
// Refusing at startup rather than at the first command. An operator who
// turned this on and whose machine cannot provide it should be told while
// they are still looking, not hours later inside a tool result the model
// paraphrases.
func confinement(cfg config.Config, dbPath string, logger *slog.Logger) (*builtin.Confinement, error) {
	if !cfg.Sandbox.Enabled {
		return nil, nil
	}
	if !sandbox.Available() {
		// Said at startup rather than at the first command, and said as a
		// refusal: the alternative is a deployment that believes it is
		// confining and is not.
		return nil, errors.New(
			"[sandbox] enabled is on and this machine cannot confine a command. " +
				"macOS uses sandbox-exec, which every Mac has. Linux has a landlock " +
				"backend that needs a kernel with it enabled, and even then " +
				"cannot hide the deployment directory, so it is refused too. " +
				"Nowhere else is implemented. Turn it off, or run it on macOS")
	}

	dir, found := home.Resolve()
	if !found {
		return nil, fmt.Errorf("[sandbox] enabled needs a deployment directory to keep its caches in")
	}

	dirs, err := sandbox.Under(filepath.Join(dir.Root, "sandbox"))
	if err != nil {
		return nil, err
	}

	hidden := hiddenFrom(dir.Root, dbPath, cfg.Sandbox.Hidden)

	policy := sandbox.Policy{
		Writable:   dirs.Writable(),
		Unreadable: hidden,
		Network:    cfg.Sandbox.Network,
	}

	// Asked here with the real policy, not left to the first command: this
	// one hides a directory, which landlock cannot do, so on Linux every
	// command would otherwise be refused one at a time.
	if err := sandbox.Check(policy); err != nil {
		return nil, fmt.Errorf(
			"[sandbox] enabled is on and this machine cannot keep the policy it "+
				"needs: %w. Turn it off, or run it somewhere that can", err)
	}

	logger.Info("commands are confined",
		"writable", dirs.Writable(), "network", cfg.Sandbox.Network, "hidden", hidden)

	return &builtin.Confinement{Policy: policy, Environment: dirs.Environment()}, nil
}

// hiddenFrom is what a confined command is not shown.
//
// The deployment's own directory, always. Its credentials are in there, and a
// confined command that could read them would be confined against writing and
// not against the thing worth protecting.
//
// The database too when it is somewhere else. It holds the settings, which can
// carry the headers and variables a tool server is given, and when it was in the
// deployment directory it was hidden with it. The files and not the directory
// they are in: that one is somebody's, and may hold more than this.
func hiddenFrom(root, dbPath string, more []string) []string {
	hidden := []string{root}

	if !isUnder(root, dbPath) {
		hidden = append(hidden, dbPath, dbPath+"-wal", dbPath+"-shm")
	}
	return append(hidden, expandHomes(more)...)
}

// isUnder says whether path is root or something in it.
func isUnder(root, path string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}

	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// expandHomes turns a leading ~ into the real home directory.
//
// Written by hand in the settings, "~/.ssh" is what somebody means
// and what no part of Go expands on their behalf.
func expandHomes(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if after, found := strings.CutPrefix(path, "~/"); found {
			if where, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(where, after)
			}
		}
		out = append(out, path)
	}
	return out
}

// describeConfinement says what commands will actually be held to.
func describeConfinement(cfg config.Config) string {
	if !cfg.Sandbox.Enabled {
		return "off"
	}
	return sandbox.Describe()
}
