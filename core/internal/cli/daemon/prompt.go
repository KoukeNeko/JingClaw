package daemon

import (
	"log/slog"
	goruntime "runtime"
	"sort"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"github.com/KoukeNeko/JingClaw/core/internal/prompt"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// buildPrompt assembles what the agent is told, from the environment and the
// two documents that say who it is and how work is done here. It also says
// which skills the catalogue in it offers, because that is a fact about this
// prompt and not about the disk: the skills service compares the two.
//
// The documents arrive as text, from the snapshot the daemon took when it
// started, and are not read again here. The hash the control plane calls
// running is the hash of the same bytes, so "running" means what was put in
// front of the model and not what happened to be saved when the prompt was
// built.
//
// They go in under the names AGENTS.md and PERSONA.md, which is what the model
// has always been told they are called, in that order. They are not files any
// more; the names are only how a line of the prompt says where it came from.
func buildPrompt(
	cfg config.Config,
	ws *workspace.Workspace,
	tools *tool.Registry,
	servers *mcp.Manager,
	agents, persona string,
	logger *slog.Logger,
) ([]prompt.Layer, control.OfferedSkills) {
	specs := tools.Specs()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}

	catalogue, offered := skillCatalogue(logger)

	return prompt.Build(
		prompt.Environment{
			WorkspaceRoot:   ws.Root(),
			OS:              goruntime.GOOS,
			Arch:            goruntime.GOARCH,
			ToolNames:       names,
			DeferredServers: deferredServers(servers),
		},
		[]prompt.StandingInstructions{
			{Path: config.InstructionsFile, Text: agents},
			{Path: config.PersonaFile, Text: persona},
		},
		catalogue,
	), offered
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
//
// The skills it lists are returned with it, as they were read, so that what the
// model was told can be compared with the disk later.
func skillCatalogue(logger *slog.Logger) (string, control.OfferedSkills) {
	dir, found := home.Resolve()
	if !found {
		return "", nil
	}

	installed, rejected, err := skill.Installed(dir.Skills())
	if err != nil {
		logger.Warn("could not read the installed skills", "error", err)
		return "", nil
	}

	for _, one := range rejected {
		logger.Warn("a skill could not be read and is not offered",
			"skill", one.Name, "reason", one.Reason)
	}

	return skill.Catalogue(installed), control.OfferedBy(installed)
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
