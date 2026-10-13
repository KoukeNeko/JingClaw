package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// Hash names the text of a document, so whoever edits it can say which version
// they started from.
func Hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Preview applies edits to the text of the config and says what the result
// would do: what changes, and what, if anything, is wrong with it.
//
// Nothing is saved. The problems are the ones the daemon would report on
// starting with this text, because they come from the same check.
func Preview(src []byte, edits []Edit) (Patched, []Problem, error) {
	patched, err := Patch(src, edits)
	if err != nil {
		return Patched{}, nil, err
	}

	problems, err := Check(patched.Text)
	if err != nil {
		return Patched{}, nil, err
	}
	return patched, problems, nil
}

// Check says what is wrong with a text as a whole config, and nothing is when
// it would start.
//
// The check the daemon runs on the saved text when it starts: the same reading
// of it, over the same environment, and the same Validate. An error is a text
// that cannot be read as settings at all; problems are settings in it that will
// not do. The bootstrap keys are not part of it — a line for one is ignored
// when the text is read, not refused — so a save that wants to refuse them asks
// BootstrapProblems as well.
func Check(content []byte) ([]Problem, error) {
	candidate, err := Candidate(content)
	if err != nil {
		return nil, err
	}

	var invalid *InvalidError
	if err := candidate.Validate(); errors.As(err, &invalid) {
		return invalid.Problems, nil
	}
	return nil, nil
}

// Edit is one change to the config text: give a setting a value, or take it
// back to its default.
type Edit struct {
	Key string

	// Value is the plain form Flatten reports. A number arriving from JSON is a
	// float64, and that is accepted wherever an integer is.
	Value any

	// Remove writes the setting back as the commented-out default the example
	// started it as. The text then says nothing about it, which is what a
	// setting nobody has changed looks like.
	Remove bool
}

// Change is what one edit did to the text, for showing before it is written.
type Change struct {
	// Table is the section the lines belong to.
	Table   string
	Removed []string
	Added   []string
}

// Patched is the text after its edits.
type Patched struct {
	Text    []byte
	Changes []Change
}

// DiffLine is one line of what a write would change.
type DiffLine struct {
	// Kind is "context" for a section heading, "removed" or "added".
	Kind string
	Text string
}

// Diff lays the changes out grouped by section, the way the text is read.
func (p Patched) Diff() []DiffLine {
	var out []DiffLine
	last := "\x00"
	for _, change := range p.Changes {
		if change.Table != last {
			out = append(out, DiffLine{Kind: "context", Text: "@@ [" + change.Table + "]"})
			last = change.Table
		}
		for _, line := range change.Removed {
			out = append(out, DiffLine{Kind: "removed", Text: line})
		}
		for _, line := range change.Added {
			out = append(out, DiffLine{Kind: "added", Text: line})
		}
	}
	return out
}

// Patch applies edits to the text of the config and touches nothing else in it.
//
// The text is the operator's. It carries their comments, the order they put
// things in, and settings this version may not know about, and none of that
// survives being read into a struct and written back out. So an edit is made
// on the text: the line that sets the key is replaced, the commented-out
// default it started as is switched on, or a new line is added to its section.
// After that every setting nobody edited is compared with what it was before,
// because a patch that quietly changed something else is the failure that
// matters here, and the way to notice is to look.
func Patch(src []byte, edits []Edit) (Patched, error) {
	known := make(map[string]Setting)
	for _, setting := range Settings() {
		known[setting.Key] = setting
	}

	text := bytes.Clone(src)
	var changes []Change

	for _, edit := range edits {
		setting, ok := known[edit.Key]
		if !ok {
			return Patched{}, fmt.Errorf("config: %q is not a setting", edit.Key)
		}

		var change Change
		var err error
		if setting.Kind == KindList {
			text, change, err = editList(text, edit, setting)
		} else {
			text, change, err = editValue(text, edit, setting)
		}
		if err != nil {
			return Patched{}, fmt.Errorf("config: %s: %w", edit.Key, err)
		}
		if len(change.Removed) != 0 || len(change.Added) != 0 {
			changes = append(changes, change)
		}
	}

	if err := nothingElseChanged(src, text, edits, known); err != nil {
		return Patched{}, err
	}
	return Patched{Text: text, Changes: changes}, nil
}

// ── the text as lines of settings ──────────────────────────────────────

type node struct {
	kind unstable.Kind
	// path is the section's name for a heading, and the section plus the key
	// for a setting, so text written with dotted keys is found the same way
	// as text written with sections.
	path string
	// written is the key as the text spells it, relative to the section the
	// line is in: "retry.base_delay" in text that uses dotted keys.
	written string
	// start is the offset of the line it begins on, and end the offset past
	// its last line. The blank and comment lines that follow a setting belong
	// to whatever comes next, so end stops before them.
	start, end int
}

func scan(text []byte) ([]node, error) {
	var parser unstable.Parser
	parser.Reset(text)

	var (
		nodes []node
		table string
	)
	for parser.NextExpression() {
		expr := parser.Expression()

		path, first := keyPath(expr)
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = path
			nodes = append(nodes, node{kind: expr.Kind, path: path, start: lineStart(text, first)})
		case unstable.KeyValue:
			written := path
			if table != "" {
				path = table + "." + path
			}
			nodes = append(nodes, node{kind: expr.Kind, path: path, written: written, start: lineStart(text, first)})
		}
	}
	if err := parser.Error(); err != nil {
		return nil, fmt.Errorf("does not parse as TOML: %w", err)
	}

	for i := range nodes {
		limit := len(text)
		if i+1 < len(nodes) {
			limit = nodes[i+1].start
		}
		nodes[i].end = withoutTrailer(text, nodes[i].start, limit)
	}
	return nodes, nil
}

// keyPath joins the parts of a key, and says where the first of them is.
func keyPath(expr *unstable.Node) (string, int) {
	var parts []string
	offset := -1

	for it := expr.Key(); it.Next(); {
		part := it.Node()
		if offset < 0 {
			// Its own range. The text of a key with an escape in it is not a
			// slice of the input, and asking where it is would panic.
			offset = int(part.Raw.Offset)
		}
		parts = append(parts, string(part.Data))
	}
	return strings.Join(parts, "."), offset
}

func lineStart(text []byte, offset int) int {
	if offset < 0 {
		return 0
	}
	return bytes.LastIndexByte(text[:offset], '\n') + 1
}

// withoutTrailer is where a setting's own lines stop.
func withoutTrailer(text []byte, start, limit int) int {
	end := limit
	for end > start {
		body := text[start:end]
		if body[len(body)-1] == '\n' {
			body = body[:len(body)-1]
		}
		begin := start + bytes.LastIndexByte(body, '\n') + 1
		if begin <= start {
			break
		}

		line := bytes.TrimSpace(text[begin:end])
		if len(line) != 0 && line[0] != '#' {
			break
		}
		end = begin
	}
	return end
}

func lines(text []byte) []string {
	trimmed := strings.TrimRight(string(text), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func splitKey(key string) (table, leaf string) {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// ── a single setting ───────────────────────────────────────────────────

func editValue(text []byte, edit Edit, setting Setting) ([]byte, Change, error) {
	table, leaf := splitKey(edit.Key)
	change := Change{Table: table}

	nodes, err := scan(text)
	if err != nil {
		return nil, change, err
	}

	var value string
	if edit.Remove {
		value = mustRender(setting.Kind, setting.Default)
	} else {
		rendered, err := render(setting.Kind, edit.Value)
		if err != nil {
			return nil, change, err
		}
		value = rendered
	}
	prefix := ""
	if edit.Remove {
		prefix = "# "
	}

	// It is set already: replace what sets it, spelled the way the text spells
	// it. Text that writes retry.base_delay under [provider] keeps writing it
	// that way; the short name there would be a different setting.
	for _, n := range nodes {
		if n.kind == unstable.KeyValue && n.path == edit.Key {
			line := prefix + n.written + " = " + value
			change.Removed = lines(text[n.start:n.end])
			change.Added = []string{line}
			return splice(text, n.start, n.end, line+"\n"), change, nil
		}
	}

	// Nothing to take back: the text already says nothing about it.
	if edit.Remove {
		return text, Change{Table: table}, nil
	}

	line := leaf + " = " + value
	change.Added = []string{line}

	section, found := sectionOf(nodes, table, len(text))
	if !found {
		return appendSection(text, table, line), change, nil
	}

	// The example lists every setting commented out. Turning that line on
	// keeps the setting where somebody reading the text expects it.
	placeholder := regexp.MustCompile(`^[ \t]*#[ \t]*` + regexp.QuoteMeta(leaf) + `[ \t]*=`)
	for offset := section.start; offset < section.stop; {
		next := bytes.IndexByte(text[offset:], '\n')
		stop := section.stop
		if next >= 0 {
			stop = offset + next + 1
		}

		if !covered(nodes, offset) && placeholder.Match(text[offset:stop]) {
			change.Removed = lines(text[offset:stop])
			return splice(text, offset, stop, line+"\n"), change, nil
		}
		offset = stop
	}

	return insertLine(text, section.after, line+"\n"), change, nil
}

// covered says whether an offset is inside a setting's own lines, where a
// line that begins with # is a comment in the middle of its value.
func covered(nodes []node, offset int) bool {
	for _, n := range nodes {
		if n.kind == unstable.KeyValue && offset >= n.start && offset < n.end {
			return true
		}
	}
	return false
}

type span struct {
	// start and stop bound the section's text, heading included.
	start, stop int
	// after is where a new setting goes: after the last one, or the heading.
	after int
}

func sectionOf(nodes []node, table string, size int) (span, bool) {
	for i, n := range nodes {
		if n.kind != unstable.Table || n.path != table {
			continue
		}

		found := span{start: n.start, stop: size, after: n.end}
		for _, later := range nodes[i+1:] {
			if later.kind != unstable.KeyValue {
				found.stop = later.start
				break
			}
			found.after = later.end
		}
		return found, true
	}
	return span{}, false
}

// insertLine puts text at an offset, after finishing the line before it when
// that one is the last in text with no final newline.
func insertLine(text []byte, at int, with string) []byte {
	if at > 0 && text[at-1] != '\n' {
		with = "\n" + with
	}
	return splice(text, at, at, with)
}

func appendSection(text []byte, table, line string) []byte {
	out := bytes.Clone(text)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return append(out, "["+table+"]\n"+line+"\n"...)
}

func splice(text []byte, from, to int, with string) []byte {
	out := make([]byte, 0, len(text)-(to-from)+len(with))
	out = append(out, text[:from]...)
	out = append(out, with...)
	return append(out, text[to:]...)
}

// ── a list of tables ───────────────────────────────────────────────────

// editList replaces a whole list.
//
// A list of tables is several blocks, not one line, and editing one element in
// place would mean working out which block is which when the text may have
// reordered or commented them. So the list is written as a unit: the blocks it
// has are taken out, and the new ones go where the first of them was.
func editList(text []byte, edit Edit, setting Setting) ([]byte, Change, error) {
	table, _ := splitKey(edit.Key)
	change := Change{Table: edit.Key}

	var items []any
	if !edit.Remove {
		var ok bool
		if items, ok = edit.Value.([]any); !ok {
			return nil, change, fmt.Errorf("a list of tables is written as a list, not %T", edit.Value)
		}
	}

	block, err := renderItems(edit.Key, setting, items)
	if err != nil {
		return nil, change, err
	}

	nodes, err := scan(text)
	if err != nil {
		return nil, change, err
	}

	// Every block of the list, and the sections hanging off its elements.
	var spans []span
	for i, n := range nodes {
		if n.kind == unstable.KeyValue || (n.path != edit.Key && !strings.HasPrefix(n.path, edit.Key+".")) {
			continue
		}

		stop := len(text)
		for _, later := range nodes[i+1:] {
			if later.kind != unstable.KeyValue {
				stop = later.start
				break
			}
		}
		spans = append(spans, span{start: n.start, stop: withoutTrailer(text, n.start, stop)})
	}

	if len(spans) == 0 && block == "" {
		return text, change, nil
	}

	var out []byte
	if len(spans) == 0 {
		at := len(text)
		if section, found := sectionOf(nodes, table, len(text)); found {
			at = section.after
		}
		out = insertLine(text, at, "\n"+block)
		change.Added = lines([]byte(block))
		return out, change, nil
	}

	// Taken out back to front, so the offsets that are left stay true.
	out = bytes.Clone(text)
	for i := len(spans) - 1; i >= 0; i-- {
		change.Removed = append(lines(text[spans[i].start:spans[i].stop]), change.Removed...)
		out = splice(out, spans[i].start, spans[i].stop, "")
	}
	out = splice(out, spans[0].start, spans[0].start, block)
	change.Added = lines([]byte(block))
	return out, change, nil
}

func renderItems(key string, setting Setting, items []any) (string, error) {
	var out strings.Builder

	for n, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("element %d is not a table", n+1)
		}

		valid := make(map[string]bool, len(setting.Items))
		for _, inner := range setting.Items {
			valid[inner.Key] = true
		}
		for name := range fields {
			if !valid[name] {
				return "", fmt.Errorf("element %d has %q, which is not a setting of it", n+1, name)
			}
		}

		if n > 0 {
			out.WriteString("\n")
		}
		out.WriteString("[[" + key + "]]\n")

		// In the order the struct declares them, so two edits of one list
		// produce the same text.
		for _, inner := range setting.Items {
			value, present := fields[inner.Key]
			if !present || isEmpty(value) {
				continue
			}
			rendered, err := render(inner.Kind, value)
			if err != nil {
				return "", fmt.Errorf("element %d, %s: %w", n+1, inner.Key, err)
			}
			out.WriteString(inner.Key + " = " + rendered + "\n")
		}
	}
	return out.String(), nil
}

// isEmpty is a value that writing down adds nothing to: it is what an absent
// key reads as.
func isEmpty(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case bool:
		return !value
	case float64:
		return value == 0
	case int:
		return value == 0
	case int64:
		return value == 0
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	}
	return false
}

// ── writing values ─────────────────────────────────────────────────────

func mustRender(kind Kind, v any) string {
	text, err := render(kind, v)
	if err != nil {
		// A default that cannot be written is a bug in the defaults.
		panic(err)
	}
	return text
}

func render(kind Kind, v any) (string, error) {
	switch kind {
	case KindBool:
		if value, ok := v.(bool); ok {
			return strconv.FormatBool(value), nil
		}

	case KindInt:
		if number, ok := asNumber(v); ok && number == math.Trunc(number) && math.Abs(number) < 1<<53 {
			return strconv.FormatInt(int64(number), 10), nil
		}

	case KindFloat:
		if number, ok := asNumber(v); ok && !math.IsNaN(number) && !math.IsInf(number, 0) {
			text := strconv.FormatFloat(number, 'g', -1, 64)
			if !strings.ContainsAny(text, ".e") {
				text += ".0"
			}
			return text, nil
		}

	case KindString:
		if value, ok := v.(string); ok {
			return tomlString(value), nil
		}

	case KindDuration:
		if value, ok := v.(string); ok {
			if _, err := time.ParseDuration(value); err != nil {
				return "", fmt.Errorf("%q is not a duration", value)
			}
			return tomlString(value), nil
		}

	case KindStrings:
		if list, ok := v.([]any); ok {
			parts := make([]string, len(list))
			for i, element := range list {
				text, ok := element.(string)
				if !ok {
					return "", fmt.Errorf("a list of text has %T in it", element)
				}
				parts[i] = tomlString(text)
			}
			return "[" + strings.Join(parts, ", ") + "]", nil
		}

	case KindMap:
		if table, ok := v.(map[string]any); ok {
			names := make([]string, 0, len(table))
			for name := range table {
				names = append(names, name)
			}
			sort.Strings(names)

			parts := make([]string, len(names))
			for i, name := range names {
				text, ok := table[name].(string)
				if !ok {
					return "", fmt.Errorf("%s: a table of text has %T in it", name, table[name])
				}
				parts[i] = bareKey(name) + " = " + tomlString(text)
			}
			if len(parts) == 0 {
				return "{}", nil
			}
			return "{ " + strings.Join(parts, ", ") + " }", nil
		}
	}

	return "", fmt.Errorf("a %s cannot be written as %T", kind, v)
}

func asNumber(v any) (float64, bool) {
	switch value := v.(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	}
	return 0, false
}

var bareKeyText = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func bareKey(name string) string {
	if bareKeyText.MatchString(name) {
		return name
	}
	return tomlString(name)
}

// tomlString writes a TOML basic string. Go's own quoting is not it: it has
// escapes TOML does not (\a, \v, \x), and text that does not parse is worse
// than the value it was trying to carry.
func tomlString(s string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\t':
			out.WriteString(`\t`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\f':
			out.WriteString(`\f`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&out, `\u%04X`, r)
		case r == utf8.RuneError:
			out.WriteString(`�`)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// ── the check that nothing else moved ──────────────────────────────────

func nothingElseChanged(before, after []byte, edits []Edit, known map[string]Setting) error {
	was, err := settingsInFile(before, known)
	if err != nil {
		// Text that did not parse could not have been patched either.
		return fmt.Errorf("config: the text does not parse as TOML: %w", err)
	}
	now, err := settingsInFile(after, known)
	if err != nil {
		return fmt.Errorf("config: the edit left text that does not parse as TOML: %w", err)
	}

	edited := make(map[string]Edit, len(edits))
	for _, edit := range edits {
		edited[edit.Key] = edit
	}

	for key, value := range was {
		if _, ours := edited[key]; ours {
			continue
		}
		if !sameValue(value, now[key]) {
			return fmt.Errorf("config: editing the text changed %s as well, which was not asked for", key)
		}
	}
	for key := range now {
		if _, ours := edited[key]; ours {
			continue
		}
		if _, existed := was[key]; !existed {
			return fmt.Errorf("config: editing the text added %s, which was not asked for", key)
		}
	}

	for key, edit := range edited {
		want := edit.Value
		if edit.Remove {
			if _, stays := now[key]; stays {
				return fmt.Errorf("config: %s is still set after being taken back", key)
			}
			continue
		}
		if known[key].Kind == KindList {
			want = withoutEmpty(want)

			// An empty list and text that does not mention it say the same.
			if list, _ := want.([]any); len(list) == 0 {
				if _, stays := now[key]; stays {
					return fmt.Errorf("config: %s is still set after being emptied", key)
				}
				continue
			}
		}
		if !sameValue(want, now[key]) {
			return fmt.Errorf("config: %s did not come out as written", key)
		}
	}
	return nil
}

// settingsInFile reads the keys a text sets, by dotted name.
func settingsInFile(content []byte, known map[string]Setting) (map[string]any, error) {
	tree, err := toml.Parser().Unmarshal(content)
	if err != nil {
		return nil, err
	}

	out := make(map[string]any)
	var walk func(prefix string, table map[string]any)
	walk = func(prefix string, table map[string]any) {
		for name, value := range table {
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			// A setting is a leaf even when it is a table: its contents are
			// its value, not more settings.
			if nested, ok := value.(map[string]any); ok && known[key].Key == "" {
				walk(key, nested)
				continue
			}
			out[key] = value
		}
	}
	walk("", tree)
	return out, nil
}

// withoutEmpty drops the empty fields of a list's elements, which is how they
// are written and so how they read back.
func withoutEmpty(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}

	out := make([]any, len(list))
	for i, element := range list {
		fields, ok := element.(map[string]any)
		if !ok {
			out[i] = element
			continue
		}
		kept := make(map[string]any, len(fields))
		for name, value := range fields {
			if !isEmpty(value) {
				kept[name] = value
			}
		}
		out[i] = kept
	}
	return out
}

// sameValue compares two values as JSON does, so an integer read from the text
// and the number a client sent are the same number.
func sameValue(a, b any) bool {
	return reflect.DeepEqual(viaJSON(a), viaJSON(b))
}

func viaJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}
