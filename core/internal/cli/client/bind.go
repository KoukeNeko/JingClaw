package client

import (
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
)

// Bindings are the operator's statement that a channel may reach a workspace.
// Nothing is inferred from a channel's name, so this is the only way one gets
// created.

func newBindingsCommand() *cobra.Command {
	bindings := &cobra.Command{Use: "bindings", Short: "Manage which channels may reach a workspace"}
	bindings.AddCommand(newBindingsListCommand(), newBindCommand(), newUnbindCommand())
	return bindings
}

func newBindingsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show every channel binding",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialChannels()
			if err != nil {
				return err
			}

			resp, err := client.ListBindings(cmd.Context(),
				connect.NewRequest(&controlv1.ListBindingsRequest{}))
			if err != nil {
				return err
			}

			if len(resp.Msg.GetBindings()) == 0 {
				fmt.Fprintln(os.Stderr, "no bindings; no channel can reach a workspace")
				return nil
			}

			for _, binding := range resp.Msg.GetBindings() {
				fmt.Printf("%s  %s/%s channel=%s  workspace=%s  profile=%s\n",
					binding.GetId(), binding.GetPlatform(), binding.GetAccountId(),
					binding.GetChannelId(), binding.GetWorkspaceId(), binding.GetPermissionProfile())

				if principals := binding.GetAllowedPrincipals(); len(principals) > 0 {
					fmt.Printf("    may trigger: %s\n", strings.Join(principals, ", "))
				} else {
					fmt.Printf("    may trigger: nobody\n")
				}
			}
			return nil
		},
	}
}

func newBindCommand() *cobra.Command {
	var (
		platform  string
		account   string
		guild     string
		channel   string
		workspace string
		profile   string
		users     []string
		roles     []string
	)

	cmd := &cobra.Command{
		Use:   "add",
		Short: "Let a channel reach a workspace",
		Long: "Let a channel reach a workspace.\n\n" +
			"A binding with nobody allowed permits nobody: pass --user or --role, " +
			"using the platform's own identifiers rather than display names.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialChannels()
			if err != nil {
				return err
			}

			claims := make([]*controlv1.PrincipalClaim, 0, len(roles))
			for _, role := range roles {
				claims = append(claims, &controlv1.PrincipalClaim{
					Namespace: platform + ".role",
					Value:     role,
				})
			}

			resp, err := client.UpsertBinding(cmd.Context(),
				connect.NewRequest(&controlv1.UpsertBindingRequest{
					Meta: newMeta(),
					Binding: &controlv1.Binding{
						Platform:          platform,
						AccountId:         account,
						TenantId:          guild,
						ChannelId:         channel,
						WorkspaceId:       workspace,
						PermissionProfile: profile,
						AllowedPrincipals: users,
						AllowedClaims:     claims,
					},
				}))
			if err != nil {
				return err
			}

			fmt.Println(resp.Msg.GetBinding().GetId())
			if len(users) == 0 && len(roles) == 0 {
				fmt.Fprintln(os.Stderr,
					"warning: this binding allows nobody; add --user or --role before it can be used")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&platform, "platform", "discord", "messaging platform")
	cmd.Flags().StringVar(&account, "account", "main", "bot account name within JingClaw")
	cmd.Flags().StringVar(&guild, "guild", "", "server (guild) id; empty for direct messages")
	cmd.Flags().StringVar(&channel, "channel", "", "channel id")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace this channel may reach")
	cmd.Flags().StringVar(&profile, "profile", "gateway", "permission profile for runs from here: gateway or console")
	cmd.Flags().StringSliceVar(&users, "user", nil, "platform user id allowed to trigger work")
	cmd.Flags().StringSliceVar(&roles, "role", nil, "platform role id allowed to trigger work")

	_ = cmd.MarkFlagRequired("channel")
	_ = cmd.MarkFlagRequired("workspace")

	return cmd
}

func newUnbindCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <binding-id>",
		Short: "Stop a channel reaching a workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialChannels()
			if err != nil {
				return err
			}

			_, err = client.DeleteBinding(cmd.Context(),
				connect.NewRequest(&controlv1.DeleteBindingRequest{
					Meta:      newMeta(),
					BindingId: args[0],
				}))
			return err
		},
	}
}
