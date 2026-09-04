package discord

import (
	"bytes"
	"fmt"
	"strings"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render/tableimage"
)

// tableFileName is what a drawn table is called when it is uploaded.
const tableFileName = "table.png"

// fonts is loaded once and kept, because opening a twenty-megabyte typeface
// for every table would be paying the price of the feature per message rather
// than per process.
//
// A machine without one is remembered too. Failing to find a font is not a
// transient condition, and probing nine paths on every answer that has a
// table in it would be nine failures per answer forever.
var (
	fontsOnce sync.Once
	loaded    tableimage.Fonts
	fontsErr  error
)

func sharedFonts() (tableimage.Fonts, error) {
	fontsOnce.Do(func() { loaded, fontsErr = tableimage.Load(nil) })
	return loaded, fontsErr
}

// tableFonts is the typeface this adapter draws with.
//
// Indirect so that a check can be a machine with no such font. That is the
// failure this feature has to survive — an answer must still arrive, written
// out rather than drawn — and a case only reachable on a machine that happens
// to lack a font is a case nothing ever checks.
func (a *Adapter) tableFonts() (tableimage.Fonts, error) {
	if a.fonts != nil {
		return a.fonts()
	}
	return sharedFonts()
}

// postWithTablesDrawn sends an answer whose tables become pictures.
//
// One message per piece, in the order they were written. A picture cannot sit
// inside a message beside text on this platform — an attachment is always
// below the content — so an answer with a table in the middle is three
// messages rather than one, and their order is the answer's order.
func (a *Adapter) postWithTablesDrawn(
	channelID snowflake.ID, dispatch jcgateway.Dispatch,
) ([]string, bool, error) {
	segments, err := render.DispatchSegments(dispatch, discordStyle)
	if err != nil {
		return nil, false, err
	}

	drawable := false
	for _, segment := range segments {
		if segment.Kind == render.SegmentTable {
			drawable = true
			break
		}
	}
	if !drawable {
		// Nothing to draw, so nothing this path does differently. Said rather
		// than done, so the caller takes the ordinary route and every answer
		// without a table stays byte for byte what it was.
		return nil, false, nil
	}

	fonts, err := a.tableFonts()
	if err != nil {
		// No typeface that can draw the text. The answer still has to arrive,
		// so it arrives the way it did before this feature existed.
		a.config.Logger.Warn("drawing tables is on and there is no font for it; writing them out instead",
			"error", err)
		return nil, false, nil
	}

	// The answer may have been growing in one message while it was written.
	// That message is finished in place with what came before the first
	// table — the way finishAsMessages does — rather than left standing with
	// the whole answer, table written out, beside the same answer posted
	// again in pieces. Released at the end either way, so a later answer
	// cannot extend it by mistake.
	answer := answerOf(dispatch)
	defer a.clearAnswer(answer)

	reuse, growing := a.liveAnswer(answer)
	if growing && !opensWithText(segments) {
		// Nothing at the front to finish it into: the answer opens with a
		// table, which the growing message holds written out. Taken down
		// rather than left beside the picture of itself.
		if err := a.client.Rest.DeleteMessage(channelID, reuse); err != nil {
			a.config.Logger.Debug("could not take down the growing answer",
				"message_id", answer, "error", err)
		}
		growing = false
	}

	var posted []string
	for index, segment := range segments {
		if segment.Kind == render.SegmentText {
			// Only the first piece of the answer takes over the growing
			// message; everything after it is new.
			var into snowflake.ID
			if growing && index == 0 {
				into = reuse
			}
			ids, err := a.postTextSegment(channelID, dispatch, segment.Text, into)
			if err != nil {
				return posted, true, err
			}
			posted = append(posted, ids...)
			continue
		}

		drawn, err := tableimage.Draw(tableimage.Table{
			Header: segment.Table.Header,
			Rows:   segment.Table.Rows,
		}, fonts)
		if err != nil {
			// One table that would not draw. The rest of the answer is
			// already posted, so this piece goes out written rather than
			// leaving a gap where a table was.
			a.config.Logger.Warn("could not draw a table; writing it out instead", "error", err)
			ids, err := a.postTextSegment(channelID, dispatch, render.TableText(segment.Table, discordStyle), 0)
			if err != nil {
				return posted, true, err
			}
			posted = append(posted, ids...)
			continue
		}

		message, err := a.client.Rest.CreateMessage(channelID, discord.MessageCreate{
			AllowedMentions: &discord.AllowedMentions{},
			Files: []*discord.File{{
				Name:   tableFileName,
				Reader: bytes.NewReader(drawn),
			}},
		})
		if err != nil {
			return posted, true, fmt.Errorf("discord: post a drawn table: %w", err)
		}
		posted = append(posted, message.ID.String())
	}

	return posted, true, nil
}

// postTextSegment sends one piece of prose, split if the platform needs it.
// postTextSegment sends one piece of prose, split if the platform needs it.
//
// into names a message already growing with this answer, or is zero. The
// first piece finishes that message in place and the rest are new — the same
// shape as finishAsMessages, so an answer that streamed and then turned out
// to hold a table ends up in the same messages as one that did not.
func (a *Adapter) postTextSegment(
	channelID snowflake.ID, dispatch jcgateway.Dispatch, text string, into snowflake.ID,
) ([]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}

	var posted []string
	for index, piece := range render.Split(text, discordStyle) {
		if index == 0 && into != 0 {
			_, err := a.client.Rest.UpdateMessage(channelID, into, discord.MessageUpdate{
				Content:         &piece,
				AllowedMentions: &discord.AllowedMentions{},
			})
			if err == nil {
				posted = append(posted, into.String())
				continue
			}
			a.config.Logger.Debug("could not finish the answer in place",
				"message_id", into, "error", err)
		}

		message, err := a.client.Rest.CreateMessage(channelID, messageWith(piece))
		if err != nil {
			return posted, fmt.Errorf("discord: post part of an answer: %w", err)
		}
		posted = append(posted, message.ID.String())
	}
	return posted, nil
}

// opensWithText reports whether an answer's first piece is prose a growing
// message can be finished into.
func opensWithText(segments []render.Segment) bool {
	return len(segments) > 0 &&
		segments[0].Kind == render.SegmentText &&
		strings.TrimSpace(segments[0].Text) != ""
}
