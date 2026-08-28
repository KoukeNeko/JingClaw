// Package media decides whether a file is what it says it is.
//
// It sits on its own because the question is the same wherever a file comes
// from. A screenshot pasted into a chat and one attached by the operator at
// their own terminal are both bytes somebody else produced, and a control
// client being trusted to make requests is not the same as its files being
// safe to decode.
package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"net/http"
	"strings"

	// Registered for their DecodeConfig, which reads the header rather than
	// the picture: the dimensions have to be known before anything decides
	// whether decoding the whole thing is safe.
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// What an image may be, and how big.
//
// The allowlist is short on purpose. Every format is a decoder, and a decoder
// is an attack surface that runs before anything has decided whether to trust
// the file. SVG is a document that can carry script and is not here. Animated
// GIF is a sequence of frames pretending to be a picture and is not here
// either.
const (
	MaxImageBytes  = 8 << 20
	MaxImagePixels = 40_000_000
	MaxImageSide   = 12_000

	// MaxImagesPerMessage is how many one message may carry. A model does not
	// need ten pictures to answer a question, and each of them is paid for
	// again on every turn it stays in the conversation.
	MaxImagesPerMessage = 4
)

var acceptedImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

var ErrNotAnImage = errors.New("gateway: not an image this agent accepts")

// CheckImage decides whether a file that arrived may be kept and shown.
//
// The declared type is not believed. It comes from the platform, which got it
// from whoever uploaded the file, and a picture that says it is a PNG while
// being something else is the oldest trick there is. The bytes have to agree
// with the label, and the label has to be on the list.
//
// The size limits are two, not one. A bounded number of bytes is not a bounded
// amount of work: a few megabytes of PNG can decode to gigabytes of pixels,
// and the machine that runs out of memory is this one.
func CheckImage(declared string, data []byte) (mediaType string, err error) {
	declared = CanonicalMediaType(declared)
	if !acceptedImageTypes[declared] {
		return "", fmt.Errorf("%w: %s", ErrNotAnImage, declared)
	}
	if len(data) > MaxImageBytes {
		return "", fmt.Errorf("gateway: an image may be %d bytes and this is %d",
			MaxImageBytes, len(data))
	}

	sniffed := CanonicalMediaType(http.DetectContentType(data))
	if sniffed != declared {
		return "", fmt.Errorf("%w: labelled %s and is %s", ErrNotAnImage, declared, sniffed)
	}

	// Header only. Deciding whether it is safe to decode by decoding it would
	// be deciding after the fact.
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: its header does not parse: %v", ErrNotAnImage, err)
	}
	if formatMediaType(format) != declared {
		return "", fmt.Errorf("%w: labelled %s and decodes as %s",
			ErrNotAnImage, declared, format)
	}

	if config.Width > MaxImageSide || config.Height > MaxImageSide {
		return "", fmt.Errorf("gateway: %dx%d is past the %d a side allowed",
			config.Width, config.Height, MaxImageSide)
	}
	if config.Width*config.Height > MaxImagePixels {
		return "", fmt.Errorf("gateway: %dx%d is %d pixels, past the %d allowed",
			config.Width, config.Height, config.Width*config.Height, MaxImagePixels)
	}

	return declared, nil
}

// CanonicalMediaType drops the parameters a sniffer adds and the spellings a
// platform might use.
func CanonicalMediaType(declared string) string {
	if index := strings.IndexByte(declared, ';'); index >= 0 {
		declared = declared[:index]
	}
	declared = strings.ToLower(strings.TrimSpace(declared))

	if declared == "image/jpg" {
		return "image/jpeg"
	}
	return declared
}

// formatMediaType maps what the image package calls a format to what everybody
// else calls it.
func formatMediaType(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	default:
		return "image/" + format
	}
}
