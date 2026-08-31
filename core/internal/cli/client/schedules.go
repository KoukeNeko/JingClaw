package client

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
)

// newScheduleCommand is standing instructions: things the agent does without
// being asked at the time.
func newScheduleCommand() *cobra.Command {
	schedule := &cobra.Command{
		Use:   "schedule",
		Short: "Have the agent do something on a timetable",
		Long: "A schedule runs with nobody watching, so it runs under a profile that " +
			"refuses everything a person would have been asked about: it can read " +
			"and search, and it cannot write, run commands, or remember.\n\n" +
			"Creating one is a delegation, not a licence for your own permissions " +
			"to act at three in the morning.",
	}
	schedule.AddCommand(
		newScheduleAddCommand(),
		newScheduleListCommand(),
		newScheduleRemoveCommand(),
		newSchedulePauseCommand(),
	)
	return schedule
}

func newScheduleAddCommand() *cobra.Command {
	var (
		zone   string
		missed string
		here   bool
	)

	add := &cobra.Command{
		Use:   "add <expression> <prompt>",
		Short: "Add a standing instruction",
		Long: "The expression is five cron fields — minute, hour, day of month, month, " +
			"day of week — or one of @hourly, @daily, @weekly, @monthly.\n\n" +
			"Examples:\n" +
			"  jingclaw schedule add \"0 9 * * *\" \"what changed in the workspace yesterday\"\n" +
			"  jingclaw schedule add @hourly \"is anything failing\"",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			request := &controlv1.CreateScheduleRequest{
				Expression:   args[0],
				Zone:         zone,
				Prompt:       args[1],
				MissedPolicy: missed,
			}
			// Where the answer goes, said outright rather than assumed. A
			// schedule that delivered nowhere still keeps its answers in the
			// log, which is where "jingclaw session" reads them from.
			if here {
				request.Deliver = []*controlv1.DeliveryTarget{
					{Kind: "local_client", Ref: clientName},
				}
			}

			resp, err := client.CreateSchedule(cmd.Context(), connect.NewRequest(request))
			if err != nil {
				return err
			}

			one := resp.Msg.GetSchedule()
			fmt.Println(one.GetId())
			fmt.Fprintf(cmd.ErrOrStderr(), "%s in session %s%s\n",
				one.GetExpression(), one.GetSessionId(), describeNext(one))
			return nil
		},
	}

	add.Flags().StringVar(&zone, "zone", "",
		"timezone its hours are in, such as Asia/Taipei; empty uses this machine's")
	add.Flags().StringVar(&missed, "missed", "",
		"what to do about firings missed while nothing was running: "+
			"empty coalesces them into one, \"skip\" runs nothing already late")
	add.Flags().BoolVar(&here, "deliver-here", false,
		"send each answer to this client as well as to the log")

	return add
}

func newScheduleListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show standing instructions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.ListSchedules(cmd.Context(),
				connect.NewRequest(&controlv1.ListSchedulesRequest{}))
			if err != nil {
				return err
			}

			schedules := resp.Msg.GetSchedules()
			if len(schedules) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing is scheduled")
				return nil
			}

			for _, one := range schedules {
				state := describeNext(one)
				if one.GetPaused() {
					state = "  paused"
				}
				fmt.Printf("%s  %-14s %s%s\n",
					one.GetId(), one.GetExpression(), shorten(one.GetPrompt()), state)
			}
			return nil
		},
	}
}

func newScheduleRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <schedule-id>",
		Short: "Forget a standing instruction",
		Long: "The session it made is left behind: what it holds is a record of runs " +
			"that really happened, and removing a schedule is a statement about " +
			"the future.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			_, err = client.DeleteSchedule(cmd.Context(),
				connect.NewRequest(&controlv1.DeleteScheduleRequest{ScheduleId: args[0]}))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "forgot %s\n", args[0])
			return nil
		},
	}
}

func newSchedulePauseCommand() *cobra.Command {
	var resume bool

	pause := &cobra.Command{
		Use:   "pause <schedule-id>",
		Short: "Stop a standing instruction without forgetting it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.SetSchedulePaused(cmd.Context(),
				connect.NewRequest(&controlv1.SetSchedulePausedRequest{
					ScheduleId: args[0],
					Paused:     !resume,
				}))
			if err != nil {
				return err
			}

			one := resp.Msg.GetSchedule()
			if one.GetPaused() {
				fmt.Fprintf(cmd.ErrOrStderr(), "paused %s\n", one.GetId())
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "resumed %s%s\n", one.GetId(), describeNext(one))
			}
			return nil
		},
	}

	pause.Flags().BoolVar(&resume, "resume", false, "start it again")
	return pause
}

// describeNext says when a schedule next comes due, if it does.
//
// A paused one has no next time, and saying one anyway would be saying it
// will run then.
func describeNext(one *controlv1.Schedule) string {
	if one.GetNextAt() == nil {
		return ""
	}
	return "  next " + one.GetNextAt().AsTime().Local().Format(time.DateTime)
}

func shorten(text string) string {
	const most = 44

	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if len(text) <= most {
		return text
	}
	return text[:most] + "…"
}
