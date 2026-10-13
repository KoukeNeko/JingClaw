package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/id"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/process"
	"github.com/KoukeNeko/JingClaw/core/internal/prompt"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// agent is everything the daemon builds in order to do work: the provider, the
// tools, the runtime and what serves them.
//
// A daemon whose settings cannot be used has none of it. Building one is the
// part of starting that can fail because of the settings or the machine, and
// is kept apart from serving for that reason: the daemon that cannot build one
// still serves, so that the settings can be corrected.
type agent struct {
	provider    provider.Provider
	model       provider.ModelInfo
	workspace   *workspace.Workspace
	artifacts   *artifact.Store
	tools       *tool.Registry
	servers     *mcp.Manager
	processes   *process.Manager
	permissions *permission.Engine
	memory      memorytool.Options
	hub         *event.Hub
	plane       *gateway.Plane
	rt          *runtime.Runtime

	window       int64
	windowSource provider.ContextSource

	// offeredSkills are the skills the catalogue in the prompt lists, which is
	// what the agent knows of them until it is restarted.
	offeredSkills control.OfferedSkills

	// cleanups are run in reverse by close: the child processes the agent
	// started, which nothing else would end.
	cleanups []func()
}

// close stops what the agent started. Safe on one that was only partly built.
func (a *agent) close() {
	for i := len(a.cleanups) - 1; i >= 0; i-- {
		a.cleanups[i]()
	}
}

// agentSpec is what an agent is built from.
type agentSpec struct {
	cfg    config.Config
	store  *sqlite.Store
	dbPath string
	logger *slog.Logger

	// agents and persona are the standing instructions, as the snapshot holds
	// them.
	agents, persona string

	// listModels and printPrompt are questions asked of the build, answered
	// as soon as it can and then nothing runs.
	listModels, printPrompt bool
}

// startAgent builds the agent from the settings.
//
// Every failure here is returned and leaves nothing running: whatever was
// started on the way is stopped before it returns. Nil with no error is a
// question that was asked and answered (--list-models, --print-prompt), after
// which there is nothing to serve.
func startAgent(rootCtx context.Context, spec agentSpec) (*agent, error) {
	var (
		cfg    = spec.cfg
		store  = spec.store
		logger = spec.logger
		a      = &agent{}
		keep   bool
	)
	defer func() {
		if !keep {
			a.close()
		}
	}()

	modelProvider, err := buildProvider(rootCtx, cfg)
	if err != nil {
		return nil, err
	}
	a.provider = modelProvider

	if spec.listModels {
		return nil, printModels(rootCtx, modelProvider)
	}

	selected, err := resolveModel(rootCtx, modelProvider, cfg.Provider.Model())
	if err != nil {
		return nil, err
	}
	a.model = selected

	workspaceDir, err := ensureWorkspace()
	if err != nil {
		return nil, err
	}

	ws, err := workspace.Open(workspaceDir)
	if err != nil {
		return nil, err
	}
	a.workspace = ws

	artifacts, err := artifact.Open(artifactDir(spec.dbPath), cfg.Artifacts.MaxBytes)
	if err != nil {
		return nil, err
	}
	a.artifacts = artifacts

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
	a.tools = tools
	confine, err := confinement(cfg, spec.dbPath, logger)
	if err != nil {
		return nil, err
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
	a.processes = processes
	a.cleanups = append(a.cleanups, processes.CloseAll)

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
		return nil, err
	}

	// Tool servers are started before the prompt is assembled, because what
	// they offer is part of what the agent can do and the prompt says so.
	//
	// They are child processes of this daemon, so they are shut down with it;
	// the cleanup is registered immediately, before anything else can fail and
	// leave them running.
	servers := mcp.Start(rootCtx, mcpServers(cfg, sessions), mcp.Limits{
		StartTimeout: cfg.MCP.StartTimeout,
		CallTimeout:  cfg.MCP.CallTimeout,
		MaxOutput:    cfg.MCP.MaxOutput,
	}, artifacts, logger)
	a.servers = servers
	a.cleanups = append(a.cleanups, func() {
		if err := servers.Close(); err != nil {
			logger.Warn("an mcp server did not shut down cleanly", "error", err)
		}
	})

	if err := servers.Register(tools); err != nil {
		return nil, err
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
		embedder, err := buildEmbedder(cfg.Memory.Embedding)
		if err != nil {
			return nil, err
		}
		if embedder != nil {
			memoryOptions.Embedder = embedder
			memoryOptions.MinSimilarity = cfg.Memory.Embedding.MinSimilarity
			memoryOptions.RecallMinSimilarity = cfg.Memory.Embedding.RecallMinSimilarity
		}

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
	a.memory = memoryOptions

	// Reading the web is registered last among the built-ins, and only when an
	// operator asked for it. A failure to resolve the interpreter is reported
	// now rather than the first time the model asks for a page: an agent that
	// advertises a tool it cannot run wastes a turn discovering that.
	if fetcher, err := webFetcher(cfg); err != nil {
		return nil, err
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
		return nil, err
	} else if searcher != nil {
		tools.MustRegister(&builtin.WebSearch{
			Searcher:   searcher,
			MaxResults: cfg.Web.Search.MaxResults,
		})
		logger.Info("web search is on", "backend", searcher.Describe())
	}

	profile, ok := permission.ProfileByName(cfg.Agent.PermissionProfile)
	if !ok {
		return nil, fmt.Errorf("unknown permission profile %q", cfg.Agent.PermissionProfile)
	}
	permissions := permission.New(profile)
	a.permissions = permissions

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

	layers, offered := buildPrompt(cfg, ws, tools, servers, spec.agents, spec.persona, logger)
	a.offeredSkills = offered
	if spec.printPrompt {
		fmt.Print(prompt.Describe(layers))
		return nil, nil
	}

	// Settled once, so the figure compaction plans against and the one the
	// startup line reports cannot drift apart.
	a.window, a.windowSource = contextWindow(cfg, selected)

	a.hub = event.NewHub()

	// Both halves of the gateway at once: accepting messages without routing
	// replies back would answer into the void, and the two are built together
	// so that cannot be half-configured.
	plane := gateway.NewPlane(store, nil, permissions, artifacts, nil, nil,
		func() string { return id.WithPrefix("dsp") }, time.Now, logger)
	plane.Projector.Provider = modelProvider.Name()
	plane.Projector.Model = selected.ID
	plane.Projector.WorkingInterval = cfg.Gateway.WorkingInterval
	plane.Projector.StreamInterval = cfg.Gateway.StreamInterval
	a.plane = plane

	rt := runtime.New(rootCtx, runtime.Options{
		Store:       store,
		Hub:         a.hub,
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
			Window:        a.window,
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
		}, plane.Projector, later),
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
	a.rt = rt

	// The handle the tools were registered against, filled in. Before
	// anything serves, so no run can reach a tool whose collaborator is still
	// missing.
	later.is = rt

	// The runtime does not exist until after the options are built, so the
	// ingress is completed here rather than taking a half-built runtime.
	plane.Ingress.Runtime = rt

	// The console's extra reach is attached alongside, and only here. A
	// channel bound to the console profile can decide the approvals raised by
	// its own conversations; every other binding sees the narrow interface
	// and cannot decide anything.
	plane.Ingress.Decisions = rt
	plane.Ingress.Withdrawals = rt

	keep = true
	return a, nil
}
