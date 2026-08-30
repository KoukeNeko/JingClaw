package tableimage

import (
	"runtime"
	"testing"
)

// TestOneTypefaceIsNotEnough is why the chain exists.
//
// On macOS the typefaces with Chinese have no ✓ ✗ ⚠ and the interface
// typeface has no Chinese. They are different files and always were, so a
// renderer holding one face draws boxes wherever the other one was needed.
func TestOneTypefaceIsNotEnough(t *testing.T) {
	fonts := loaded(t)

	for _, r := range []rune{'漢', 'A', '✓', '✗', '⚠', '○', '×', '△'} {
		if _, ok := fonts.Body.GlyphAdvance(r); !ok {
			t.Errorf("nothing loaded can draw %q (U+%04X)", r, r)
		}
	}
}

// TestTheChainIsLedByTheTypefaceWithTheScript keeps the rows the right height.
//
// The first face decides the metrics. A chain led by a Latin typeface lays
// out Chinese at Latin heights, and the result is text clipped by its own row
// — which reads as a rendering fault rather than as a measurement one.
func TestTheChainIsLedByTheTypefaceWithTheScript(t *testing.T) {
	fonts := loaded(t)

	chained, ok := fonts.Body.(*fallback)
	if !ok {
		t.Skip("one typeface here covers everything, so there is no chain to lead")
	}

	if _, has := chained.faces[0].GlyphAdvance('漢'); !has {
		t.Error("the chain is led by a typeface with no Chinese")
	}

	// And the metrics are an envelope, not the leader's alone.
	envelope := fonts.Body.Metrics()
	for index, face := range chained.faces {
		one := face.Metrics()
		if one.Height > envelope.Height {
			t.Errorf("face %d is taller (%v) than the chain reports (%v)",
				index, one.Height, envelope.Height)
		}
		if one.Ascent > envelope.Ascent {
			t.Errorf("face %d rises higher (%v) than the chain reports (%v)",
				index, one.Ascent, envelope.Ascent)
		}
	}
}

// TestNoKerningIsClaimedBetweenTypefaces is the honest answer to a pair
// neither face has seen.
//
// One typeface's kerning table says nothing about another's shapes, so
// asking either about a pair that spans them is asking about something it has
// no opinion on.
func TestNoKerningIsClaimedBetweenTypefaces(t *testing.T) {
	fonts := loaded(t)

	chained, ok := fonts.Body.(*fallback)
	if !ok || len(chained.faces) < 2 {
		t.Skip("only one typeface here")
	}

	// A rune from each end of the chain. Which faces answer is what decides
	// the assertion, so it is asked rather than assumed.
	if chained.faceFor('漢') == chained.faceFor('✓') {
		t.Skip("both came from the same typeface on this machine")
	}
	if got := fonts.Body.Kern('漢', '✓'); got != 0 {
		t.Errorf("kerning was claimed across typefaces: %v", got)
	}
}

// TestAMachineWithNoneOfTheKnownPathsStillDrawsATable is the point of
// searching rather than listing.
//
// The list of paths is a fast path, not the mechanism. A distribution that
// packages its fonts somewhere else, a container with one typeface installed
// by hand, a macOS release that moved something — each of those used to mean
// no table at all.
func TestAMachineWithNoneOfTheKnownPathsStillDrawsATable(t *testing.T) {
	found := installed()
	if len(found) == 0 {
		t.Skip("no typefaces are installed here at all")
	}

	// Everything this machine has, minus the shortcuts. What is left is what
	// a machine none of the known paths fit would be working from.
	known := make(map[string]bool)
	for _, path := range preferred[runtime.GOOS] {
		known[path] = true
	}
	var searched []string
	for _, path := range found {
		if !known[path] {
			searched = append(searched, path)
		}
	}

	fonts, err := Load(searched)
	if err != nil {
		t.Fatalf("no table could be drawn from %d installed typefaces: %v",
			len(searched), err)
	}
	defer fonts.Close()

	for _, r := range []rune{'漢', 'A'} {
		if _, ok := fonts.Body.GlyphAdvance(r); !ok {
			t.Errorf("what was found cannot draw %q", r)
		}
	}
}

// TestSearchingIsBoundedByWhatIsStillMissing keeps the chain from growing to
// the length of the machine's font directory.
//
// Every face is asked about every unseen rune and every one of them was a
// file read and parsed, so a chain that took whatever it found would make
// drawing a table cost more the more typefaces somebody has installed.
func TestSearchingIsBoundedByWhatIsStillMissing(t *testing.T) {
	fonts := loaded(t)

	chained, ok := fonts.Body.(*fallback)
	if !ok {
		return // one face covered everything, which is the cheapest outcome
	}
	if len(chained.faces) > mostFaces {
		t.Errorf("the chain grew to %d faces", len(chained.faces))
	}

	// And each one earns its place: removing it loses something.
	for index := range chained.faces {
		var without []rune
		for _, r := range append(append([]rune{}, needed.script...), needed.marks...) {
			answered := chained.faceFor(r)
			if answered == chained.faces[index] {
				without = append(without, r)
			}
		}
		if len(without) == 0 && index > 0 {
			t.Errorf("face %d answers for nothing the others do not", index)
		}
	}
}
