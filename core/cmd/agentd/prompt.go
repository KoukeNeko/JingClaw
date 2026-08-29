package main

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/prompt"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// buildPrompt assembles what the agent is told, from the environment and the
// standing-instruction files the deployment carries.
func buildPrompt(
	cfg config.Config,
	ws *workspace.Workspace,
	tools *tool.Registry,
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
			WorkspaceRoot: ws.Root(),
			OS:            goruntime.GOOS,
			Arch:          goruntime.GOARCH,
			ToolNames:     names,
		},
		instructions,
	), nil
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
// A missing one is not an error. The pair is created by --init and either may
// be emptied or removed by somebody who does not want it.
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
