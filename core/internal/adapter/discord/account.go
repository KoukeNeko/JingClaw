package discord

import (
	"fmt"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// accountKeptFor bounds how long the line under an answer is remembered. What
// is added to it arrives within seconds of it; an entry older than this is a
// run nothing more will be said about.
const accountKeptFor = 15 * time.Minute

// account is the line posted under an answer when its run ended, kept so
// that something learned after the run can be added to it rather than posted
// as a second line.
//
// In memory, like the working line: losing it across a restart costs one
// extra line in a channel, not a wrong one.
type account struct {
	message snowflake.ID
	text    string
	at      time.Time
}

func (a *Adapter) rememberAccount(run domain.RunID, message snowflake.ID, text string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	now := time.Now()
	for id, kept := range a.accounts {
		if now.Sub(kept.at) > accountKeptFor {
			delete(a.accounts, id)
		}
	}
	a.accounts[run] = account{message: message, text: text, at: now}
}

func (a *Adapter) takeAccount(run domain.RunID) (account, bool) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	kept, ok := a.accounts[run]
	delete(a.accounts, run)
	return kept, ok
}

// addToTheAccount puts a line learned after the run onto the line under its
// answer, or under the answer on its own when that line is not known here.
func (a *Adapter) addToTheAccount(
	channelID snowflake.ID,
	dispatch jcgateway.Dispatch,
	body string,
) ([]string, error) {
	kept, ok := a.takeAccount(dispatch.RunID)
	if !ok {
		message, err := a.client.Rest.CreateMessage(channelID, messageWith(body))
		if err != nil {
			return nil, fmt.Errorf("discord: post what was noted to %s: %w", channelID, err)
		}
		return []string{message.ID.String()}, nil
	}

	content := kept.text + "\n" + body
	if _, err := a.client.Rest.UpdateMessage(channelID, kept.message, discord.MessageUpdate{
		Content:         &content,
		AllowedMentions: &discord.AllowedMentions{},
	}); err != nil {
		return nil, fmt.Errorf("discord: add to the line under the answer: %w", err)
	}
	return nil, nil
}
