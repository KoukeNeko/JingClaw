package tableimage

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func loaded(t *testing.T) Fonts {
	t.Helper()
	fonts, err := Load(nil)
	if err != nil {
		t.Skipf("no font with Chinese glyphs on this machine: %v", err)
	}
	t.Cleanup(func() { _ = fonts.Close() })
	return fonts
}

func decode(t *testing.T, drawn []byte) image.Image {
	t.Helper()
	picture, err := png.Decode(bytes.NewReader(drawn))
	if err != nil {
		t.Fatalf("what was drawn is not a picture: %v", err)
	}
	return picture
}

// The height is arithmetic, so it can be asserted rather than looked at: a
// margin, a header, the one rule, a row each, and a margin.
func TestTheHeightIsTheRowsItWasGiven(t *testing.T) {
	fonts := loaded(t)

	rows := [][]string{{"a", "1"}, {"b", "2"}, {"c", "3"}}
	drawn, err := Draw(Table{Header: []string{"letter", "number"}, Rows: rows}, fonts)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	picture := decode(t, drawn)
	want := margin + headHeight + ruleHeight + len(rows)*rowHeight + margin
	if got := picture.Bounds().Dy(); got != want {
		t.Errorf("the picture is %d tall, want %d", got, want)
	}
}

// Drawn at twice the size it is shown at. A picture built at the displayed
// size arrives soft, and one built at an arbitrary larger size arrives with
// text too small to read once the client has scaled it down.
func TestItIsDrawnAtTwiceTheDisplayedSize(t *testing.T) {
	fonts := loaded(t)

	drawn, err := Draw(Table{
		Header: []string{"項目", "數據"},
		Rows:   [][]string{{"融資金額", "200 萬美元"}},
	}, fonts)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	picture := decode(t, drawn)
	if width := picture.Bounds().Dx(); width%scale != 0 {
		t.Errorf("the width %d is not a whole number of displayed pixels", width)
	}
	if width := picture.Bounds().Dx(); width > maxWidth {
		t.Errorf("the picture is %d wide, past the cap of %d", width, maxWidth)
	}
}

// The one that matters: Chinese has to reach the picture. A font without the
// glyphs draws blanks, and blanks are the same size as the text that should
// have been there, so nothing about the layout says it went wrong.
func TestChineseIsActuallyDrawn(t *testing.T) {
	fonts := loaded(t)

	chinese, err := Draw(Table{
		Header: []string{"項目"},
		Rows:   [][]string{{"融資金額與營運指標"}},
	}, fonts)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	blank, err := Draw(Table{
		Header: []string{"項目"},
		Rows:   [][]string{{"                 "}},
	}, fonts)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	if inked(decode(t, chinese)) <= inked(decode(t, blank)) {
		t.Error("a row of Chinese put no more ink on the picture than a row of spaces")
	}
}

// inked counts pixels that are neither of the two row fills, which is to say
// the ones that are text.
func inked(picture image.Image) int {
	bounds := picture.Bounds()
	count := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if isText(picture.At(x, y)) {
				count++
			}
		}
	}
	return count
}

// isText reports whether a pixel is part of a glyph.
//
// Above the rule rather than above the backgrounds: the rule is #44474E, and
// counting it as text made "no text was drawn at all" indistinguishable from
// "text was drawn", because both pictures have the same rule in them.
func isText(at color.Color) bool {
	r, g, b, _ := at.RGBA()
	return r>>8 > 0x60 && g>>8 > 0x60 && b>>8 > 0x60
}

// rightmostInk is the last column holding a glyph.
func rightmostInk(picture image.Image, top, bottom int) int {
	bounds := picture.Bounds()
	for x := bounds.Max.X - 1; x >= bounds.Min.X; x-- {
		for y := top; y < bottom; y++ {
			if isText(picture.At(x, y)) {
				return x
			}
		}
	}
	return -1
}

// A table wider than a reader's window wraps rather than growing, shrinking
// or losing anything: a value cut off is a value nobody knows is missing.
func TestAWideTableWrapsRatherThanGrowing(t *testing.T) {
	fonts := loaded(t)

	long := "這是一段很長的說明文字，長到一個欄位放不下，" +
		"必須換行才能完整顯示，而不是把後面的內容裁掉不說。"
	drawn, err := Draw(Table{
		Header: []string{"項目", "說明", "再一欄說明"},
		Rows:   [][]string{{long, long, long}},
	}, fonts)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	picture := decode(t, drawn)
	if width := picture.Bounds().Dx(); width > maxWidth {
		t.Errorf("a wide table grew to %d, past the cap of %d", width, maxWidth)
	}
	// Wrapped, so taller than one row.
	if height := picture.Bounds().Dy(); height <= margin+headHeight+ruleHeight+rowHeight+margin {
		t.Errorf("a wrapped row is %d tall, no taller than an unwrapped one", height)
	}
}

func TestNothingToDrawIsAnError(t *testing.T) {
	fonts := loaded(t)
	if _, err := Draw(Table{}, fonts); err == nil {
		t.Error("an empty table drew something")
	}
}

// A machine with no such font must say so rather than drawing blanks.
func TestNoFontIsAnError(t *testing.T) {
	if _, err := Load([]string{"/nowhere/no-such-font.ttc"}); err == nil {
		t.Error("loading found a font that is not there")
	}
}

// Numbers line up by place value; labels start where the column does.
func TestNumbersAreRightAligned(t *testing.T) {
	for _, text := range []string{"5,000", "10", "15%", "200", "1.5"} {
		if !isNumber(text) {
			t.Errorf("%q was not recognised as a number", text)
		}
	}
	for _, text := range []string{"融資金額", "2025 年 11 月", "500 Global", ""} {
		if isNumber(text) {
			t.Errorf("%q was treated as a number", text)
		}
	}
}

// A number ends at the right edge of its column, so digits line up by place
// value down the column; a label of the same length does not.
//
// Measured from the right edge rather than by comparing where two pieces of
// text start: "1" and "a" begin at slightly different places inside their own
// glyph boxes, and a test that compared those was measuring the typeface.
func TestANumberEndsAtTheRightOfItsColumn(t *testing.T) {
	fonts := loaded(t)

	// One column, made wide by its header, so left and right are far apart.
	wide := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	top := margin + headHeight + ruleHeight
	bottom := top + rowHeight

	drawnWith := func(cell string) (image.Image, int) {
		t.Helper()
		drawn, err := Draw(Table{Header: []string{wide}, Rows: [][]string{{cell}}}, fonts)
		if err != nil {
			t.Fatalf("draw %q: %v", cell, err)
		}
		picture := decode(t, drawn)
		return picture, rightmostInk(picture, top, bottom)
	}

	numberPicture, numberEnds := drawnWith("12")
	_, labelEnds := drawnWith("ab")

	if numberEnds < 0 || labelEnds < 0 {
		t.Fatalf("nothing was drawn: number ends at %d, label at %d", numberEnds, labelEnds)
	}

	// The right edge of the only column, less the padding it keeps.
	edge := numberPicture.Bounds().Dx() - margin - padX
	if numberEnds < edge-scale*4 {
		t.Errorf("the number ends at %d, well short of the column edge at %d", numberEnds, edge)
	}
	if labelEnds >= edge-scale*4 {
		t.Errorf("the label also ends at the column edge (%d); nothing is left aligned", labelEnds)
	}
}
