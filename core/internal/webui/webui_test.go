package webui_test

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/webui"
)

func TestThePageIsServedWithItsPolicy(t *testing.T) {
	rec := httptest.NewRecorder()
	webui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("got %d: %.80s", rec.Code, rec.Body)
	}

	for header, want := range map[string]string{
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s is %q, want it to contain %q", header, got, want)
		}
	}
}

// The page can approve a command. Nothing but a read is a reason to answer.
func TestOnlyReadsAreAnswered(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		webui.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/", strings.NewReader("x")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s got %d", method, rec.Code)
		}
	}
}

func TestAFileThatIsNotThereIsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	webui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nothing.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d", rec.Code)
	}
}

var reference = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// A page that names a file the binary does not carry is blank, and nothing
// says why.
func TestEveryFileThePageNamesIsEmbedded(t *testing.T) {
	index, err := fs.ReadFile(webui.Assets(), "index.html")
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}

	found := reference.FindAllStringSubmatch(string(index), -1)
	if len(found) == 0 {
		t.Fatal("the page names no files")
	}

	for _, match := range found {
		name := match[1]

		// The policy forbids loading from anywhere else; this is where a page
		// that tried would be caught before it is a blank screen.
		if strings.Contains(name, "://") || strings.HasPrefix(name, "//") {
			t.Errorf("the page loads %s from somewhere else", name)
			continue
		}
		if _, err := fs.Stat(webui.Assets(), name); err != nil {
			t.Errorf("the page names %s, which is not embedded", name)
		}
	}
}

// ── the schema ─────────────────────────────────────────────────────────

type option struct {
	V string `json:"v"`
}

type field struct {
	Key     string   `json:"key"`
	Zh      string   `json:"zh"`
	Type    string   `json:"type"`
	Def     any      `json:"def"`
	Help    string   `json:"help"`
	Options []option `json:"options"`
	Item    *struct {
		Title  string  `json:"title"`
		Fields []field `json:"fields"`
	} `json:"item"`
}

type block struct {
	When *struct {
		Key string `json:"key"`
		Eq  string `json:"eq"`
	} `json:"when"`
	Fields []field `json:"fields"`
}

type schema struct {
	Sections []struct {
		ID     string  `json:"id"`
		Blocks []block `json:"blocks"`
	} `json:"sections"`
}

func loadSchema(t *testing.T) schema {
	t.Helper()

	file, err := webui.Assets().Open("schema.js")
	if err != nil {
		t.Fatalf("open the schema: %v", err)
	}
	defer func() { _ = file.Close() }()

	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}

	// The file is JSON assigned to a name, so the page can read it without
	// fetching it, and so a test can read it without running a browser.
	text := string(raw)
	_, body, found := strings.Cut(text, "window.SCHEMA = ")
	if !found {
		t.Fatal("the schema does not assign window.SCHEMA")
	}

	var loaded schema
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), ";"))), &loaded); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	return loaded
}

// harnessOnly are the settings the example deliberately does not show, because
// they drive the offline provider the end-to-end checks use. A page that
// offered them would teach the wrong thing for the same reason the example
// does not. Kept the same as config_test.go's list, which is where it is
// argued.
var harnessOnly = map[string]bool{
	"provider.fake_model":     true,
	"provider.fake_delay":     true,
	"provider.fake_reasoning": true,
	"provider.fake_script":    true,
}

func (s schema) fields() map[string]field {
	out := make(map[string]field)
	for _, section := range s.Sections {
		for _, b := range section.Blocks {
			for _, f := range b.Fields {
				out[f.Key] = f
			}
		}
	}
	return out
}

// kindOf is the type a schema field has to carry for a setting of this kind.
func kindOf(kind config.Kind) []string {
	if kind == config.KindString {
		return []string{"string", "enum"}
	}
	return []string{string(kind)}
}

// The page is built from this file, so a setting it does not list is one the
// page can never offer — and said nowhere. The settings come from the struct,
// the same walk that checks the example documents them.
func TestTheSchemaCoversEverySettingAndOnlyTheseSettings(t *testing.T) {
	declared := loadSchema(t).fields()

	known := make(map[string]config.Setting)
	for _, setting := range config.Settings() {
		known[setting.Key] = setting

		if harnessOnly[setting.Key] {
			if _, offered := declared[setting.Key]; offered {
				t.Errorf("%s is for the test double and should not be offered", setting.Key)
			}
			continue
		}

		f, ok := declared[setting.Key]
		if !ok {
			t.Errorf("the schema does not describe %s", setting.Key)
			continue
		}

		if !contains(kindOf(setting.Kind), f.Type) {
			t.Errorf("%s is a %s in the program and a %s in the schema", setting.Key, setting.Kind, f.Type)
		}
		if f.Zh == "" || f.Help == "" {
			t.Errorf("%s has no name or no explanation", setting.Key)
		}

		if setting.Kind == config.KindList {
			if f.Item == nil {
				t.Errorf("%s is a list and the schema does not say what an element is", setting.Key)
				continue
			}
			checkElement(t, setting, *f.Item)
			continue
		}
		if !sameJSON(setting.Default, f.Def) {
			t.Errorf("%s defaults to %v in the program and %v in the schema", setting.Key, setting.Default, f.Def)
		}
	}

	for key := range declared {
		if _, ok := known[key]; !ok {
			t.Errorf("the schema describes %s, which is not a setting", key)
		}
	}
}

func checkElement(t *testing.T, list config.Setting, item struct {
	Title  string  `json:"title"`
	Fields []field `json:"fields"`
}) {
	t.Helper()

	inner := make(map[string]field, len(item.Fields))
	for _, f := range item.Fields {
		inner[f.Key] = f
	}

	for _, setting := range list.Items {
		f, ok := inner[setting.Key]
		if !ok {
			t.Errorf("%s: the schema does not describe %s of an element", list.Key, setting.Key)
			continue
		}
		if !contains(kindOf(setting.Kind), f.Type) {
			t.Errorf("%s[].%s is a %s in the program and a %s in the schema", list.Key, setting.Key, setting.Kind, f.Type)
		}
		if !sameJSON(setting.Default, f.Def) {
			t.Errorf("%s[].%s defaults to %v in the program and %v in the schema", list.Key, setting.Key, setting.Default, f.Def)
		}
		delete(inner, setting.Key)
	}
	for key := range inner {
		t.Errorf("%s: the schema describes %s of an element, which is not a setting", list.Key, key)
	}
	if _, ok := inner[item.Title]; !ok && item.Title != "" {
		found := false
		for _, setting := range list.Items {
			found = found || setting.Key == item.Title
		}
		if !found {
			t.Errorf("%s names %s as the title of an element, which it does not have", list.Key, item.Title)
		}
	}
}

// A block that waits on a setting that is not there is shown never, or always.
func TestEveryBlockThatWaitsOnASettingWaitsOnOneThatExists(t *testing.T) {
	loaded := loadSchema(t)
	declared := loaded.fields()

	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			if b.When == nil {
				continue
			}
			if _, ok := declared[b.When.Key]; !ok {
				t.Errorf("a block in %s waits on %s, which is not described", section.ID, b.When.Key)
			}
		}
	}
}

// Every choice the page offers has to be one the daemon accepts. A dropdown
// with an option that fails validation is a page that invites its own error.
func TestEveryChoiceThePageOffersIsAcceptedByTheDaemon(t *testing.T) {
	loaded := loadSchema(t)

	// The settings a choice only means something under, so the check that is
	// being asked of it is actually made.
	gates := make(map[string]config.Edit)
	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			if b.When != nil {
				for _, f := range b.Fields {
					gates[f.Key] = config.Edit{Key: b.When.Key, Value: b.When.Eq}
				}
			}
		}
	}

	var checked int
	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			for _, f := range b.Fields {
				if f.Type != "enum" {
					continue
				}
				for _, o := range f.Options {
					edits := []config.Edit{{Key: f.Key, Value: o.V}}
					if gate, ok := gates[f.Key]; ok && gate.Key != f.Key {
						edits = append(edits, gate)
					}

					_, problems, err := config.Preview([]byte(config.Example), edits)
					if err != nil {
						t.Errorf("%s = %q could not be written: %v", f.Key, o.V, err)
						continue
					}
					for _, problem := range problems {
						if problem.Key == f.Key {
							t.Errorf("%s = %q is offered, and the daemon refuses it: %s", f.Key, o.V, problem.Why)
						}
					}
					checked++
				}
			}
		}
	}
	if checked < 20 {
		t.Errorf("only %d choices were checked", checked)
	}

	// The same for a choice inside a list's element.
	for _, section := range loaded.Sections {
		for _, b := range section.Blocks {
			for _, f := range b.Fields {
				if f.Item == nil {
					continue
				}
				for _, inner := range f.Item.Fields {
					if inner.Type != "enum" {
						continue
					}
					for _, o := range inner.Options {
						element := map[string]any{"name": "x", "command": "x", inner.Key: o.V}
						if f.Key != "mcp.servers" {
							continue
						}
						_, problems, err := config.Preview([]byte(config.Example), []config.Edit{{Key: f.Key, Value: []any{element}}})
						if err != nil {
							t.Errorf("%s[].%s = %q could not be written: %v", f.Key, inner.Key, o.V, err)
							continue
						}
						for _, problem := range problems {
							if strings.HasSuffix(problem.Key, "."+inner.Key) {
								t.Errorf("%s[].%s = %q is offered, and the daemon refuses it: %s", f.Key, inner.Key, o.V, problem.Why)
							}
						}
					}
				}
			}
		}
	}
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// sameJSON compares as JSON does, so 12 and 12.0 are the same default and an
// absent list is an empty one.
func sameJSON(a, b any) bool {
	normal := func(v any) any {
		if v == nil {
			return []any{}
		}
		raw, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	return reflect.DeepEqual(normal(a), normal(b))
}
