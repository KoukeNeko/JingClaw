package daemon

import (
	"flag"
	"os"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

// settingsHeldFromTheSavedText are the settings something else is deciding for
// this run: a variable in the environment, or a flag.
//
// Whatever the saved text says about them, the answer is already given. A client
// that offered to edit one would be offering a change that does nothing and
// looks as though it worked, so it is told which they are.
func settingsHeldFromTheSavedText(flags *flag.FlagSet, cfg config.Config) []control.ConfigOverride {
	held := make(map[string]control.ConfigOverride)

	for key, name := range config.EnvironmentOverrides(os.LookupEnv) {
		held[key] = control.ConfigOverride{Key: key, Source: "environment", Name: name}
	}

	// A flag wins over the environment, so it is the one that is reported.
	flags.Visit(func(passed *flag.Flag) {
		for _, key := range keysOfFlag(passed.Name, cfg) {
			held[key] = control.ConfigOverride{Key: key, Source: "flag", Name: "--" + passed.Name}
		}
	})

	out := make([]control.ConfigOverride, 0, len(held))
	for _, override := range held {
		out = append(out, override)
	}
	return out
}

// keysOfFlag names the setting a flag stands in for, and none for a flag that
// does not stand in for one.
func keysOfFlag(name string, cfg config.Config) []string {
	switch name {
	case "provider", "dev-fake":
		return []string{"provider.backend"}
	case "model":
		// Each backend names its own model, and the flag lands on the one in
		// use.
		if cfg.Provider.Backend == "fake" {
			return []string{"provider.fake_model"}
		}
		return []string{"provider." + cfg.Provider.Backend + ".model"}
	case "data-dir":
		return []string{"server.data_dir"}
	case "addr":
		return []string{"server.addr"}
	case "max-iterations":
		return []string{"agent.max_iterations"}
	}
	return nil
}
