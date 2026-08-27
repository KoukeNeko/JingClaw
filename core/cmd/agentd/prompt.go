package main

import (
	"fmt"
	"os"
	goruntime "runtime"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/prompt"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// buildPrompt assembles what the agent is told, from configuration, the
// environment, and any instructions the workspace carries.
func buildPrompt(
	cfg config.Config,
	ws *workspace.Workspace,
	tools *tool.Registry,
) ([]prompt.Layer, error) {
	instructions, err := readWorkspaceInstructions(ws, cfg.Agent.InstructionFiles)
	if err != nil {
		return nil, err
	}

	specs := tools.Specs()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}

	return prompt.Build(
		prompt.Config{
			AgentName:    cfg.Agent.Name,
			Persona:      cfg.Agent.Persona,
			Instructions: cfg.Agent.Instructions,
		},
		prompt.Environment{
			WorkspaceRoot: ws.Root(),
			OS:            goruntime.GOOS,
			Arch:          goruntime.GOARCH,
			ToolNames:     names,
		},
		instructions,
	), nil
}

// readWorkspaceInstructions loads instruction files a project carries.
//
// A project saying how it wants to be worked on is ordinary and useful, and
// AGENTS.md is a convention several tools already share. What such a file
// cannot do is grant permissions: it is read as directions, and the policy
// engine never sees it.
func readWorkspaceInstructions(
	ws *workspace.Workspace,
	names []string,
) ([]prompt.WorkspaceInstructions, error) {
	var found []prompt.WorkspaceInstructions

	for _, name := range names {
		absolute, err := ws.Resolve(name)
		if err != nil {
			// A configured name that escapes the workspace is a mistake worth
			// reporting rather than quietly skipping.
			return nil, fmt.Errorf("instruction file %s: %w", name, err)
		}

		content, err := os.ReadFile(absolute)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		found = append(found, prompt.WorkspaceInstructions{Path: name, Text: string(content)})
	}

	return found, nil
}
