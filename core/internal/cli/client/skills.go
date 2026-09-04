package client

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
)

// newSkillsCommand installs and lists skills.
//
// Local: skills are files in the deployment directory, and the daemon reads
// them when it starts. Going through the control plane would mean asking a
// background process to clone a repository somebody typed at a terminal,
// which puts the fetching further from the person who asked for it and no
// closer to anything that needs it.
func newSkillsCommand() *cobra.Command {
	skills := &cobra.Command{
		Use:   "skills",
		Short: "Install instructions the agent can read",
		Long: "A skill is a note somebody wrote about how to do something here. It " +
			"grants nothing: anything it tells the agent to do goes through the " +
			"same checks and the same approvals as if the agent had thought of it " +
			"itself.\n\n" +
			"The daemon reads them at startup, so restart it after installing one.",
	}
	skills.AddCommand(
		newSkillsInstallCommand(),
		newSkillsListCommand(),
		newSkillsStagedCommand(),
		newSkillsRemoveCommand(),
	)
	return skills
}

func newSkillsInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "install <source>",
		Short: "Fetch a skill from a git repository",
		Long: "The source names a repository, an exact commit, and a path inside it:\n\n" +
			"  jingclaw skills install git:https://github.com/someone/skills#<commit>:release\n\n" +
			"A commit rather than a branch or a tag, because those are names " +
			"somebody else can repoint — and what is being installed is text that " +
			"goes in front of the model asking it to do things. What arrives is " +
			"recorded with its hash in " + skill.LockName + ", which is the only " +
			"place that says which instructions were chosen and which were " +
			"written by somebody else.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			source, err := skill.ParseSource(args[0])
			if err != nil {
				return err
			}

			root, err := skillsDir()
			if err != nil {
				return err
			}

			installer := &skill.Installer{Root: root}
			locked, err := installer.Install(cmd.Context(), source)
			if err != nil {
				return err
			}

			lock, err := skill.ReadLock(root)
			if err != nil {
				return err
			}
			if err := skill.WriteLock(root, lock.Record(locked)); err != nil {
				return err
			}

			fmt.Println(locked.Name)
			fmt.Fprintf(cmd.ErrOrStderr(),
				"installed %s (%s)\nrestart the daemon for it to be offered\n",
				locked.Name, locked.Digest[:21])
			return nil
		},
	}
}

func newSkillsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show installed skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := skillsDir()
			if err != nil {
				return err
			}

			installed, rejected, err := skill.Installed(root)
			if err != nil {
				return err
			}
			lock, err := skill.ReadLock(root)
			if err != nil {
				return err
			}

			if len(installed) == 0 && len(rejected) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no skills are installed")
				return nil
			}

			sort.Slice(installed, func(a, b int) bool {
				return installed[a].Name < installed[b].Name
			})
			for _, one := range installed {
				fmt.Printf("%-20s %s\n", one.Name, describeSource(lock, one.Name))
			}

			// A skill that will not load is said about rather than left out.
			// One that silently does not appear is an afternoon somebody
			// spends, and the reason is always in the file.
			for _, one := range rejected {
				fmt.Fprintf(cmd.ErrOrStderr(), "%-20s cannot be read: %s\n", one.Name, one.Reason)
			}

			// And what is on disk is not what was installed, which is worth
			// saying because what is on disk is what the model reads.
			for _, changed := range skill.Changed(root, lock, installed) {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", changed)
			}
			return nil
		},
	}
}

// newSkillsStagedCommand shows what the agent has fetched and is waiting for
// somebody to approve.
//
// The operator's window onto a proposal before it becomes standing
// instructions: what it calls itself, where it came from, and the exact
// commit — enough to go and read the source before approving the activation.
func newSkillsStagedCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "staged",
		Short: "Show skills fetched but not yet installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := skillsDir()
			if err != nil {
				return err
			}

			staged, err := skill.StagedSkills(root)
			if err != nil {
				return err
			}
			if len(staged) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing is staged")
				return nil
			}

			for _, one := range staged {
				fmt.Printf("%-20s %s#%s\n", one.Name, one.Source.Repository, one.Source.Commit)
			}
			return nil
		},
	}
}

func newSkillsRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete an installed skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := skillsDir()
			if err != nil {
				return err
			}

			installer := &skill.Installer{Root: root}
			if err := installer.Remove(args[0]); err != nil {
				return err
			}

			lock, err := skill.ReadLock(root)
			if err != nil {
				return err
			}
			if lock, forgotten := lock.Forget(args[0]); forgotten {
				if err := skill.WriteLock(root, lock); err != nil {
					return err
				}
			}

			fmt.Fprintf(cmd.ErrOrStderr(),
				"removed %s\nrestart the daemon to stop offering it\n", args[0])
			return nil
		},
	}
}

// describeSource is where a skill came from, for a listing.
//
// A skill nobody installed is said to be placed by hand rather than left
// blank: that is a real difference — one was chosen from somewhere and one
// was written here — and a blank column reads as missing information.
func describeSource(lock skill.Lock, name string) string {
	entry, installed := lock.Entry(name)
	if !installed {
		return "placed by hand"
	}

	said := entry.From.Repository
	if entry.From.Path != "" {
		said += " " + entry.From.Path
	}
	return said + " @ " + entry.From.Commit[:min(12, len(entry.From.Commit))]
}

// skillsDir is where this deployment keeps them.
func skillsDir() (string, error) {
	dir, found := home.Resolve()
	if !found {
		return "", fmt.Errorf(
			"there is no deployment directory here; start the daemon once to make one")
	}
	return dir.Skills(), nil
}
