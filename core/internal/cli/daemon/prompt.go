package daemon

import (
	"fmt"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/prompt"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// buildPrompt assembles what the agent is told, from the environment and the
// standing-instruction files the deployment carries.
func buildPrompt(
	cfg config.Config,
	ws *workspace.Workspace,
	tools *tool.Registry,
	servers *mcp.Manager,
	logger *slog.Logger,
) ([]prompt.Layer, error) {
	instructions, err := readStandingInstructions()
	if err != nil {
		return nil, err
	}

	specs := tools.Specs()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}

	return prompt.Build(
		prompt.Environment{
			WorkspaceRoot:   ws.Root(),
			OS:              goruntime.GOOS,
			Arch:            goruntime.GOARCH,
			ToolNames:       names,
			DeferredServers: deferredServers(servers),
		},
		instructions,
		skillCatalogue(logger),
	), nil
}

// skillCatalogue lists what an operator has installed, for the prompt.
//
// A skill that could not be read is said about rather than dropped: one that
// silently does not appear is an afternoon somebody spends finding out why,
// and the reason is always in the file.
//
// Failing to read the directory at all is not worth refusing to start over.
// An agent with no skills is the ordinary case, and one that will not run
// because a directory is unreadable is worse than one that runs without them.
func skillCatalogue(logger *slog.Logger) string {
	dir, found := home.Resolve()
	if !found {
		return ""
	}

	installed, rejected, err := skill.Installed(dir.Skills())
	if err != nil {
		logger.Warn("could not read the installed skills", "error", err)
		return ""
	}

	for _, one := range rejected {
		logger.Warn("a skill could not be read and is not offered",
			"skill", one.Name, "reason", one.Reason)
	}

	return skill.Catalogue(installed)
}

// readStandingInstructions loads instruction files a project carries.
//
// A project saying how it wants to be worked on is ordinary and useful, and
// AGENTS.md is a convention several tools already share. What such a file
// cannot do is grant permissions: it is read as directions, and the policy
// engine never sees it.
// readStandingInstructions reads the files that say who the agent is and how
// it works.
//
// From the deployment directory, not the workspace. They describe the agent
// rather than the work, and the workspace is what the agent may change: kept
// in there, the agent's own instructions are a file it can edit while doing a
// job, and they sit among a project's files as though they were part of it.
//
// A missing one is not an error. The pair is created on the first start and
// either may be emptied or removed by somebody who does not want it.
func readStandingInstructions() ([]prompt.StandingInstructions, error) {
	dir, found := home.Resolve()
	if !found {
		return nil, nil
	}

	var carried []prompt.StandingInstructions

	for _, name := range config.InstructionFiles() {
		content, err := os.ReadFile(filepath.Join(dir.Root, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		carried = append(carried, prompt.StandingInstructions{
			Path: name,
			Text: string(content),
		})
	}

	return carried, nil
}

// deferredServers is the catalogue line for each server whose tools are kept
// out of the prompt: name, how many, what they are gated at. Sorted, so the
// prefix a provider caches is the same every start.
func deferredServers(servers *mcp.Manager) []prompt.DeferredServer {
	if servers == nil {
		return nil
	}
	var out []prompt.DeferredServer
	for _, server := range servers.Servers() {
		if !server.Deferred() || len(server.Tools()) == 0 {
			continue
		}
		out = append(out, prompt.DeferredServer{
			Name:  server.Name(),
			Tools: len(server.Tools()),
			Level: server.Level().String(),
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}
