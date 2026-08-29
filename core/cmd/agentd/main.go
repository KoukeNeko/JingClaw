// Command agentd is the JingClaw agent daemon.
//
// It owns every piece of durable state: sessions, runs, the event log and, in
// later milestones, tools and permissions. Control clients (GUI, CLI, web) are
// projections of it, so closing one never stops work that is already running.
package main

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
	"strings"
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
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/process"
	"github.com/KoukeNeko/JingClaw/core/internal/prompt"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/gemini"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/ollama"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/secret"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/web"
	"github.com/KoukeNeko/JingClaw/core/internal/webui"
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

func main() {
	if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("agentd exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "configuration file; defaults to the one in the config directory")
		printPrompt = flag.Bool("print-prompt", false, "print the assembled system prompt with its sources and exit")
		printConfig = flag.Bool("print-config", false, "print an example configuration file and exit")
		listModels  = flag.Bool("list-models", false, "print the provider's available models and exit")
		initHere    = flag.Bool("init", false, "create a "+home.DirName+" directory here and exit")
		printPaths  = flag.Bool("print-paths", false, "print where this deployment keeps things and exit")

		// Every flag below has a setting of the same meaning in the
		// configuration file, and exists only so one run can differ from it.
		// They carry no defaults of their own: the file already holds those,
		// and a second copy here would be one more place for them to disagree.
		providerName = flag.String("provider", "", "model provider: gemini, ollama, openai_compat, or fake")
		model        = flag.String("model", "", "model to use")
		addr         = flag.String("addr", "", "loopback address to listen on; port 0 picks a free one")
		dataDir      = flag.String("data-dir", "", "directory for the database")
		workspaceDir = flag.String("workspace", "", "directory the agent may read; tools cannot reach outside it")
		maxIters     = flag.Int("max-iterations", 0, "cap on tool iterations per run")
	)
	// Retained so existing invocations keep working.
	devFake := flag.Bool("dev-fake", false, "alias for --provider=fake")
	flag.Parse()

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
	var createdConfig bool
	if *configPath == "" {
		var err error
		if _, createdConfig, err = config.EnsureFile(); err != nil {
			return err
		}
	}

	cfg, configFile, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Flags win over the file, which wins over the defaults it was seeded
	// with. Only a flag the operator actually typed counts, so an unset flag
	// does not overwrite a configured value with a default.
	applyFlagOverrides(&cfg, providerName, model, workspaceDir, dataDir, addr, maxIters)
	if *devFake {
		cfg.Model.Provider = "fake"
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

	selected, err := resolveModel(rootCtx, modelProvider, cfg.Model.Model)
	if err != nil {
		return err
	}

	ws, err := workspace.Open(cfg.Workspace.Root)
	if err != nil {
		return err
	}

	artifacts, err := artifact.Open(artifactDir(cfg, dbPath), cfg.Artifacts.MaxBytes)
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
	tools.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed, Limits: limits},
		&builtin.GlobFiles{Workspace: ws, Limits: limits},
		&builtin.Grep{Workspace: ws, Limits: limits},
		writeFile,
		editFile,
		&builtin.ExecCommand{Workspace: ws, Limits: limits, Artifacts: artifacts},
		&builtin.ReadArtifact{Artifacts: artifacts, Limits: limits},
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
	)

	// Tool servers are started before the prompt is assembled, because what
	// they offer is part of what the agent can do and the prompt says so.
	//
	// They are child processes of this daemon, so they are shut down with it;
	// the defer is registered immediately, before anything else can fail and
	// leave them running.
	servers := mcp.Start(rootCtx, mcpServers(cfg), mcp.Limits{
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
		Now:          time.Now,
	}
	if cfg.Memory.Enabled {
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

	profile, ok := permission.ProfileByName(cfg.Agent.PermissionProfile)
	if !ok {
		return fmt.Errorf("unknown permission profile %q", cfg.Agent.PermissionProfile)
	}
	permissions := permission.New(profile)

	layers, err := buildPrompt(cfg, ws, tools)
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
		Attachments:     artifacts,
		MaxImageBytes:   cfg.Artifacts.MaxImageBytes,
		SystemPrompt:    prompt.Render(layers),
		SystemPromptFor: standingDirections(cfg, memoryOptions, logger),
		MaxIterations:   cfg.Agent.MaxIterations,
		NewSessionID:    func() string { return id.WithPrefix("ses") },
		NewRunID:        func() string { return id.WithPrefix("run") },
		NewMessageID:    func() string { return id.WithPrefix("msg") },
		NewEventID:      func() string { return id.WithPrefix("evt") },
		NewApprovalID:   func() string { return id.WithPrefix("apr") },
		Now:             time.Now,
		Logger:          logger,
	})

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

	// Browsers get one credential each, minted when they pair and recorded so
	// an operator can see what has been let in and revoke one of them without
	// revoking the rest — or the credential the CLI and the gateway hold.
	grants := control.NewGrants(cfg.Server.ConsoleTTL, time.Now,
		func() string { return id.WithPrefix("con") })
	pairing := control.NewPairing(grants, cfg.Server.PairingTTL, time.Now)

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
	plane.Ingress.Console = rt

	api := http.NewServeMux()
	api.Handle(controlv1connect.NewSessionServiceHandler(
		control.NewServer(rt, store, hub, artifacts, modelProvider, selected.ID)))
	api.Handle(controlv1connect.NewGatewayIngressServiceHandler(
		control.NewGatewayServer(plane.Ingress, store, time.Now)))
	api.Handle(controlv1connect.NewArtifactServiceHandler(control.NewArtifactServer(artifacts)))
	api.Handle(controlv1connect.NewConsoleServiceHandler(
		control.NewConsoleServer(pairing, grants, baseURL)))
	api.Handle(controlv1connect.NewMemoryServiceHandler(control.NewMemoryServer(store)))
	api.Handle(controlv1connect.NewChannelServiceHandler(
		control.NewChannelServer(store, func() string { return id.WithPrefix("bnd") }, time.Now)))

	// Everything that can do something sits behind the credential check. The
	// console's own files do not, because a browser cannot present a bearer
	// token on the request that fetches the page it would get one from, and
	// those files are code rather than data.
	guarded := control.AuthMiddleware(
		[]control.Token{controlToken, gatewayToken}, grants, port, api)

	root := http.NewServeMux()
	for _, service := range []string{
		controlv1connect.SessionServiceName,
		controlv1connect.ArtifactServiceName,
		controlv1connect.ChannelServiceName,
		controlv1connect.GatewayIngressServiceName,
		controlv1connect.ConsoleServiceName,
		controlv1connect.MemoryServiceName,
	} {
		root.Handle("/"+service+"/", guarded)
	}
	if cfg.Server.WebConsole {
		// The one request a browser makes before it has anything to
		// authenticate with. What protects it is that a code works once,
		// expires in minutes, and is eighty bits wide.
		root.Handle(control.RedeemPath,
			control.RequireLoopbackHost(port, pairing.RedeemHandler()))
		root.Handle("/", control.RequireLoopbackHost(port, webui.Handler()))
	}

	// h2c so a gRPC client (the Windows client will use grpc-dotnet) can reach
	// the same endpoint as Connect and gRPC-Web over plaintext loopback.
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
		"config_file", configFile,
		"database", dbPath,
		"discovery", discoveryPath,
	)
	// Human-facing line on stdout; the structured log goes to stderr.
	fmt.Printf("JingClaw daemon\nListening: %s\nConfig:    %s\nProvider:  %s\nModel:     %s\nWorkspace: %s\nTools:     %s\nDatabase:  %s\nDiscovery: %s\n",
		baseURL, describeConfigFile(configFile, createdConfig), modelProvider.Name(), selected.ID,
		ws.Root(), describeTools(tools, servers, cfg), dbPath, discoveryPath)

	if cfg.Server.WebConsole {
		// A code rather than the credential itself. This line is going to sit
		// in a terminal's scrollback — over SSH, on a machine somebody else
		// can scroll back through — and a code that works once and expires in
		// minutes is a very different thing to leave lying there.
		code, expires, err := pairing.Issue()
		if err != nil {
			return err
		}
		fmt.Printf("Console:   %s\n           valid once, until %s (agent console for another)\n",
			control.ConsoleURL(baseURL, code), expires.Format("15:04:05"))
	}

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
	if dir, found := home.FromWorkingDirectory(); found {
		root = dir.Root
	}

	for _, line := range [][2]string{
		{"home", root},
		{"config", orNone(configFile)},
		{"workspace", cfg.Workspace.Root},
		{"database", database},
		{"artifacts", artifactDir(cfg, database)},
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

// initialise creates a .JingClaw directory in the working directory.
//
// Explicit rather than automatic. A daemon that made one wherever it was
// started would leave them scattered through a filesystem, which is the
// problem this exists to solve rather than a way to solve it.
func initialise() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	if existing, found := home.Find(cwd); found {
		return fmt.Errorf("%s already covers this directory", existing.Root)
	}

	dir, err := home.Create(cwd)
	if err != nil {
		return err
	}

	// The configuration goes in with it, so the directory is usable rather
	// than merely present.
	if err := os.WriteFile(dir.ConfigFile(), []byte(config.Example), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dir.ConfigFile(), err)
	}

	fmt.Printf("Created %s\n\n", dir.Root)
	fmt.Printf("  %-12s the settings, all at their defaults\n", ConfigName())
	fmt.Printf("  %-12s what the agent may read and change\n", home.WorkspaceName+"/")
	fmt.Printf("  %-12s the database and stored output\n", home.DataName+"/")
	fmt.Printf("  %-12s how clients find this daemon\n", home.RunName+"/")
	fmt.Printf("\nCredentials go in beside them, mode 600.\n")
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
		if dir, found := home.FromWorkingDirectory(); found {
			dataDir = dir.Data()
		} else {
			base, err := os.UserConfigDir()
			if err != nil {
				return "", fmt.Errorf("locate config dir: %w", err)
			}
			dataDir = filepath.Join(base, "JingClaw")
		}
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
	if !cfg.Memory.Enabled {
		return nil
	}

	return func(ctx context.Context, run domain.Run) string {
		directions, err := memorytool.Instructions(ctx, options.Store, options, run,
			cfg.Memory.MaxInstructionBytes)
		if err != nil {
			// A run that cannot read memory is still a run. Failing it would
			// make an unavailable database into a broken agent.
			logger.Warn("could not read standing directions",
				"run_id", string(run.ID), "error", err)
			return ""
		}
		return directions
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

	roots := []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/dev/shm"}
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

func artifactDir(cfg config.Config, dbPath string) string {
	if cfg.Artifacts.Dir != "" {
		return cfg.Artifacts.Dir
	}
	return filepath.Join(filepath.Dir(dbPath), "artifacts")
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
		if err != nil {
			return nil, fmt.Errorf("web.backend is \"browser\": %w", err)
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

func mcpServers(cfg config.Config) []mcp.ServerConfig {
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

	return fmt.Sprintf("%s (%d from %d of %d mcp servers)",
		described, servers.ToolCount(), servers.Connected(), wanted)
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
func describeConfigFile(path string, created bool) string {
	if path == "" {
		return "(none)"
	}
	if created {
		return path + " (created, all defaults)"
	}
	return path
}

func buildProvider(ctx context.Context, cfg config.Config) (provider.Provider, error) {
	switch cfg.Model.Provider {
	case "fake":
		offline := fake.New(cfg.Model.FakeDelay)
		offline.Reasoning = cfg.Model.FakeReasoning
		for _, turn := range cfg.Model.FakeScript {
			offline.Script = append(offline.Script, fake.Turn{
				Text: turn.Text, Tool: turn.Tool, Args: turn.Args,
			})
		}
		return offline, nil

	case "gemini":
		keyFiles, err := secret.DefaultFiles(cfg.Model.APIKeyFile)
		if err != nil {
			return nil, err
		}

		apiKey, err := secret.Load(secret.LoadOptions{
			EnvVars: cfg.Model.APIKeyEnv,
			Files:   keyFiles,
		})
		if err != nil {
			return nil, err
		}
		if !apiKey.IsSet() {
			return nil, fmt.Errorf(
				"no Gemini API key: set %s, or write it with mode 600 to one of: %s",
				strings.Join(cfg.Model.APIKeyEnv, " or "), strings.Join(keyFiles, ", "))
		}

		p, err := gemini.New(ctx, gemini.Config{APIKey: apiKey.Reveal()})
		if err != nil {
			return nil, err
		}

		return provider.WithRetry(p, retryPolicy(cfg)), nil

	case "ollama":
		// A credential only if one was supplied: the hosted service needs one
		// and a local daemon has nothing to check it against.
		key, err := optionalKey(cfg)
		if err != nil {
			return nil, err
		}

		p, err := ollama.New(ollama.Config{
			BaseURL:   cfg.Model.Ollama.BaseURL,
			APIKey:    key,
			KeepAlive: cfg.Model.Ollama.KeepAlive,
			NumCtx:    cfg.Model.Ollama.NumCtx,
			Think:     cfg.Model.Ollama.Think,
		})
		if err != nil {
			return nil, err
		}
		return provider.WithRetry(p, retryPolicy(cfg)), nil

	case "openai_compat":
		key, err := optionalKey(cfg)
		if err != nil {
			return nil, err
		}

		p, err := openaicompat.New(openaicompat.Config{
			BaseURL: cfg.Model.OpenAICompat.BaseURL,
			APIKey:  key,
			Profile: cfg.Model.OpenAICompat.Profile,
			Name:    cfg.Model.OpenAICompat.Name,
		})
		if err != nil {
			return nil, err
		}
		return provider.WithRetry(p, retryPolicy(cfg)), nil

	default:
		return nil, fmt.Errorf(
			"unknown provider %q; use gemini, ollama, openai_compat, or fake", cfg.Model.Provider)
	}
}

// retryPolicy is shared by every provider that reaches the network, so a
// deployment cannot end up with one of them retrying on different terms from
// the rest.
func retryPolicy(cfg config.Config) provider.RetryPolicy {
	return provider.RetryPolicy{
		MaxAttempts: cfg.Model.Retry.MaxAttempts,
		BaseDelay:   cfg.Model.Retry.BaseDelay,
		MaxDelay:    cfg.Model.Retry.MaxDelay,
		Jitter:      cfg.Model.Retry.Jitter,
		Budget:      cfg.Model.Retry.Budget,
	}
}

// optionalKey loads a credential if one was configured, and is content when
// none was.
//
// A local model server usually has no credential at all, so a missing one is
// an ordinary state here rather than the startup failure it is for a hosted
// provider.
func optionalKey(cfg config.Config) (string, error) {
	files, err := secret.DefaultFiles(cfg.Model.APIKeyFile)
	if err != nil {
		return "", err
	}

	key, err := secret.Load(secret.LoadOptions{EnvVars: cfg.Model.APIKeyEnv, Files: files})
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
func applyFlagOverrides(cfg *config.Config, providerName, model, workspaceDir, dataDir, addr *string, maxIters *int) {
	passed := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	if passed["provider"] {
		cfg.Model.Provider = *providerName
	}
	if passed["model"] {
		cfg.Model.Model = *model
	}
	if passed["workspace"] {
		cfg.Workspace.Root = *workspaceDir
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
