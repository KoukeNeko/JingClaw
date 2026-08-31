// Package panel is the command that opens the terminal panel.
//
// Its own package rather than a command inside internal/tui, so the drawing
// stays testable without cobra and the wiring stays in the layer that already
// knows how to find a running daemon.
package panel

import (
	"github.com/spf13/cobra"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/client"
	"github.com/KoukeNeko/JingClaw/core/internal/tui"
)

// Commands is the panel command.
func Commands() []*cobra.Command {
	var runtimeDir string

	command := &cobra.Command{
		Use:   "panel",
		Short: "Watch a session, and decide what is waiting",
		Long: "Watch a session, and decide what is waiting.\n\n" +
			"A full-screen view of one session at a time. It reads and\n" +
			"decides; it does not send turns. Leaving the panel does not\n" +
			"stop anything.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			daemon, err := client.Dial(runtimeDir)
			if err != nil {
				return err
			}
			return tui.Run(cmd.Context(), tui.Options{Sessions: tui.Over(daemon)})
		},
	}
	command.Flags().StringVar(&runtimeDir, "runtime-dir", "",
		"where the daemon publishes itself; defaults to this deployment's")

	return []*cobra.Command{command}
}
