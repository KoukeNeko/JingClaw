// Package config loads the daemon's settings.
//
// Precedence runs defaults → file → environment → flags, so the more specific
// and more immediate a source is, the more it wins. Everything is decoded into
// a typed struct at startup rather than being queried by key later: a
// configuration library reaching into every corner of a program is how a typo
// in a key name becomes a silent default at runtime.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

const (
	appDir   = "JingClaw"
	fileName = "config.toml"

	// envPrefix keeps JingClaw's variables from colliding with anything else
	// in a shell.
	envPrefix = "JINGCLAW_"
)

// Config is the whole of the daemon's settings.
type Config struct {
	Agent     Agent     `koanf:"agent"`
	Model     Model     `koanf:"model"`
	Workspace Workspace `koanf:"workspace"`
	Server    Server    `koanf:"server"`
}

// Agent is who the agent is and what it has been told.
//
// None of this is a security control. It shapes how the agent presents itself
// and what it tries; what it is permitted to do is settled by the policy
// engine, which does not read any of these fields.
type Agent struct {
	// Name is what the agent calls itself. Empty leaves it unnamed, which is
	// better than insisting on one that contradicts whatever the account is
	// called wherever it is deployed.
	Name string `koanf:"name"`

	// Persona is extra identity: tone, stance, what this deployment is for.
	Persona string `koanf:"persona"`

	// Instructions are the operator's standing directions.
	Instructions string `koanf:"instructions"`

	// InstructionFiles are read from the workspace when present. AGENTS.md is
	// a convention several tools already share; adding a private filename as
	// well is cheap, inventing a new one and ignoring the shared one is not.
	InstructionFiles []string `koanf:"instruction_files"`

	// MaxIterations bounds the tool loop per run.
	MaxIterations int `koanf:"max_iterations"`
}

type Model struct {
	Provider string `koanf:"provider"`
	Model    string `koanf:"model"`
}

type Workspace struct {
	Root string `koanf:"root"`
}

type Server struct {
	// Addr must stay on the loopback interface. This API grows a shell, and
	// exposing it is a decision that deserves more than a config line.
	Addr string `koanf:"addr"`

	// DataDir holds the database. Empty means the platform's own location.
	DataDir string `koanf:"data_dir"`
}

// Defaults are what runs when nothing says otherwise.
func Defaults() Config {
	return Config{
		Agent: Agent{
			Name:             "JingClaw",
			InstructionFiles: []string{"AGENTS.md", "JINGCLAW.md"},
			MaxIterations:    12,
		},
		Model: Model{
			Provider: "fake",
		},
		Workspace: Workspace{
			Root: ".",
		},
		Server: Server{
			Addr: "127.0.0.1:0",
		},
	}
}

// Load reads the configuration.
//
// A missing file is not an error: running with defaults and no configuration
// at all has to work, or the first experience of the tool is editing TOML. A
// file that exists but cannot be parsed is an error, because silently ignoring
// it would run with settings the operator believes are in force.
func Load(path string) (Config, string, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return Config{}, "", fmt.Errorf("config: load defaults: %w", err)
	}

	resolved := path
	if resolved == "" {
		var err error
		if resolved, err = DefaultPath(); err != nil {
			return Config{}, "", err
		}
	}

	used := ""
	if err := k.Load(file.Provider(resolved), toml.Parser()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Config{}, "", fmt.Errorf("config: read %s: %w", resolved, err)
		}
		// An explicitly requested file that is not there is a mistake worth
		// reporting; the default one simply may not exist yet.
		if path != "" {
			return Config{}, "", fmt.Errorf("config: %s does not exist", path)
		}
	} else {
		used = resolved
	}

	// JINGCLAW_AGENT_NAME sets agent.name, and so on.
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix: envPrefix,
		TransformFunc: func(key, value string) (string, any) {
			key = strings.ToLower(strings.TrimPrefix(key, envPrefix))
			return strings.Replace(key, "_", ".", 1), value
		},
	}), nil); err != nil {
		return Config{}, "", fmt.Errorf("config: read environment: %w", err)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return Config{}, "", fmt.Errorf("config: decode: %w", err)
	}

	return cfg, used, nil
}

// DefaultPath is where the configuration lives when none is given.
func DefaultPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: locate config dir: %w", err)
	}

	platform := filepath.Join(base, appDir, fileName)
	if _, err := os.Stat(platform); err == nil {
		return platform, nil
	}

	// Developer tools on macOS commonly follow the XDG convention even though
	// os.UserConfigDir does not, and a headless machine is usually set up that
	// way, so an existing file there is honoured.
	if home, err := os.UserHomeDir(); err == nil {
		xdg := filepath.Join(home, ".config", appDir, fileName)
		if _, err := os.Stat(xdg); err == nil {
			return xdg, nil
		}
	}

	return platform, nil
}

// Example is a starting configuration, written with everything commented so
// that copying it changes nothing until a line is deliberately uncommented.
const Example = `# JingClaw configuration.
#
# Every setting here has a default; a value only needs to appear if it differs.
# Flags override this file, and JINGCLAW_-prefixed environment variables sit
# between the two.

[agent]
# What the agent calls itself. Leave empty to have it not claim a name, which
# is the honest choice when the account it speaks through is called something
# else.
# name = "JingClaw"

# Extra identity: tone, stance, what this deployment is for.
# persona = """
# You are careful and concise. You work on this team's Go services.
# """

# Standing directions for every session.
# instructions = """
# Prefer table-driven tests. Do not add dependencies without saying why.
# """

# Files read from the workspace when they exist. AGENTS.md is a convention
# several tools already share.
# instruction_files = ["AGENTS.md", "JINGCLAW.md"]

# How many model turns a single run may take before it gives up.
# max_iterations = 12

[model]
# provider = "gemini"
# model = "gemma-4-31b-it"

[workspace]
# The only directory tools can reach.
# root = "."

[server]
# Loopback only. This API can run programs; exposing it deserves more thought
# than a config line.
# addr = "127.0.0.1:0"

# Where the database lives. Empty uses the platform's own location.
# data_dir = ""
`
