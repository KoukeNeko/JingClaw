package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Manager owns the connected servers for the life of the daemon.
type Manager struct {
	servers []*Server
	logger  *slog.Logger

	// needLogin names servers that answered, said they wanted authorizing,
	// and had nothing stored to present.
	needLogin []string
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
	artifacts *artifact.Store,
	logger *slog.Logger,
) *Manager {
	manager := &Manager{logger: logger}

	for _, cfg := range configs {
		server, err := Connect(ctx, cfg, limits, artifacts, logger)
		if err != nil {
			// A server nobody has signed in to is not a broken one, and
			// saying "did not start" about it sends whoever reads this
			// looking for a fault. What it needs is a person and a browser,
			// once, and the line says so.
			var login *mcpauth.NeedsLogin
			if errors.As(err, &login) {
				logger.Warn("an mcp server needs authorizing",
					"server", cfg.Name,
					"fix", "jingclaw mcp login "+cfg.Name)
				manager.needLogin = append(manager.needLogin, cfg.Name)
				continue
			}
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

// NeedLogin names the servers waiting on somebody to sign in.
//
// Kept apart from the ones that failed, because the two ask different things
// of whoever is reading. A server that could not start is a fault to
// investigate; this is a command to run.
func (m *Manager) NeedLogin() []string { return m.needLogin }

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
