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
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/id"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
)

const shutdownGrace = 10 * time.Second

func main() {
	if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("agentd exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		devFake    = flag.Bool("dev-fake", false, "use the deterministic fake provider (the only option in M0)")
		addr       = flag.String("addr", "127.0.0.1:0", "loopback address to listen on; port 0 picks a free one")
		chunkDelay = flag.Duration("fake-delay", 150*time.Millisecond, "delay between fake provider chunks")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if !*devFake {
		return errors.New("M0 has no real provider yet; start with --dev-fake")
	}

	// Signal-aware root context. Everything the daemon owns descends from it,
	// so one Ctrl+C unwinds the whole tree.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := event.NewMemoryStore(id.New, time.Now)
	hub := event.NewHub()

	rt := runtime.New(rootCtx, runtime.Options{
		Store:        store,
		Hub:          hub,
		Provider:     fake.New(*chunkDelay),
		NewSessionID: func() string { return id.WithPrefix("ses") },
		NewRunID:     func() string { return id.WithPrefix("run") },
		NewMessageID: func() string { return id.WithPrefix("msg") },
		NewEventID:   func() string { return id.WithPrefix("evt") },
		Now:          time.Now,
	})

	token, err := control.NewToken()
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	port := fmt.Sprintf("%d", tcpAddr.Port)
	baseURL := "http://" + net.JoinHostPort("127.0.0.1", port)

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewSessionServiceHandler(control.NewServer(rt, store, hub)))

	// h2c so a gRPC client (the Windows client will use grpc-dotnet) can reach
	// the same endpoint as Connect and gRPC-Web over plaintext loopback.
	handler := control.AuthMiddleware(token, port, mux)
	server := &http.Server{
		Handler:           h2c.NewHandler(handler, &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	discoveryPath, err := discovery.Path()
	if err != nil {
		_ = listener.Close()
		return err
	}
	if err := discovery.Write(discoveryPath, discovery.File{
		PID:             os.Getpid(),
		BaseURL:         baseURL,
		Token:           token,
		ProtocolVersion: discovery.ProtocolVersion,
	}); err != nil {
		_ = listener.Close()
		return err
	}
	defer func() { _ = os.Remove(discoveryPath) }()

	logger.Info("jingclaw daemon listening",
		"base_url", baseURL,
		"provider", "fake",
		"discovery", discoveryPath,
	)
	// Human-facing line on stdout; the structured log goes to stderr.
	fmt.Printf("JingClaw daemon\nListening: %s\nProvider:  fake\nDiscovery: %s\n", baseURL, discoveryPath)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case err := <-serveErr:
		return err
	case <-rootCtx.Done():
	}

	logger.Info("shutting down")

	// Ordered teardown: stop accepting, let in-flight streams close, then wait
	// for run goroutines. Never os.Exit while work is outstanding.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "error", err)
	}
	if err := rt.Shutdown(shutdownCtx); err != nil {
		logger.Warn("runtime shutdown", "error", err)
	}

	logger.Info("stopped")
	return nil
}
