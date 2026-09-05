package discord

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// canned answers the two questions from fixed messages.
type canned struct {
	byID    map[snowflake.ID]*discord.Message
	history []discord.Message
	err     error
}

func (c *canned) GetMessage(_, id snowflake.ID, _ ...rest.RequestOpt) (*discord.Message, error) {
	if c.err != nil {
		return nil, c.err
	}
	if m, ok := c.byID[id]; ok {
		return m, nil
	}
	return nil, errors.New("no such message")
}

func (c *canned) GetMessages(_, _, _, _ snowflake.ID, _ int, _ ...rest.RequestOpt) ([]discord.Message, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.history, nil
}

const (
	botUser   snowflake.ID = 1
	askerUser snowflake.ID = 2
	otherUser snowflake.ID = 3
	guildID   snowflake.ID = 100
	channelID snowflake.ID = 200
)

var now = time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC)

func withContext(source messageSource) *Adapter {
	a := &Adapter{config: Config{Logger: slog.New(slog.DiscardHandler)}, messages: source}
	a.selfID.Store(uint64(botUser))
	return a
}

func picture(id snowflake.ID) discord.Attachment {
	return discord.Attachment{ID: id, Filename: "shot.png", ContentType: ptr("image/png"), Size: 1000}
}

func ptr[T any](v T) *T { return &v }

// msg builds a message; mentionBot addresses it to the agent.
func msg(id, author snowflake.ID, at time.Time, mentionBot bool, files ...discord.Attachment) discord.Message {
	m := discord.Message{
		ID: id, ChannelID: channelID, Author: discord.User{ID: author}, CreatedAt: at, Attachments: files,
	}
	if mentionBot {
		m.Mentions = []discord.User{{ID: botUser}}
	}
	return m
}

// ask is the message that arrived: a mention, at now.
func ask(files ...discord.Attachment) discord.Message {
	return msg(50, askerUser, now, true, files...)
}

func ids(attachments []discord.Attachment) []snowflake.ID {
	out := make([]snowflake.ID, 0, len(attachments))
	for _, a := range attachments {
		out = append(out, a.ID)
	}
	return out
}

func expect(t *testing.T, got []discord.Attachment, want ...snowflake.ID) {
	t.Helper()
	have := ids(got)
	if len(have) != len(want) {
		t.Fatalf("got %v, want %v", have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Fatalf("got %v, want %v", have, want)
		}
	}
}

// Replying to a picture and asking about it is the request, whoever posted the
// picture.
func TestAReplyBringsThePictureItPointsAt(t *testing.T) {
	theirs := msg(40, otherUser, now.Add(-time.Minute), false, picture(400))
	source := &canned{byID: map[snowflake.ID]*discord.Message{40: &theirs}}

	request := ask()
	request.MessageReference = &discord.MessageReference{MessageID: ptr(snowflake.ID(40))}

	expect(t, withContext(source).attachmentsFor(context.Background(), request, ptr(guildID)), 400)
}

// A screenshot posted as one message and the question as the next: the
// screenshot was never delivered, so it comes with the question.
func TestThePersonsOwnEarlierPictureComesWithTheQuestion(t *testing.T) {
	source := &canned{history: []discord.Message{
		msg(49, askerUser, now.Add(-30*time.Second), false, picture(490)),
	}}

	expect(t, withContext(source).attachmentsFor(context.Background(), ask(picture(500)), ptr(guildID)), 500, 490)
}

// What is deliberately not taken.
func TestWhatIsNotPartOfTheRequest(t *testing.T) {
	cases := []struct {
		name    string
		history []discord.Message
	}{
		{
			// Somebody's picture in a busy channelID is not part of a
			// stranger's request just because it was near it.
			name:    "another person's picture",
			history: []discord.Message{msg(49, otherUser, now.Add(-time.Minute), false, picture(490))},
		},
		{
			// Delivered when it arrived; its files came with it then, and
			// sending them again would show the model the same thing twice.
			name:    "a picture that already addressed the agent",
			history: []discord.Message{msg(49, askerUser, now.Add(-time.Minute), true, picture(490))},
		},
		{
			// Ten minutes is a conversation; an hour is a different one.
			name:    "a picture from too long ago",
			history: []discord.Message{msg(49, askerUser, now.Add(-time.Hour), false, picture(490))},
		},
		{
			// The agent's own reply ends the previous exchange. A picture
			// from before it is not what "this one" means.
			name: "a picture from before the agent's last reply",
			history: []discord.Message{
				msg(49, botUser, now.Add(-time.Minute), false),
				msg(48, askerUser, now.Add(-2*time.Minute), false, picture(480)),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withContext(&canned{history: tc.history}).attachmentsFor(context.Background(), ask(), ptr(guildID))
			if len(got) != 0 {
				t.Fatalf("took %v, want nothing", ids(got))
			}
		})
	}
}

// Several earlier pictures come in the order they were posted, after the
// question's own, and the same file is never taken twice.
func TestOrderAndNoDuplicates(t *testing.T) {
	shared := picture(490)
	replied := msg(40, otherUser, now.Add(-time.Minute), false, shared, picture(401))
	source := &canned{
		byID: map[snowflake.ID]*discord.Message{40: &replied},
		history: []discord.Message{
			msg(49, askerUser, now.Add(-20*time.Second), false, shared),
			msg(48, askerUser, now.Add(-40*time.Second), false, picture(480)),
		},
	}
	request := ask(picture(500))
	request.MessageReference = &discord.MessageReference{MessageID: ptr(snowflake.ID(40))}

	expect(t, withContext(source).attachmentsFor(context.Background(), request, ptr(guildID)), 500, 490, 401, 480)
}

// The platform not answering leaves the request with what it had, rather
// than losing the file that did arrive.
func TestThePlatformNotAnsweringLeavesTheOwnFiles(t *testing.T) {
	source := &canned{err: errors.New("rate limited")}
	request := ask(picture(500))
	request.MessageReference = &discord.MessageReference{MessageID: ptr(snowflake.ID(40))}

	expect(t, withContext(source).attachmentsFor(context.Background(), request, ptr(guildID)), 500)
}

// Before the connection exists there is nothing to ask, and that is not an
// error either.
func TestWithNothingToAskOnlyTheOwnFilesAreTaken(t *testing.T) {
	a := &Adapter{config: Config{Logger: slog.New(slog.DiscardHandler)}}
	expect(t, a.attachmentsFor(context.Background(), ask(picture(500)), ptr(guildID)), 500)
}
