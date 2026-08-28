package discord

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

const (
	// fetchTimeout bounds one download. A message handler that waits on
	// somebody else's CDN is a message handler that stops handling messages.
	fetchTimeout = 20 * time.Second

	// maxFetchBytes is what will be pulled down before giving up. The ingress
	// has its own, stricter limits; this one only exists so that a hostile
	// Content-Length cannot make this process the thing that fails.
	maxFetchBytes = 16 << 20

	// maxFetched bounds how many files one message can cost. Discord allows
	// ten per message and an agent does not need to look at ten.
	maxFetched = 4
)

// collectAttachments downloads what came with a message.
//
// The adapter fetches rather than passing a link inward, for two reasons. The
// link is signed for a client this daemon is not and expires, so a
// conversation replayed next month would find nothing behind it; and the
// gateway is the only part of this system that is meant to talk to the
// platform at all.
//
// What cannot be fetched is still reported, without its bytes. A message whose
// text only makes sense alongside a picture should say a picture was sent,
// rather than arriving as a sentence about nothing.
func (a *Adapter) collectAttachments(
	ctx context.Context,
	attachments []discord.Attachment,
) []jcgateway.Attachment {
	if len(attachments) == 0 {
		return nil
	}

	collected := make([]jcgateway.Attachment, 0, len(attachments))
	fetched := 0

	for _, attachment := range attachments {
		record := jcgateway.Attachment{
			ID:          attachment.ID.String(),
			Name:        attachment.Filename,
			ContentType: contentTypeOf(attachment),
			Size:        int64(attachment.Size),
		}

		switch {
		case fetched >= maxFetched:
			a.config.Logger.Info("not fetching any more files from one message",
				"name", attachment.Filename, "limit", maxFetched)

		case !looksFetchable(record):
			// Filtered here as well as at the ingress, so that a video nobody
			// is going to look at is not downloaded first and rejected after.
			a.config.Logger.Debug("not fetching a file this agent does not read",
				"name", attachment.Filename, "content_type", record.ContentType)

		case int64(attachment.Size) > maxFetchBytes:
			a.config.Logger.Info("not fetching a file this large",
				"name", attachment.Filename, "size", attachment.Size)

		default:
			data, err := a.fetch(ctx, attachment.URL)
			if err != nil {
				a.config.Logger.Warn("could not fetch a file",
					"name", attachment.Filename, "error", err)
				break
			}
			record.Data = data
			record.Size = int64(len(data))
			fetched++
		}

		collected = append(collected, record)
	}

	return collected
}

func (a *Adapter) fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord: fetching the file gave %s", response.Status)
	}

	// Bounded by what is read, not by what the response claims: a Content-Length
	// is a promise from somebody else's server.
	data, err := io.ReadAll(io.LimitReader(response.Body, maxFetchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFetchBytes {
		return nil, fmt.Errorf("discord: the file is larger than the %d bytes allowed", maxFetchBytes)
	}

	return data, nil
}

// contentTypeOf is what Discord says the file is, which is what whoever
// uploaded it said. It is a hint for deciding whether to spend a download on
// it, and nothing downstream believes it.
func contentTypeOf(attachment discord.Attachment) string {
	if attachment.ContentType != nil {
		return *attachment.ContentType
	}
	return ""
}

// looksFetchable is the cheap filter that runs before the download.
func looksFetchable(attachment jcgateway.Attachment) bool {
	return strings.HasPrefix(strings.ToLower(attachment.ContentType), "image/")
}
