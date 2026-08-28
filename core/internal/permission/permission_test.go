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

	for _, profile := range []permission.Profile{permission.LocalProfile(), permission.GatewayProfile()} {
		for _, level := range levels {
			if _, ok := profile.Defaults[level]; !ok {
				t.Errorf("the %s profile has no rule for %s tools", profile.Name, level)
			}
		}
	}
}
