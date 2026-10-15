package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/chatgptauth"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/id"
	chatgptprovider "github.com/KoukeNeko/JingClaw/core/internal/provider/chatgpt"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/webui"
)

// service is one RPC service the daemon answers for.
type service struct {
	path    string
	handler http.Handler
}

// mount takes what a generated NewXServiceHandler returns.
func mount(path string, handler http.Handler) service {
	return service{path: path, handler: handler}
}

// serving is what a daemon serves, and what from.
type serving struct {
	cfg    config.Config
	logger *slog.Logger
	store  *sqlite.Store

	// signin is the ChatGPT account.
	signin *chatgptauth.Auth

	dbPath        string
	discoveryPath string

	// settings is where the saved settings came from, for the startup line.
	settings string

	overrides []control.ConfigOverride

	// started are the hashes of the documents the agent was built from. Unused
	// without one.
	started control.StartedDocuments

	// begun is when this process began, before it read anything. A credential
	// file modified after it is one this process has not read.
	begun time.Time

	// candidate is how saved text becomes the settings this process would run
	// with: the defaults, the text, the environment and the flags it was given.
	candidate func(text []byte) (config.Config, error)

	// problems are why there is no agent, and agent is nil exactly then.
	problems []control.AgentProblem
	agent    *agent
}

// services are the RPC services this daemon answers for.
//
// The only list of them. The routes are built from it, so a service that is not
// here is not served at all, whatever a credential is allowed to ask for: a
// daemon with no agent has no sessions to read, and the answer to a request for
// one is that there is no such thing and not that it is refused.
//
// What a daemon with no agent keeps is the four that its repair needs. The
// settings are how it is corrected, the credentials are how a key it could not
// use is put right, the ChatGPT account is how the one backend with no key is
// signed in, and the pairing is how a browser gets in to do any of them. The
// skills are not among them: they are what the agent reads, and without one
// there is nothing for a change to them to be a change to.
func (s serving) services(browsers *control.Browsers, state control.AgentState) []service {
	started := s.started
	if s.agent == nil {
		// Nothing was built from the documents, so none of them is running.
		started = control.StartedDocuments{}
	}

	list := []service{
		mount(controlv1connect.NewConfigServiceHandler(
			control.NewConfigServer(s.store, s.cfg, s.overrides, started, state))),
		mount(controlv1connect.NewCredentialServiceHandler(
			control.NewCredentialServer(s.store, s.candidate, s.begun, s.logger))),
		mount(controlv1connect.NewChatGPTAuthServiceHandler(
			control.NewChatGPTServer(s.signin, chatgptprovider.New(chatgptprovider.Config{
				Tokens: s.signin, BaseURL: s.signin.APIBase(),
			}), s.cfg.Provider.Backend == "chatgpt", s.logger))),
		mount(controlv1connect.NewBrowserServiceHandler(control.NewBrowserServer(browsers))),
	}

	a := s.agent
	if a == nil {
		return list
	}
	return append(list,
		mount(controlv1connect.NewSessionServiceHandler(
			control.NewServer(a.rt, s.store, a.hub, a.artifacts, a.provider, a.model.ID, a.processes))),
		mount(controlv1connect.NewGatewayIngressServiceHandler(
			control.NewGatewayServer(a.plane.Ingress, s.store, s.cfg, time.Now))),
		mount(controlv1connect.NewArtifactServiceHandler(control.NewArtifactServer(a.artifacts))),
		mount(controlv1connect.NewMemoryServiceHandler(control.NewMemoryServer(s.store))),
		mount(controlv1connect.NewChannelServiceHandler(
			control.NewChannelServer(s.store, func() string { return id.WithPrefix("bnd") }, time.Now))),
		mount(controlv1connect.NewSkillServiceHandler(control.NewSkillServer(a.offeredSkills, s.logger))),
	)
}

// serve answers requests until the context ends, and then shuts down in order.
//
// The same listener, credentials, host check, pages and discovery file whether
// there is an agent or not. A daemon without one is not a second way in: it is
// the same door with fewer rooms behind it.
func serve(rootCtx context.Context, s serving) error {
	var (
		logger = s.logger
		a      = s.agent
		repair = a == nil
	)

	if !repair {
		// Runs that were live when this process last stopped have nobody driving
		// them. Resolving them before serving means clients never see a run that
		// can no longer make progress.
		recovered, err := a.rt.RecoverOrphanedRuns(rootCtx)
		if err != nil {
			return err
		}
		if recovered > 0 {
			logger.Warn("resolved runs orphaned by a previous shutdown", "count", recovered)
		}

		// Schedules are reconciled from here on: once immediately, because a
		// machine that was asleep ran no timers and has to work out what came due
		// while it was gone, and then every minute.
		go watchSchedules(rootCtx, a.rt, logger)

		// Memories written before there was an embedder, or under another
		// model, or while it was down, are given vectors in the background:
		// until then they are found by their words, the way they always were.
		go embedMissingMemories(rootCtx, a.memory, logger)

		// The channels the settings declare are put into effect before anything
		// can arrive through one.
		if err := applyChannels(rootCtx, s.store, s.cfg, logger); err != nil {
			return err
		}
	}

	controlToken, err := control.NewToken(control.ScopeControl)
	if err != nil {
		return err
	}
	tokens := []control.Token{controlToken}

	// A separate, narrower credential for gateway processes. One that could
	// also execute tools would make a compromised chat library equivalent to a
	// compromised daemon. Not made for a daemon with no ingress: there is
	// nothing for it to reach.
	var gatewayToken control.Token
	if !repair {
		gatewayToken, err = control.NewToken(control.ScopeGateway)
		if err != nil {
			return err
		}
		tokens = append(tokens, gatewayToken)
	}

	// What lets a browser in. It is given a credential of its own and never
	// the one above, which programs on this machine read from a file.
	browsers := control.NewBrowsers(time.Now)

	listener, err := listen(s.cfg.Server.Addr, repair, logger)
	if err != nil {
		return err
	}

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	port := fmt.Sprintf("%d", tcpAddr.Port)
	baseURL := "http://" + net.JoinHostPort("127.0.0.1", port)

	state := control.AgentState{Repair: repair, Problems: s.problems, StartedAt: time.Now()}

	// Everything that does anything sits behind the credential check. A
	// browser's credential is checked the same way, against the same list of
	// what it may reach; it differs only in having been issued to it.
	root := http.NewServeMux()
	for _, one := range s.services(browsers, state) {
		root.Handle(one.path, control.AuthMiddleware(tokens, browsers, port, one.handler))
	}

	// The page, and the one place a browser gets in. Neither takes a
	// credential: the page holds no secrets and can do nothing until it has
	// one, and a pairing code is the credential. The host check still stands
	// in front of both, which is what keeps a web page from elsewhere out.
	root.Handle("/ui/pair", control.RequireLoopbackHost(port, control.PairHandler(browsers)))
	root.Handle("/ui/", control.RequireLoopbackHost(port, http.StripPrefix("/ui/", webui.Handler())))

	// h2c so a gRPC client can reach the same endpoint as Connect and
	// gRPC-Web over plaintext loopback.
	server := &http.Server{
		Handler:           h2c.NewHandler(root, &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	found := discovery.File{
		PID:             os.Getpid(),
		BaseURL:         baseURL,
		Token:           controlToken.Value,
		GatewayToken:    gatewayToken.Value,
		ProtocolVersion: discovery.ProtocolVersion,
	}
	if repair {
		found.Mode = discovery.ModeRepair
	}
	if err := discovery.Write(s.discoveryPath, found); err != nil {
		_ = listener.Close()
		return err
	}
	// Only if it still describes this process. A daemon that has been replaced
	// must not delete its replacement's file on the way out, which would leave
	// a running daemon no client can find.
	defer func() {
		if err := discovery.RemoveIfOwnedBy(s.discoveryPath, os.Getpid()); err != nil {
			logger.Warn("could not remove the discovery file", "error", err)
		}
	}()

	if repair {
		announceRepair(s, baseURL)
	} else {
		announce(s, baseURL)
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
	// A daemon with no agent has no runs.
	if !repair {
		runCtx, cancelRun := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancelRun()
		if err := a.rt.Shutdown(runCtx); err != nil {
			logger.Warn("runtime shutdown", "error", err)
		}
	}

	logger.Info("stopped")
	return nil
}

// listen opens the address the daemon serves on.
//
// A daemon with no agent is on loopback whatever the settings say. They may be
// the reason there is no agent, and one of the mistakes they can hold is an
// address this API must never be on. It keeps the port when it can, so that
// whatever was told where to find the daemon still can; when the port will not
// do, a free one is better than a daemon nobody can reach to correct the line
// that named it.
func listen(addr string, repair bool, logger *slog.Logger) (net.Listener, error) {
	if !repair {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		return listener, nil
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = "0"
	}
	want := net.JoinHostPort("127.0.0.1", port)

	listener, err := net.Listen("tcp", want)
	if err == nil {
		return listener, nil
	}

	logger.Warn("the address the settings name cannot be used, so the repair daemon is on a free port",
		"address", want, "error", err)
	listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on 127.0.0.1:0: %w", err)
	}
	return listener, nil
}

// announce says that a daemon with an agent is up, and what it is made of.
func announce(s serving, baseURL string) {
	a := s.agent

	s.logger.Info("jingclaw daemon listening",
		"base_url", baseURL,
		"provider", a.provider.Name(),
		"model", a.model.ID,
		"context_window", a.window,
		"context_window_from", string(a.windowSource),
		"workspace", a.workspace.Root(),
		"permission_profile", a.permissions.Profile(),
		// What this machine will actually enforce, rather than whether it was
		// asked for. On Linux the two are not the same: the filesystem rules
		// and the network rules arrived four kernel versions apart.
		"confinement", describeConfinement(s.cfg),
		"settings", s.settings,
		"database", s.dbPath,
		"discovery", s.discoveryPath,
	)
	// Human-facing line on stdout; the structured log goes to stderr.
	fmt.Printf("JingClaw daemon\nListening: %s\nSettings:  %s\nProvider:  %s\nModel:     %s\nWorkspace: %s\nTools:     %s\nDatabase:  %s\nDiscovery: %s\n",
		baseURL, s.settings, a.provider.Name(), a.model.ID,
		a.workspace.Root(), describeTools(a.tools, a.servers, s.cfg), s.dbPath, s.discoveryPath)
}

// announceRepair says that a daemon with no agent is up, and why it has none.
func announceRepair(s serving, baseURL string) {
	var first string
	if len(s.problems) > 0 {
		first = describeProblem(s.problems[0])
	}

	// The same message as a daemon with an agent, so that whatever waits for it
	// finds a repair daemon too; the mode is what tells them apart.
	s.logger.Warn("jingclaw daemon listening",
		"mode", discovery.ModeRepair,
		"base_url", baseURL,
		"problems", len(s.problems),
		"first", first,
		"settings", s.settings,
		"database", s.dbPath,
		"discovery", s.discoveryPath,
	)

	fmt.Printf("JingClaw daemon (repair mode: no agent is running)\nListening: %s\nSettings:  %s\nDatabase:  %s\nDiscovery: %s\n\nWhy there is no agent:\n",
		baseURL, s.settings, s.dbPath, s.discoveryPath)
	for _, problem := range s.problems {
		fmt.Printf("  - %s\n", describeProblem(problem))
		if problem.Fix != "" {
			fmt.Printf("      %s\n", problem.Fix)
		}
	}
	fmt.Printf("\nFix it in the dashboard (run: jingclaw dashboard), then restart: jingclaw stop, then jingclaw.\n")
}
