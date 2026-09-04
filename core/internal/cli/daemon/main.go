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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/id"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/process"
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
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
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

func run(args []string) error {
	flags := flag.NewFlagSet("jingclaw daemon", flag.ContinueOnError)
	var (
		configPath  = flags.String("config", "", "configuration file; defaults to the one in the config directory")
		printPrompt = flags.Bool("print-prompt", false, "print the assembled system prompt with its sources and exit")
		printConfig = flags.Bool("print-config", false, "print an example configuration file and exit")
		listModels  = flags.Bool("list-models", false, "print the provider's available models and exit")
		initHere    = flags.Bool("init", false, "create a "+home.DirName+" directory here and exit")
		printPaths  = flags.Bool("print-paths", false, "print where this deployment keeps things and exit")

		// Every flag below has a setting of the same meaning in the
		// configuration file, and exists only so one run can differ from it.
		// They carry no defaults of their own: the file already holds those,
		// and a second copy here would be one more place for them to disagree.
		providerName = flags.String("provider", "", "model provider: gemini, ollama, openai_compat, or fake")
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

	// Nothing to configure means nothing to read, and an operator asking "where
	// do I put this?" is a question the program can answer by putting it there.
	// Only the default location, and only when it is empty: a file the operator
	// named themselves and that is missing is a mistake, not an invitation.
	//
	// Not for --print-paths, which is how somebody asks a machine what a
	// deployment there would look like. A command whose whole purpose is to
	// report can be run on a machine to find out where things are; creating
	// the first of them while answering is not reporting.
	var createdConfig, seededConfig bool
	if *configPath == "" && !*printPaths {
		// The files a deployment carries, from variables, where a variable is
		// the only way in. Before the example is written, so that a platform
		// that supplied a configuration gets that one rather than a default
		// it can then never replace.
		seeded, err := config.SeedFromEnvironment()
		if err != nil {
			return err
		}
		seededConfig = slices.Contains(seeded, home.ConfigName)

		if _, createdConfig, err = config.EnsureFile(); err != nil {
			return err
		}

		// And the two files the settings point at. Created rather than
		// documented, for the same reason the settings are: a file that
		// exists is a file somebody edits, and one they have to know to
		// create is one that stays absent. --init did this and nothing else
		// did, so every deployment that was not started by hand ran without
		// them. After what the environment supplied, which is never replaced.
		if dir, found := home.Resolve(); found {
			if err := writeInstructionFiles(dir.Root); err != nil {
				return err
			}
		}
	}

	cfg, configFile, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Flags win over the file, which wins over the defaults it was seeded
	// with. Only a flag the operator actually typed counts, so an unset flag
	// does not overwrite a configured value with a default.
	applyFlagOverrides(flags, &cfg, providerName, model, dataDir, addr, maxIters)
	if *devFake {
		cfg.Provider.Backend = "fake"
	}

	// Settings are checked once, here. Making something configurable is
	// exactly when it needs validating: a value nobody could previously write
	// is now one somebody can.
	if *printPaths {
		return reportPaths(cfg, configFile)
	}

	if err := cfg.Validate(); err != nil {
		// Reported here rather than left to main: a mistyped setting is
		// something a person is about to go and edit, and a JSON log line is
		// not what they need to do that.
		if config.Report(os.Stderr, err, configFile) {
			os.Exit(1)
		}
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	slog.SetDefault(logger)

	// Signal-aware root context. Everything the daemon owns descends from it,
	// so one Ctrl+C unwinds the whole tree.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath, err := databasePath(cfg.Server.DataDir)
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
			"holds", "sessions, approvals and memories",
			"fix", "set server.data_dir to somewhere that survives a reboot",
		)
	}

	store, err := sqlite.Open(rootCtx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	modelProvider, err := buildProvider(rootCtx, cfg)
	if err != nil {
		return err
	}

	if *listModels {
		return printModels(rootCtx, modelProvider)
	}

	selected, err := resolveModel(rootCtx, modelProvider, cfg.Provider.Model())
	if err != nil {
		return err
	}

	workspaceDir, err := ensureWorkspace()
	if err != nil {
		return err
	}

	ws, err := workspace.Open(workspaceDir)
	if err != nil {
		return err
	}

	artifacts, err := artifact.Open(artifactDir(dbPath), cfg.Artifacts.MaxBytes)
	if err != nil {
		return err
	}

	// One observer shared by the tools, so a write can tell whether the file it
	// is about to replace is one the agent actually read, and one lock set so
	// two writes to the same file cannot interleave.
	observed := builtin.NewObserver()
	locks := builtin.NewFileLocks()

	limits := builtin.Limits{
		ReadLimit:         cfg.Tools.ReadLimit,
		MaxReadableFile:   cfg.Tools.MaxReadableFile,
		MaxOverwriteBytes: cfg.Tools.MaxOverwriteBytes,
		MaxSearchableFile: cfg.Tools.MaxSearchableFile,
		GlobResults:       cfg.Tools.GlobResults,
		GrepResults:       cfg.Tools.GrepResults,
		CommandTimeout:    cfg.Tools.CommandTimeout,
		MaxCommandTimeout: cfg.Tools.MaxCommandTimeout,
		MaxCommandOutput:  cfg.Tools.MaxCommandOutput,
	}

	writeFile := builtin.NewWriteFile(ws, observed, locks)
	writeFile.Limits = limits
	editFile := builtin.NewEditFile(ws, observed, locks)

	tools := tool.NewRegistry()
	confine, err := confinement(cfg, logger)
	if err != nil {
		return err
	}

	tools.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed, Limits: limits},
		&builtin.GlobFiles{Workspace: ws, Limits: limits},
		&builtin.Grep{Workspace: ws, Limits: limits},
		writeFile,
		editFile,
		builtin.NewApplyPatch(ws, observed, locks),
		&builtin.ExecCommand{Workspace: ws, Limits: limits, Artifacts: artifacts, Confine: confine},
		&builtin.ReadArtifact{Artifacts: artifacts, Limits: limits},
		&builtin.GitStatus{Workspace: ws},
		&builtin.GitDiff{Workspace: ws, Artifacts: artifacts},
	)

	// Long-running programs belong to the daemon rather than to a run, which
	// is the point of them: a run that starts a dev server and then ends must
	// not take the server with it. Nothing else would end them, so stopping
	// the daemon does.
	processes := process.NewManager()
	processes.BufferBytes = cfg.Process.BufferBytes
	defer processes.CloseAll()

	tools.MustRegister(
		&builtin.StartProcess{Workspace: ws, Processes: processes},
		&builtin.ProcessIO{Processes: processes},
		&builtin.StopProcess{Processes: processes},
		&builtin.ListProcesses{Processes: processes},
	)

	// Where sessions somebody signed in with are kept. Opened even when no
	// server uses OAuth, because it is a directory and the alternative is a
	// nil that every path below has to remember.
	sessions, err := mcpauth.Open(mcpauth.DefaultDir())
	if err != nil {
		return err
	}

	// Tool servers are started before the prompt is assembled, because what
	// they offer is part of what the agent can do and the prompt says so.
	//
	// They are child processes of this daemon, so they are shut down with it;
	// the defer is registered immediately, before anything else can fail and
	// leave them running.
	servers := mcp.Start(rootCtx, mcpServers(cfg, sessions), mcp.Limits{
		StartTimeout: cfg.MCP.StartTimeout,
		CallTimeout:  cfg.MCP.CallTimeout,
		MaxOutput:    cfg.MCP.MaxOutput,
	}, artifacts, logger)
	defer func() {
		if err := servers.Close(); err != nil {
			logger.Warn("an mcp server did not shut down cleanly", "error", err)
		}
	}()

	if err := servers.Register(tools); err != nil {
		return err
	}

	// Memory is registered before the prompt is assembled, because what the
	// agent has been told to remember is part of what it can do.
	memoryOptions := memorytool.Options{
		Store:        store,
		WorkspaceRef: ws.Root(),
		NewID:        func() string { return id.WithPrefix("mem") },
		Clock:        time.Now,
		Log:          logger,
	}
	if cfg.Memory.Enabled {
		// A lookup that matched nothing may be tried once more with other
		// words, and the model running the session is the one asked for
		// them: it is already loaded, and it knows what the query was about
		// in a way a word list never could.
		if cfg.Memory.ExpandQueries {
			memoryOptions.Expander = &modelExpander{modelCompleter{
				provider: modelProvider,
				model:    selected.ID,
			}}
		}

		tools.MustRegister(
			&memorytool.Remember{Options: memoryOptions},
			&memorytool.Recall{Options: memoryOptions},
		)
	}

	// Reading the web is registered last among the built-ins, and only when an
	// operator asked for it. A failure to resolve the interpreter is reported
	// now rather than the first time the model asks for a page: an agent that
	// advertises a tool it cannot run wastes a turn discovering that.
	if fetcher, err := webFetcher(cfg); err != nil {
		return err
	} else if fetcher != nil {
		tools.MustRegister(&builtin.WebRead{
			Fetcher:       fetcher,
			Artifacts:     artifacts,
			MaxCharacters: cfg.Web.MaxCharacters,
		})
		logger.Info("web reading is on", "backend", fetcher.Describe())
	}

	// Searching is registered separately, and needs reading to be on: an
	// agent that can find addresses and not read them has been given the
	// half of the capability that is no use on its own.
	if searcher, err := webSearcher(cfg); err != nil {
		return err
	} else if searcher != nil {
		tools.MustRegister(&builtin.WebSearch{
			Searcher:   searcher,
			MaxResults: cfg.Web.Search.MaxResults,
		})
		logger.Info("web search is on", "backend", searcher.Describe())
	}

	profile, ok := permission.ProfileByName(cfg.Agent.PermissionProfile)
	if !ok {
		return fmt.Errorf("unknown permission profile %q", cfg.Agent.PermissionProfile)
	}
	permissions := permission.New(profile)

	// Registered before the prompt is assembled, because the prompt lists the
	// tools and a tool the model is never told about is one it will not use.
	// What they need is the runtime, which needs the prompt, so they are given
	// a handle to it and it is filled in below.
	later := &theRuntime{}
	tools.MustRegister(
		&builtin.TodoUpdate{Planner: later},
		&builtin.AskUser{},
		&builtin.Now{},
		&builtin.SkillLoad{Skills: installedSkills{}, Activations: later},
		&builtin.SkillStage{Installer: deploymentSkills{}},
		&builtin.SkillActivate{Installer: deploymentSkills{}},
		&builtin.ToolSearch{Deferred: tools},
		&builtin.ToolLoad{Deferred: tools},
		&builtin.Investigate{Delegator: later},
	)

	layers, err := buildPrompt(cfg, ws, tools, servers, logger)
	if err != nil {
		return err
	}
	if *printPrompt {
		fmt.Print(prompt.Describe(layers))
		return nil
	}

	// Settled once, so the figure compaction plans against and the one the
	// startup line reports cannot drift apart.
	window, windowSource := contextWindow(cfg, selected)

	hub := event.NewHub()

	// Both halves of the gateway at once: accepting messages without routing
	// replies back would answer into the void, and the two are built together
	// so that cannot be half-configured.
	plane := gateway.NewPlane(store, nil, permissions, artifacts, nil,
		func() string { return id.WithPrefix("dsp") }, time.Now, logger)
	plane.Projector.Provider = modelProvider.Name()
	plane.Projector.Model = selected.ID
	plane.Projector.WorkingInterval = cfg.Gateway.WorkingInterval
	plane.Projector.StreamInterval = cfg.Gateway.StreamInterval

	rt := runtime.New(rootCtx, runtime.Options{
		Store:       store,
		Hub:         hub,
		Provider:    modelProvider,
		Model:       selected.ID,
		Tools:       tools,
		Permissions: permissions,
		Delivery:    plane.Projector,
		Coalescing: runtime.Coalescing{
			TextFlushBytes:     cfg.Delivery.TextFlushBytes,
			TextFlushInterval:  cfg.Delivery.TextFlushInterval,
			UsageFlushInterval: cfg.Delivery.UsageFlushInterval,
		},
		KeepAfterFold: cfg.Context.KeepAfterFold,
		ContextBudget: runtime.ContextBudget{
			Window:        window,
			CompactAt:     cfg.Context.CompactAt,
			KeepFraction:  cfg.Context.KeepFraction,
			SummaryTokens: cfg.Context.SummaryTokens,
		},
		Attachments:        artifacts,
		MaxImageBytes:      cfg.Artifacts.MaxImageBytes,
		SystemPrompt:       prompt.Render(layers),
		WorkerSystemPrompt: prompt.Render(prompt.KeepForWorker(layers)),
		SystemPromptFor:    standingDirections(cfg, memoryOptions, logger),
		Recall:             notesBeforeTurns(cfg, memoryOptions),
		AfterRun: notesAfterRuns(cfg, memoryOptions, store, &modelCompleter{
			provider: modelProvider,
			model:    selected.ID,
		}),
		MaxIterations: cfg.Agent.MaxIterations,
		NewSessionID:  func() string { return id.WithPrefix("ses") },
		NewRunID:      func() string { return id.WithPrefix("run") },
		NewMessageID:  func() string { return id.WithPrefix("msg") },
		NewEventID:    func() string { return id.WithPrefix("evt") },
		NewApprovalID: func() string { return id.WithPrefix("apr") },
		NewPlanItemID: planItemIDs(),
		NewQuestionID: func() string { return id.WithPrefix("qst") },
		NewScheduleID: func() string { return id.WithPrefix("sch") },
		Now:           time.Now,
		Logger:        logger,
	})

	// The handle the tools were registered against, filled in. Before
	// anything serves, so no run can reach a tool whose collaborator is still
	// missing.
	later.is = rt

	// Runs that were live when this process last stopped have nobody driving
	// them. Resolving them before serving means clients never see a run that
	// can no longer make progress.
	recovered, err := rt.RecoverOrphanedRuns(rootCtx)
	if err != nil {
		return err
	}
	if recovered > 0 {
		logger.Warn("resolved runs orphaned by a previous shutdown", "count", recovered)
	}

	// Schedules are reconciled from here on: once immediately, because a
	// machine that was asleep ran no timers and has to work out what came due
	// while it was gone, and then every minute.
	go watchSchedules(rootCtx, rt, logger)

	controlToken, err := control.NewToken(control.ScopeControl)
	if err != nil {
		return err
	}
	// A separate, narrower credential for gateway processes. One that could
	// also execute tools would make a compromised chat library equivalent to a
	// compromised daemon.
	gatewayToken, err := control.NewToken(control.ScopeGateway)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Addr, err)
	}

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	port := fmt.Sprintf("%d", tcpAddr.Port)
	baseURL := "http://" + net.JoinHostPort("127.0.0.1", port)

	// The runtime does not exist until after the options are built, so the
	// ingress is completed here rather than taking a half-built runtime.
	// The channels the file declares are put into effect before anything can
	// arrive through one.
	if err := applyChannels(rootCtx, store, cfg, logger); err != nil {
		return err
	}

	plane.Ingress.Runtime = rt

	// The console's extra reach is attached alongside, and only here. A
	// channel bound to the console profile can decide the approvals raised by
	// its own conversations; every other binding sees the narrow interface
	// and cannot decide anything.
	plane.Ingress.Decisions = rt

	api := http.NewServeMux()
	api.Handle(controlv1connect.NewSessionServiceHandler(
		control.NewServer(rt, store, hub, artifacts, modelProvider, selected.ID, processes)))
	api.Handle(controlv1connect.NewGatewayIngressServiceHandler(
		control.NewGatewayServer(plane.Ingress, store, time.Now)))
	api.Handle(controlv1connect.NewArtifactServiceHandler(control.NewArtifactServer(artifacts)))
	api.Handle(controlv1connect.NewMemoryServiceHandler(control.NewMemoryServer(store)))
	api.Handle(controlv1connect.NewChannelServiceHandler(
		control.NewChannelServer(store, func() string { return id.WithPrefix("bnd") }, time.Now)))

	// Everything sits behind the credential check. There is no unauthenticated
	// path: the browser console that needed one is gone, and the terminal
	// client reads its credential from the discovery file like the CLI does.
	guarded := control.AuthMiddleware(
		[]control.Token{controlToken, gatewayToken}, port, api)

	root := http.NewServeMux()
	for _, service := range []string{
		controlv1connect.SessionServiceName,
		controlv1connect.ArtifactServiceName,
		controlv1connect.ChannelServiceName,
		controlv1connect.GatewayIngressServiceName,
		controlv1connect.MemoryServiceName,
	} {
		root.Handle("/"+service+"/", guarded)
	}
	// h2c so a gRPC client can reach the same endpoint as Connect and
	// gRPC-Web over plaintext loopback.
	handler := root
	server := &http.Server{
		Handler:           h2c.NewHandler(handler, &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	discoveryPath, err := discovery.PathIn(cfg.Server.RuntimeDir)
	if err != nil {
		_ = listener.Close()
		return err
	}
	if err := discovery.Write(discoveryPath, discovery.File{
		PID:             os.Getpid(),
		BaseURL:         baseURL,
		Token:           controlToken.Value,
		GatewayToken:    gatewayToken.Value,
		ProtocolVersion: discovery.ProtocolVersion,
	}); err != nil {
		_ = listener.Close()
		return err
	}
	// Only if it still describes this process. A daemon that has been replaced
	// must not delete its replacement's file on the way out, which would leave
	// a running daemon no client can find.
	defer func() {
		if err := discovery.RemoveIfOwnedBy(discoveryPath, os.Getpid()); err != nil {
			logger.Warn("could not remove the discovery file", "error", err)
		}
	}()

	logger.Info("jingclaw daemon listening",
		"base_url", baseURL,
		"provider", modelProvider.Name(),
		"model", selected.ID,
		"context_window", window,
		"context_window_from", string(windowSource),
		"workspace", ws.Root(),
		"permission_profile", permissions.Profile(),
		// What this machine will actually enforce, rather than whether it was
		// asked for. On Linux the two are not the same: the filesystem rules
		// and the network rules arrived four kernel versions apart.
		"confinement", describeConfinement(cfg),
		"config_file", configFile,
		"database", dbPath,
		"discovery", discoveryPath,
	)
	// Human-facing line on stdout; the structured log goes to stderr.
	fmt.Printf("JingClaw daemon\nListening: %s\nConfig:    %s\nProvider:  %s\nModel:     %s\nWorkspace: %s\nTools:     %s\nDatabase:  %s\nDiscovery: %s\n",
		baseURL, describeConfigFile(configFile, createdConfig, seededConfig), modelProvider.Name(), selected.ID,
		ws.Root(), describeTools(tools, servers, cfg), dbPath, discoveryPath)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case err := <-serveErr:
		return err
	case <-rootCtx.Done():
	}

	logger.Info("shutting down")

	// Ordered teardown, and each phase gets its own window.
	//
	// One shared deadline meant the http drain consumed all of it: a gateway
	// or a console holds a stream open, that stream is an in-flight request,
	// and Shutdown waits for it. By the time the runtime was asked to drain
	// there was no time left, so it was never actually waited for — the
	// process exited while a run goroutine could still be writing.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), httpDrainGrace)
	err = server.Shutdown(drainCtx)
	cancelDrain()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		logger.Warn("http shutdown", "error", err)
	}

	// Whatever is left is a stream nobody is going to close. Dropping them is
	// safe: the log is the truth, and a client reconnects and reads what it
	// missed from there.
	_ = server.Close()

	// The one that matters. A run goroutine still writing when the process
	// exits leaves a log that ends without a terminal event, and a client
	// reattaching afterwards sees a run that never finished and never failed.
	runCtx, cancelRun := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelRun()
	if err := rt.Shutdown(runCtx); err != nil {
		logger.Warn("runtime shutdown", "error", err)
	}

	logger.Info("stopped")
	return nil
}

// databasePath resolves where state is kept. An explicit --data-dir wins so a
// test or a second instance can be fully isolated.
// reportPaths says where this deployment keeps things.
//
// Everything that resolves from a directory, a setting or a platform default
// resolves in one place; a script or an operator asking where the discovery
// file is should not have to reimplement that and then drift from it.
func reportPaths(cfg config.Config, configFile string) error {
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
		{"config", orNone(configFile)},
		{"workspace", workspaceRoot()},
		{"database", database},
		{"artifacts", artifactDir(database)},
		{"discovery", discoveryPath},
	} {
		fmt.Printf("%-10s %s\n", line[0], line[1])
	}
	return nil
}

func orNone(path string) string {
	if path == "" {
		return "(none)"
	}
	return path
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

	// The configuration goes in with it, so the directory is usable rather
	// than merely present.
	if err := os.WriteFile(dir.ConfigFile(), []byte(config.Example), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dir.ConfigFile(), err)
	}

	if err := writeInstructionFiles(dir.Root); err != nil {
		return err
	}

	fmt.Printf("Created %s\n\n", dir.Root)
	fmt.Printf("  %-12s the settings, all at their defaults\n", ConfigName())
	for _, name := range config.InstructionFiles() {
		fmt.Printf("  %-12s %s\n", name, instructionPurpose[name])
	}
	fmt.Printf("  %-12s what the agent may read and change\n", home.WorkspaceName+"/")
	fmt.Printf("  %-12s the database and stored output\n", home.DataName+"/")
	fmt.Printf("  %-12s how clients find this daemon\n", home.RunName+"/")
	fmt.Printf("\nCredentials go in beside them, mode 600.\n")
	return nil
}

// instructionPurpose is the one line each starter file says about itself,
// used both as its own first line and in what --init prints.
var instructionPurpose = map[string]string{
	config.InstructionsFile: "how work is done here",
	config.PersonaFile:      "who this agent is, and what it is for",
}

// writeInstructionFiles puts the standing-instruction files in the deployment
// directory.
//
// Beside the settings rather than inside the workspace. They describe the
// agent, and the workspace is what the agent may change: in there, its own
// instructions are a file it can edit while doing a job, and they sit among a
// project's files as though they were part of it.
//
// Created rather than documented. A file that exists is a file somebody edits;
// one they have to know to create is one that stays absent. Which is why the
// daemon does this on every start and not only under --init.
//
// Each is a heading and its purpose, and nothing else. Filling them with
// suggested content would mean every deployment starts with instructions
// nobody wrote and few will read.
func writeInstructionFiles(at string) error {
	for _, name := range config.InstructionFiles() {
		path := filepath.Join(at, name)
		if _, err := os.Stat(path); err == nil {
			continue
		}

		heading := strings.TrimSuffix(name, filepath.Ext(name))
		body := fmt.Sprintf("# %s\n\n<!-- %s -->\n", heading, instructionPurpose[name])
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// ConfigName is the configuration file's name, for the notice above.
func ConfigName() string { return home.ConfigName }

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
// install has a workspace for the same reason it has a configuration file:
// because starting created one.
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
		key, err := secret.Find(cfg.Web.Search.KeyEnv, cfg.Web.Search.KeyFile)
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

// describeConfigFile says where the settings came from, and whether this run
// is the one that put the file there.
func describeConfigFile(path string, created, seeded bool) string {
	if path == "" {
		return "(none)"
	}
	// Which of the two ways it was created, because they lead somewhere
	// different. Defaults mean nothing was configured; a file from the
	// environment means it was, and calling that defaults sends somebody
	// looking for why their settings were ignored on the one run where they
	// were first applied. Both come from what this run actually did rather
	// than from the variable, which stays set on every later run too.
	switch {
	case seeded:
		return path + " (created from " + config.FileEnvVar + ")"
	case created:
		return path + " (created, all defaults)"
	}
	return path
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
			return nil, fmt.Errorf(
				"no Gemini API key: set %s, or write it with mode 600 to one of: %s",
				strings.Join(cfg.Provider.Gemini.APIKeyEnv, " or "), strings.Join(keyFiles, ", "))
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
			return nil, fmt.Errorf(
				"no Anthropic API key: set %s, or write it with mode 600 to one of: %s",
				strings.Join(cfg.Provider.Anthropic.APIKeyEnv, " or "),
				strings.Join(keyFiles, ", "))
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
			"unknown provider %q; use gemini, ollama, openai_compat, or fake", cfg.Provider.Backend)
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

// applyFlagOverrides lets a typed flag win over the file.
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
func confinement(cfg config.Config, logger *slog.Logger) (*builtin.Confinement, error) {
	if !cfg.Sandbox.Enabled {
		return nil, nil
	}
	if !sandbox.Available() {
		// Said at startup rather than at the first command, and said as a
		// refusal: the alternative is a deployment that believes it is
		// confining and is not.
		return nil, errors.New(
			"[sandbox] enabled is on and this machine cannot confine a command. " +
				"macOS uses sandbox-exec, which every Mac has; Linux uses landlock, " +
				"which needs a kernel that has it enabled. Nowhere else is " +
				"implemented. Turn it off, or run it somewhere it works")
	}

	dir, found := home.Resolve()
	if !found {
		return nil, fmt.Errorf("[sandbox] enabled needs a deployment directory to keep its caches in")
	}

	dirs, err := sandbox.Under(filepath.Join(dir.Root, "sandbox"))
	if err != nil {
		return nil, err
	}

	// The deployment's own directory, always. Its credentials are in there,
	// and a confined command that could read them would be confined against
	// writing and not against the thing worth protecting.
	hidden := append([]string{dir.Root}, expandHomes(cfg.Sandbox.Hidden)...)

	logger.Info("commands are confined",
		"writable", dirs.Writable(), "network", cfg.Sandbox.Network, "hidden", hidden)

	return &builtin.Confinement{
		Policy: sandbox.Policy{
			Writable:   dirs.Writable(),
			Unreadable: hidden,
			Network:    cfg.Sandbox.Network,
		},
		Environment: dirs.Environment(),
	}, nil
}

// expandHomes turns a leading ~ into the real home directory.
//
// Written by hand in a configuration file, "~/.ssh" is what somebody means
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
