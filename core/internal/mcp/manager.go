package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Manager owns the connected servers for the life of the daemon.
type Manager struct {
	servers []*Server
	logger  *slog.Logger
}

// Start connects every configured server.
//
// One that will not start is reported and skipped rather than taken as a
// reason to refuse to run. The alternative is an agent that cannot do anything
// at all because a tool nobody was going to use this session is broken. It is
// logged at error and counted in the banner, because a tool that is quietly
// absent looks exactly like one the model chose not to use.
func Start(
	ctx context.Context,
	configs []ServerConfig,
	limits Limits,
	logger *slog.Logger,
) *Manager {
	manager := &Manager{logger: logger}

	for _, cfg := range configs {
		server, err := Connect(ctx, cfg, limits, logger)
		if err != nil {
			logger.Error("an mcp server did not start", "server", cfg.Name, "error", err)
			continue
		}
		manager.servers = append(manager.servers, server)
	}

	return manager
}

// Register adds every connected server's tools to the registry.
//
// A name that is already taken is an error rather than a replacement: a server
// that could shadow read_file could quietly become the thing that reads files,
// and the prefixing in toolName exists precisely so this cannot happen by
// accident.
func (m *Manager) Register(registry *tool.Registry) error {
	for _, server := range m.servers {
		for _, adapted := range server.Tools() {
			if err := registry.Register(adapted); err != nil {
				return fmt.Errorf("mcp: register %s from %s: %w",
					adapted.Spec().Name, server.Name(), err)
			}
		}
	}
	return nil
}

// Connected is how many servers are answering, for a banner that says so.
func (m *Manager) Connected() int { return len(m.servers) }

// ToolCount is how many tools those servers contributed.
func (m *Manager) ToolCount() int {
	total := 0
	for _, server := range m.servers {
		total += len(server.Tools())
	}
	return total
}

// Close shuts every server down.
//
// Every one of them, even after a failure: these are child processes, and
// abandoning the rest because the first would not close is how a daemon
// restart leaves a machine with orphans on it.
func (m *Manager) Close() error {
	var failures []error

	for _, server := range m.servers {
		if err := server.Close(); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", server.Name(), err))
		}
	}

	return errors.Join(failures...)
}
