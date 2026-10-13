package config

import "fmt"

// BootstrapKey is a setting that cannot be kept with the others, because it
// says where they are kept.
//
// The settings are a document in the daemon's database. server.data_dir says
// where the database is, and server.runtime_dir where the file clients find the
// daemon by is, so neither can be read out of the database: the daemon would
// need the answer to find the question. They are given when the daemon starts,
// by a flag or a variable, and a line for one in the saved text is ignored.
type BootstrapKey struct {
	Key string

	// Env is the variable that gives it, and Flag the flag that does, empty for
	// a setting that has none.
	Env  string
	Flag string
}

// BootstrapKeys are the settings the saved text cannot hold.
func BootstrapKeys() []BootstrapKey {
	return []BootstrapKey{
		{Key: "server.data_dir", Env: "JINGCLAW_SERVER_DATA_DIR", Flag: "--data-dir"},
		{Key: "server.runtime_dir", Env: "JINGCLAW_SERVER_RUNTIME_DIR"},
	}
}

// problem is what is said to somebody who gave this key a value in the text.
func (k BootstrapKey) problem(value string) Problem {
	given := k.Env
	if k.Flag != "" {
		given = k.Flag + " or " + k.Env
	}

	return Problem{
		Key: k.Key, Value: value,
		Why: "says where the settings are kept, so it cannot be one of them",
		Fix: "Give it as " + given + " when the daemon starts. A line for it in the settings is ignored.",
	}
}

// BootstrapProblem says why an edit cannot be made, and false for one that can.
func BootstrapProblem(edit Edit) (Problem, bool) {
	for _, bootstrap := range BootstrapKeys() {
		if bootstrap.Key != edit.Key {
			continue
		}

		var value string
		if !edit.Remove {
			value = quote(fmt.Sprint(edit.Value))
		}
		return bootstrap.problem(value), true
	}
	return Problem{}, false
}

// BootstrapSettings are the bootstrap keys a text sets, with the values it
// gives them.
//
// A text may carry them: an older version kept them with the rest, and a first
// start imports what it finds. They are never obeyed, and what the daemon does
// about a text that disagrees with where it is running is for the daemon to
// say, so this only reports what the text says.
func BootstrapSettings(content []byte) (map[string]string, error) {
	known := make(map[string]Setting)
	for _, setting := range Settings() {
		known[setting.Key] = setting
	}

	found, err := settingsInFile(content, known)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string)
	for _, bootstrap := range BootstrapKeys() {
		if value, set := found[bootstrap.Key]; set {
			out[bootstrap.Key] = fmt.Sprint(value)
		}
	}
	return out, nil
}

// BootstrapProblems are the bootstrap keys a text sets, as problems: what is
// said to somebody who is saving that text.
func BootstrapProblems(content []byte) ([]Problem, error) {
	set, err := BootstrapSettings(content)
	if err != nil {
		return nil, err
	}

	var problems []Problem
	for _, bootstrap := range BootstrapKeys() {
		if value, ok := set[bootstrap.Key]; ok {
			problems = append(problems, bootstrap.problem(quote(value)))
		}
	}
	return problems, nil
}

// isBootstrap says whether key is one of them.
func isBootstrap(key string) bool {
	for _, bootstrap := range BootstrapKeys() {
		if bootstrap.Key == key {
			return true
		}
	}
	return false
}
