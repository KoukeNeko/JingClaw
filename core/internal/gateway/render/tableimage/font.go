// Package tableimage draws a table as a picture.
//
// For a chat platform that renders a code block in whatever font it happens
// to have. An aligned table only stays aligned if the thing drawing it agrees
// about how wide each glyph is, and on Discord that is decided by a fallback
// font nobody here chose — so a table of Chinese and Latin text arrives bent
// however carefully its columns were counted.
//
// A picture is the layout, rather than a description of one somebody else
// draws. What it costs is real and is not hidden: the text in it cannot be
// selected, searched, or read by a screen reader.
package tableimage

import (
	"fmt"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// Where a font with Chinese, Japanese and Korean glyphs usually is.
//
// Probed rather than embedded. One of these files is around twenty megabytes,
// and a daemon that carried a copy so that a chat table could look right
// would be a daemon twenty megabytes larger for everyone who never posts one.
//
// Order matters only in that the first readable one wins; they are all
// suitable, and a machine with none of them falls back to text.
var candidates = []string{
	// macOS
	"/System/Library/Fonts/PingFang.ttc",
	"/System/Library/Fonts/Hiragino Sans GB.ttc",
	"/System/Library/Fonts/STHeiti Medium.ttc",
	// Linux, as packaged by the usual distributions
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
	// Windows
	"C:\\Windows\\Fonts\\msjh.ttc",
	"C:\\Windows\\Fonts\\msyh.ttc",
}

// Fonts is a loaded typeface at the two weights a table uses.
type Fonts struct {
	Header font.Face
	Body   font.Face
}

// Close releases both faces.
func (f Fonts) Close() error {
	if f.Header != nil {
		_ = f.Header.Close()
	}
	if f.Body != nil {
		_ = f.Body.Close()
	}
	return nil
}

// Load finds a typeface that can draw Chinese and opens it at the two sizes.
//
// paths overrides where to look, for a test that has to be able to fail. Empty
// means the usual places.
func Load(paths []string) (Fonts, error) {
	if len(paths) == 0 {
		paths = candidates
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		parsed, err := parse(raw)
		if err != nil {
			continue
		}

		header, err := opentype.NewFace(parsed, &opentype.FaceOptions{
			Size: pointSize, DPI: density, Hinting: font.HintingFull,
		})
		if err != nil {
			continue
		}
		body, err := opentype.NewFace(parsed, &opentype.FaceOptions{
			Size: pointSize, DPI: density, Hinting: font.HintingFull,
		})
		if err != nil {
			_ = header.Close()
			continue
		}
		return Fonts{Header: header, Body: body}, nil
	}

	return Fonts{}, fmt.Errorf("tableimage: no font with Chinese glyphs in %d places", len(paths))
}

// parse reads a font file, which may hold one typeface or several.
//
// The system fonts that carry Chinese are nearly all collections — one file
// holding a whole family — and reading one as a single font simply fails.
func parse(raw []byte) (*sfnt.Font, error) {
	if collection, err := sfnt.ParseCollection(raw); err == nil && collection.NumFonts() > 0 {
		return collection.Font(0)
	}
	return sfnt.Parse(raw)
}
