// Command jingclaw is the whole thing.
//
// Run with no arguments it starts everything and gives you the console, the
// way a game server does: one file, you open it, it is running. The
// subcommands are for the cases that are not that — being one of the daemons
// it starts, asking a running one a question from a script, or installing it
// as a service so it survives the terminal closing.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/client"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/console"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/daemon"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/service"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/supervise"
)

func main() {
	// Before anything else, because this process may not be the program at
	// all: it may be one started to confine a command and then become it.
	confineIfAsked()

	// And this process may be the smaller thing left behind to notice that
	// the program is gone, which is not the program either.
	supervise.WatchIfAsked()

	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jingclaw",
		Short: "A local agent that answers in chat",
		Long: "Run with no arguments to start everything and watch it.\n" +
			"Everything else is for scripts, or for being one of the parts.",
		SilenceUsage:  true,
		SilenceErrors: true,

		// No arguments is the whole product. Requiring a word before anything
		// happens would turn a decision with one answer into a choice.
		RunE: func(command *cobra.Command, _ []string) error {
			return supervise.Run(command.Context())
		},
	}

	// The two long-running parts. Named rather than hidden: something has to
	// start them, and a process somebody finds in ps should be something they
	// can also run by hand to see what it says.
	cmd.AddCommand(&cobra.Command{
		Use:                "daemon",
		Short:              "Run the agent daemon in the foreground",
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return daemon.Main(args)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:                "gateway",
		Short:              "Run the chat gateway in the foreground",
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return gateway.Main(args)
		},
	})

	cmd.AddCommand(supervise.Commands()...)
	cmd.AddCommand(service.Commands()...)
	cmd.AddCommand(console.Commands()...)

	// And everything a client can ask a running daemon, which is most of the
	// subcommands there are.
	client.AddTo(cmd)

	return cmd
}
