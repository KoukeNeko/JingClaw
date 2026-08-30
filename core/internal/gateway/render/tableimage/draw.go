package tableimage

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// Everything here is in the picture's own pixels, which are twice the size the
// reader sees.
//
// Designed at the displayed size and then doubled, rather than the other way
// round. Picking a pixel size first and hoping is how a table that looks
// beautiful at its own scale arrives as unreadable: Discord shows it at about
// seven hundred, so an eighteen-pixel letter drawn into an eighteen-hundred
// pixel image reaches the reader at seven.
const (
	scale = 2

	// The width the reader sees, and the width past which the table stops
	// growing and starts wrapping instead.
	displayedWidth = 700
	displayedCap   = 900

	targetWidth = displayedWidth * scale
	maxWidth    = displayedCap * scale

	// 11pt at twice the usual density is about fifteen displayed pixels.
	pointSize = 11
	density   = 72 * scale

	padX       = 10 * scale
	padY       = 5 * scale
	rowHeight  = 25 * scale
	headHeight = 27 * scale
	margin     = 8 * scale
	ruleHeight = 1 * scale

	// A cell that needs more lines than this is cut, with a mark saying so.
	// Three is where a row stops being a row and becomes a paragraph in a
	// grid.
	maxLines = 3
)

// The Discord dark palette, so the picture sits in the message rather than on
// top of it.
var (
	background = color.RGBA{0x31, 0x33, 0x38, 0xff}
	headerFill = color.RGBA{0x2b, 0x2d, 0x31, 0xff}
	oddFill    = color.RGBA{0x31, 0x33, 0x38, 0xff}
	evenFill   = color.RGBA{0x35, 0x37, 0x3c, 0xff}
	rule       = color.RGBA{0x44, 0x47, 0x4e, 0xff}
	headerInk  = color.RGBA{0xf2, 0xf3, 0xf5, 0xff}
	bodyInk    = color.RGBA{0xdb, 0xde, 0xe1, 0xff}
)

// Table is what to draw: a header and rows, already in display order.
type Table struct {
	Header []string
	Rows   [][]string
}

// Draw renders the table and encodes it as a PNG.
func Draw(table Table, fonts Fonts) ([]byte, error) {
	if len(table.Header) == 0 && len(table.Rows) == 0 {
		return nil, fmt.Errorf("tableimage: nothing to draw")
	}

	laid := layout(table, fonts)
	picture := image.NewRGBA(image.Rect(0, 0, laid.width, laid.height))
	draw.Draw(picture, picture.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)

	laid.paint(picture, fonts)

	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		return nil, fmt.Errorf("tableimage: encode: %w", err)
	}
	return out.Bytes(), nil
}

// cell is one drawn box: its text already broken into the lines it will take.
type cell struct {
	lines   []string
	rightly bool
}

// laidOut is a table with every measurement settled.
type laidOut struct {
	width, height int
	columns       []int
	header        []cell
	rows          [][]cell
	headerTop     int
	rowTops       []int
	rowHeights    []int
}

// layout measures everything before anything is drawn.
//
// Columns are sized by what the text actually measures rather than by
// counting characters: a Chinese glyph is not two Latin ones, and neither is
// an emoji, and a column sized by counting is a column that is wrong by a
// different amount in every row.
func layout(table Table, fonts Fonts) laidOut {
	count := len(table.Header)
	for _, row := range table.Rows {
		if len(row) > count {
			count = len(row)
		}
	}
	if count == 0 {
		count = 1
	}

	// What each column would like to be.
	wanted := make([]int, count)
	for index, text := range table.Header {
		wanted[index] = max(wanted[index], measure(fonts.Header, text))
	}
	for _, row := range table.Rows {
		for index, text := range row {
			wanted[index] = max(wanted[index], measure(fonts.Body, text))
		}
	}
	for index := range wanted {
		// Rounded up to a whole displayed pixel. The picture is shown at half
		// its size, so an odd width puts a column boundary on half a pixel and
		// the client decides which side it lands on.
		wanted[index] = even(wanted[index] + padX*2)
	}

	columns := fit(wanted)

	laid := laidOut{columns: columns}
	for _, width := range columns {
		laid.width += width
	}
	laid.width += margin * 2

	laid.header = make([]cell, count)
	for index := range count {
		text := ""
		if index < len(table.Header) {
			text = table.Header[index]
		}
		laid.header[index] = cell{lines: wrap(fonts.Header, text, columns[index]-padX*2)}
	}

	laid.rows = make([][]cell, len(table.Rows))
	for at, row := range table.Rows {
		cells := make([]cell, count)
		for index := range count {
			text := ""
			if index < len(row) {
				text = row[index]
			}
			cells[index] = cell{
				lines:   wrap(fonts.Body, text, columns[index]-padX*2),
				rightly: isNumber(text),
			}
		}
		laid.rows[at] = cells
	}

	// Heights, once the wrapping is known.
	laid.headerTop = margin
	height := margin + heightOf(laid.header, headHeight) + ruleHeight

	laid.rowTops = make([]int, len(laid.rows))
	laid.rowHeights = make([]int, len(laid.rows))
	for at, cells := range laid.rows {
		laid.rowTops[at] = height
		laid.rowHeights[at] = heightOf(cells, rowHeight)
		height += laid.rowHeights[at]
	}
	laid.height = height + margin

	return laid
}

// fit brings the columns within the width a reader will see.
//
// Wrapping rather than shrinking or clipping. Shrinking is unreadable well
// before it is narrow enough, since Chinese stops being legible around
// thirteen displayed pixels and a phone shows the picture smaller again.
// Clipping removes a value without saying it did, which is the one thing a
// table must never do.
func fit(wanted []int) []int {
	total := 0
	for _, width := range wanted {
		total += width
	}
	if total+margin*2 <= maxWidth {
		return wanted
	}

	// Take the excess from the widest columns first, which are the ones with
	// prose in them; a narrow column is a date or an identifier and taking
	// from it only makes it wrap without saving much.
	over := total + margin*2 - maxWidth
	fitted := append([]int(nil), wanted...)
	for over > 0 {
		widest, at := 0, -1
		for index, width := range fitted {
			if width > widest {
				widest, at = width, index
			}
		}
		if at < 0 || widest <= padX*2+minimumColumn {
			break
		}
		take := min(over, widest-(padX*2+minimumColumn))
		fitted[at] -= take
		over -= take
	}
	return fitted
}

// minimumColumn is the narrowest a column is allowed to become, in the
// picture's pixels: about four Chinese characters.
const minimumColumn = 30 * scale * 2

func heightOf(cells []cell, single int) int {
	lines := 1
	for _, c := range cells {
		lines = max(lines, len(c.lines))
	}
	if lines <= 1 {
		return single
	}
	return single + (lines-1)*(single-padY*2)
}

func measure(face font.Face, text string) int {
	return font.MeasureString(face, text).Ceil()
}

// wrap breaks text into the lines it needs at the given width.
func wrap(face font.Face, text string, width int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{""}
	}
	if width <= 0 || measure(face, text) <= width {
		return []string{text}
	}

	var lines []string
	remaining := []rune(text)
	for len(remaining) > 0 {
		if len(lines) == maxLines-1 {
			lines = append(lines, clipTo(face, string(remaining), width))
			break
		}

		taken := 0
		for taken < len(remaining) {
			if measure(face, string(remaining[:taken+1])) > width {
				break
			}
			taken++
		}
		if taken == 0 {
			taken = 1
		}
		lines = append(lines, strings.TrimRight(string(remaining[:taken]), " "))
		remaining = remaining[taken:]
	}
	return lines
}

// clipTo shortens text to width, ending with a mark that says it was cut.
func clipTo(face font.Face, text string, width int) string {
	if measure(face, text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 {
		shortened := string(runes) + "…"
		if measure(face, shortened) <= width {
			return shortened
		}
		runes = runes[:len(runes)-1]
	}
	return "…"
}

// isNumber reports whether a cell holds a quantity rather than a label.
//
// Right-aligned when it does, so digits line up by place value. Deliberately
// not given a colour of its own: a different one would say numbers matter
// more than what they are about, which is not something a table gets to
// decide.
func isNumber(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}

	stripped := strings.NewReplacer(",", "", "%", "", "$", "", " ", "").Replace(trimmed)
	if stripped == "" {
		return false
	}
	_, err := strconv.ParseFloat(stripped, 64)
	return err == nil
}

// paint draws the laid-out table onto the picture.
//
// Bands rather than a grid: alternating row fills and one rule under the
// header are enough to follow a row across, and vertical lines are not. At
// seven hundred pixels they cut the picture into boxes, and Chinese is dense
// enough already — the lines meant to organise it are what makes it noisy.
func (l laidOut) paint(picture *image.RGBA, fonts Fonts) {
	l.fill(picture, l.headerTop, heightOf(l.header, headHeight), headerFill)
	l.write(picture, fonts.Header, headerInk, l.header, l.headerTop, heightOf(l.header, headHeight))

	// The one rule there is, under the header.
	ruleTop := l.headerTop + heightOf(l.header, headHeight)
	draw.Draw(picture,
		image.Rect(margin, ruleTop, l.width-margin, ruleTop+ruleHeight),
		image.NewUniform(rule), image.Point{}, draw.Src)

	for at, cells := range l.rows {
		shade := oddFill
		if at%2 == 1 {
			shade = evenFill
		}
		l.fill(picture, l.rowTops[at], l.rowHeights[at], shade)
		l.write(picture, fonts.Body, bodyInk, cells, l.rowTops[at], l.rowHeights[at])
	}
}

func (l laidOut) fill(picture *image.RGBA, top, height int, shade color.Color) {
	draw.Draw(picture,
		image.Rect(margin, top, l.width-margin, top+height),
		image.NewUniform(shade), image.Point{}, draw.Src)
}

// write draws one row of cells.
func (l laidOut) write(
	picture *image.RGBA, face font.Face, ink color.Color, cells []cell, top, height int,
) {
	metrics := face.Metrics()
	lineHeight := metrics.Height.Ceil()

	left := margin
	for index, c := range cells {
		width := l.columns[index]

		// Centred vertically on the row, so a wrapped cell and a single-line
		// one beside it read as the same row rather than as two.
		block := len(c.lines) * lineHeight
		baseline := top + (height-block)/2 + metrics.Ascent.Ceil()

		for _, line := range c.lines {
			x := left + padX
			if c.rightly {
				x = left + width - padX - measure(face, line)
			}

			drawer := &font.Drawer{
				Dst:  picture,
				Src:  image.NewUniform(ink),
				Face: face,
				Dot:  fixed.P(x, baseline),
			}
			drawer.DrawString(line)
			baseline += lineHeight
		}
		left += width
	}
}

// even rounds up to a multiple of the scale, so every measurement in the
// picture is a whole number of the pixels a reader actually sees.
func even(value int) int {
	if remainder := value % scale; remainder != 0 {
		return value + scale - remainder
	}
	return value
}
