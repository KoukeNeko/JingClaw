package config

import (
	"reflect"
	"strings"
	"time"
)

// Kind is how a setting is written in the file.
type Kind string

const (
	KindBool     Kind = "bool"
	KindInt      Kind = "int"
	KindFloat    Kind = "float"
	KindString   Kind = "string"
	KindDuration Kind = "duration"
	KindStrings  Kind = "strings"
	KindMap      Kind = "map"
	KindList     Kind = "list"
)

// Setting is one key of the configuration, read from the struct that declares
// it rather than written out a second time.
//
// A client that edits settings needs the list of them, and a hand-kept list is
// the one that goes stale when a field is added. The struct is the only place
// that cannot.
type Setting struct {
	// Key is the dotted name an operator writes: provider.retry.max_attempts.
	// Inside a list's Items it is the short name within one element.
	Key     string
	Kind    Kind
	Default any

	// Items are the settings of one element of a list of tables.
	Items []Setting
}

var durationType = reflect.TypeFor[time.Duration]()

// Settings lists every setting, with its kind and its default.
func Settings() []Setting {
	return settingsOf(reflect.ValueOf(Defaults()), "")
}

// Flatten reports every setting of cfg under its dotted key.
//
// Values come out as plain JSON: numbers, strings, booleans, lists and maps,
// and durations as the text an operator would write. That is what a client is
// shown and what it sends back, and it means this never has to say which Go
// type a value used to be.
func Flatten(cfg Config) map[string]any {
	flat := make(map[string]any)
	for _, setting := range settingsOf(reflect.ValueOf(cfg), "") {
		flat[setting.Key] = setting.Default
	}
	return flat
}

func settingsOf(v reflect.Value, prefix string) []Setting {
	var out []Setting
	t := v.Type()

	for i := range t.NumField() {
		field := t.Field(i)

		tag := field.Tag.Get("koanf")
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}

		value := v.Field(i)
		switch {
		case field.Type != durationType && field.Type.Kind() == reflect.Struct:
			// A section is not a setting; what is inside it is.
			out = append(out, settingsOf(value, key)...)

		case field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Struct:
			items := make([]any, value.Len())
			for n := range value.Len() {
				element := make(map[string]any)
				for _, inner := range settingsOf(value.Index(n), "") {
					element[inner.Key] = inner.Default
				}
				items[n] = element
			}
			out = append(out, Setting{
				Key:     key,
				Kind:    KindList,
				Default: items,
				// The shape of an element, taken from a zero one.
				Items: settingsOf(reflect.New(field.Type.Elem()).Elem(), ""),
			})

		default:
			kind, plain := plainValue(value)
			out = append(out, Setting{Key: key, Kind: kind, Default: plain})
		}
	}

	return out
}

func plainValue(v reflect.Value) (Kind, any) {
	if v.Type() == durationType {
		return KindDuration, formatDuration(time.Duration(v.Int()))
	}

	switch v.Kind() {
	case reflect.Bool:
		return KindBool, v.Bool()
	case reflect.String:
		return KindString, v.String()
	case reflect.Int, reflect.Int32, reflect.Int64:
		return KindInt, v.Int()
	case reflect.Float64:
		return KindFloat, v.Float()
	case reflect.Slice:
		// Never nil: an empty list and a missing one are the same setting, and
		// a client should not have to know which it was.
		list := make([]any, v.Len())
		for i := range list {
			list[i] = v.Index(i).String()
		}
		return KindStrings, list
	case reflect.Map:
		table := make(map[string]any, v.Len())
		for _, name := range v.MapKeys() {
			table[name.String()] = v.MapIndex(name).String()
		}
		return KindMap, table
	}

	// A field type this does not know is a new kind of setting. Failing here
	// is the point: the test that walks every setting finds it the moment it
	// is added, instead of a client quietly never offering it.
	panic("config: no plain form for " + v.Type().String())
}

// formatDuration writes a duration the way a configuration file does.
//
// Go prints two minutes as "2m0s". Nobody writes that, and a client comparing
// what the file says with what is running would see two different values for
// one.
func formatDuration(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

// FileSettings reads which settings a file sets, by dotted key, written the
// way Flatten writes them.
//
// The same form, so that what a file says and what is running can be compared
// without "120s" and "2m" looking like two different values. Keys the program
// does not know are left out: they change nothing, and offering them for
// editing would be offering a setting that is not there.
func FileSettings(content []byte) (map[string]any, error) {
	known := make(map[string]Setting)
	for _, setting := range Settings() {
		known[setting.Key] = setting
	}

	found, err := settingsInFile(content, known)
	if err != nil {
		return nil, err
	}

	out := make(map[string]any, len(found))
	for key, value := range found {
		setting, ok := known[key]
		if !ok {
			continue
		}

		if text, isText := value.(string); isText && setting.Kind == KindDuration {
			if parsed, err := time.ParseDuration(text); err == nil {
				value = formatDuration(parsed)
			}
		}
		out[key] = value
	}
	return out, nil
}

// EnvironmentOverrides says which settings a variable in this environment
// overrides, and which variable it is.
//
// These are the ones the file cannot change: whatever the file says, the
// variable wins, and a client that offered to edit them would be offering an
// edit that does nothing.
func EnvironmentOverrides(lookup func(string) (string, bool)) map[string]string {
	out := make(map[string]string)
	for _, setting := range Settings() {
		if name, ok := EnvironmentName(setting.Key); ok {
			if _, set := lookup(name); set {
				out[setting.Key] = name
			}
		}
	}
	return out
}
