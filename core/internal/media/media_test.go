package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()

	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	canvas.Set(0, 0, color.RGBA{R: 1, A: 255})

	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out.Bytes()
}

func jpegBytes(t *testing.T, width, height int) []byte {
	t.Helper()

	canvas := image.NewRGBA(image.Rect(0, 0, width, height))

	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out.Bytes()
}

func TestARealImageIsAccepted(t *testing.T) {
	for declared, data := range map[string][]byte{
		"image/png":  pngBytes(t, 32, 32),
		"image/jpeg": jpegBytes(t, 32, 32),
		// The spelling a platform might use.
		"image/jpg": jpegBytes(t, 8, 8),
		// And with the parameters a sniffer adds.
		"image/png; charset=binary": pngBytes(t, 8, 8),
	} {
		mediaType, err := CheckImage(declared, data)
		if err != nil {
			t.Errorf("%s was refused: %v", declared, err)
			continue
		}
		if !strings.HasPrefix(mediaType, "image/") {
			t.Errorf("%s came back as %q", declared, mediaType)
		}
	}
}

// The declared type comes from the platform, which got it from whoever
// uploaded the file. A picture claiming to be a PNG while being something else
// is the oldest trick there is.
func TestTheLabelHasToMatchTheBytes(t *testing.T) {
	if _, err := CheckImage("image/png", jpegBytes(t, 8, 8)); err == nil {
		t.Error("a JPEG labelled as a PNG was accepted")
	}
	if _, err := CheckImage("image/png", []byte("<svg><script>alert(1)</script></svg>")); err == nil {
		t.Error("an SVG labelled as a PNG was accepted")
	}
	if _, err := CheckImage("image/png", []byte("#!/bin/sh\nrm -rf /")); err == nil {
		t.Error("a shell script labelled as a PNG was accepted")
	}
}

// Every format is a decoder, and a decoder runs before anything has decided
// whether to trust the file.
func TestOnlyThreeFormatsAreAccepted(t *testing.T) {
	for _, declared := range []string{
		"image/svg+xml",
		"image/gif",
		"image/tiff",
		"image/bmp",
		"application/pdf",
		"text/html",
		"",
	} {
		if _, err := CheckImage(declared, pngBytes(t, 8, 8)); err == nil {
			t.Errorf("%q was accepted", declared)
		}
	}
}

// hugePNGHeader is a valid PNG header declaring an enormous picture, and
// nothing after it.
//
// Built by hand rather than by encoding one: producing a real 30000x30000
// image costs several gigabytes, which would make this test the very thing it
// is checking for. The header is all DecodeConfig reads, and the header is
// where the refusal has to happen.
func hugePNGHeader(width, height uint32) []byte {
	header := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

	ihdr := make([]byte, 0, 17)
	ihdr = append(ihdr, 'I', 'H', 'D', 'R')
	ihdr = binary.BigEndian.AppendUint32(ihdr, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA, no interlace

	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)-4))
	chunk = append(chunk, ihdr...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(ihdr))

	return append(header, chunk...)
}

// A bounded number of bytes is not a bounded amount of work. A few dozen bytes
// of PNG header can promise gigabytes of pixels, and the machine that runs out
// of memory is this one.
func TestADecompressionBombIsRefusedOnItsHeader(t *testing.T) {
	bomb := hugePNGHeader(30_000, 30_000)

	if len(bomb) > MaxImageBytes {
		t.Fatalf("the bomb is %d bytes and would be caught by the byte limit alone", len(bomb))
	}

	// The bytes really do parse as a picture that size; the refusal is about
	// what it says, not about it being malformed.
	config, _, err := image.DecodeConfig(bytes.NewReader(bomb))
	if err != nil {
		t.Fatalf("the header this test builds does not parse: %v", err)
	}
	if config.Width != 30_000 || config.Height != 30_000 {
		t.Fatalf("the header parses as %dx%d", config.Width, config.Height)
	}

	if _, err := CheckImage("image/png", bomb); err == nil {
		t.Error("a small file that promises 900 million pixels was accepted")
	}

	// And one that is merely tall rather than large in total is refused too.
	if _, err := CheckImage("image/png", hugePNGHeader(4, 60_000)); err == nil {
		t.Error("a 4x60000 sliver was accepted")
	}
}

func TestSomethingTooLargeIsRefused(t *testing.T) {
	if _, err := CheckImage("image/png", make([]byte, MaxImageBytes+1)); err == nil {
		t.Error("a file past the byte limit was accepted")
	}
}

// Truncated or corrupt files must not reach a decoder that then has to cope.
func TestARuinedFileIsRefused(t *testing.T) {
	whole := pngBytes(t, 32, 32)

	if _, err := CheckImage("image/png", whole[:20]); err == nil {
		t.Error("a truncated PNG was accepted")
	}
	if _, err := CheckImage("image/png", nil); err == nil {
		t.Error("an empty file was accepted")
	}
}
