package permission_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Reading a page runs unattended for the operator and stops for a chat
// channel. The difference is who chose the address: the operator naming a page
// is research, and a stranger naming one is a link handed to a process that
// can read this machine's files.
func TestNetworkReadsDependOnWhoChoseTheAddress(t *testing.T) {
	tests := []struct {
		profile permission.Profile
		want    permission.Decision
	}{
		{permission.LocalProfile(), permission.Allow},
		{permission.GatewayProfile(), permission.Ask},
	}

	for _, test := range tests {
		t.Run(test.profile.Name, func(t *testing.T) {
			engine := permission.New(test.profile)

			outcome := engine.Evaluate(context.Background(), permission.Request{
				Spec: tool.Spec{
					Name:         "web_read",
					Level:        tool.LevelNetworkRead,
					Capabilities: tool.Capabilities{Network: true},
				},
				Call: tool.Call{
					Name:      "web_read",
					Arguments: json.RawMessage(`{"url":"https://example.com/"}`),
				},
			})

			if outcome.Decision != test.want {
				t.Errorf("the %s profile decided %s, want %s (%s)",
					test.profile.Name, outcome.Decision, test.want, outcome.Reason)
			}
		})
	}
}

// Every profile has to have an answer for every level. A level nobody wrote a
// rule for is denied, which is the right failure but a poor way to discover
// that a tool has shipped unusable.
func TestEveryProfileRulesOnEveryLevel(t *testing.T) {
	levels := []tool.Level{
		tool.LevelInternal, tool.LevelWorkspaceRead, tool.LevelNetworkRead,
		tool.LevelWorkspaceWrite, tool.LevelRemember, tool.LevelExecute,
		tool.LevelHighImpact,
	}

	for _, profile := range []permission.Profile{
		permission.LocalProfile(), permission.GatewayProfile(), permission.ConsoleProfile(),
	} {
		for _, level := range levels {
			if _, ok := profile.Defaults[level]; !ok {
				t.Errorf("the %s profile has no rule for %s tools", profile.Name, level)
			}
		}
	}
}

// A console channel is a third trust level, not a relabelled gateway. It may
// read, write and remember; it may not run programs.
//
// The line is where it is because of what a channel permission can and cannot
// protect. It settles who is in the room, which makes reading and writing
// reasonable there. It cannot settle whether an account still belongs to its
// owner, and a stolen one holds the request and the approval both — so running
// programs stays where somebody has to be present.
func TestAConsoleChannelSitsBetweenTheOtherTwo(t *testing.T) {
	local := permission.LocalProfile().Defaults
	console := permission.ConsoleProfile().Defaults
	channel := permission.GatewayProfile().Defaults

	// Wider than an ordinary channel: a private room does not stop to ask
	// before reading a page.
	if console[tool.LevelNetworkRead] != permission.Allow {
		t.Errorf("a console asks before reading the web: %s", console[tool.LevelNetworkRead])
	}
	if channel[tool.LevelNetworkRead] != permission.Ask {
		t.Errorf("an ordinary channel no longer asks before reading the web")
	}

	// Narrower than the machine: this is the whole point.
	if console[tool.LevelExecute] != permission.Deny {
		t.Errorf("a console can run programs, which needs somebody at the machine: %s",
			console[tool.LevelExecute])
	}
	if local[tool.LevelExecute] != permission.Ask {
		t.Errorf("the local profile no longer asks before running programs")
	}

	// Changes still stop, and a person in the channel can answer.
	for _, level := range []tool.Level{tool.LevelWorkspaceWrite, tool.LevelRemember} {
		if console[level] != permission.Ask {
			t.Errorf("a console does not stop for %s: %s", level, console[level])
		}
	}
	if console[tool.LevelHighImpact] != permission.Deny {
		t.Errorf("a console permits high-impact work: %s", console[tool.LevelHighImpact])
	}
}

// Naming it must actually select it. A binding that asks for a profile the
// engine has never heard of is refused rather than quietly served the
// permissive one.
func TestTheConsoleProfileResolvesByName(t *testing.T) {
	profile, ok := permission.ProfileByName("console")
	if !ok {
		t.Fatal("a binding naming the console profile cannot resolve it")
	}
	if profile.Name != "console" {
		t.Errorf("resolved to %q", profile.Name)
	}

	// Available on an engine built for local sessions, so a channel bound to
	// it cannot fall back to the machine's own permissions.
	engine := permission.New(permission.LocalProfile())
	if err := engine.UseProfile("ses_1", "console"); err != nil {
		t.Fatalf("a session could not be bound to the console profile: %v", err)
	}
	if err := engine.UseProfile("ses_2", "consoel"); err == nil {
		t.Error("a misspelled profile was accepted")
	}
}
