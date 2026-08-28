package gateway

import "testing"

// Naming what a call is doing is best-effort, and best-effort has to include
// the effort failing without taking the line down with it: a status line that
// disappears because a tool used arguments nobody anticipated is worse than a
// vague one.
func TestACallWithNothingWorthNamingStillSaysSomething(t *testing.T) {
	for arguments, want := range map[string]string{
		`{"path":"a.txt"}`:                 "read_file a.txt",
		`{"program":"go","args":["test"]}`: "read_file go",
		`{"unknown":"a.txt"}`:              "read_file",
		`{"path":""}`:                      "read_file",
		`{"path":42}`:                      "read_file",
		`not json at all`:                  "read_file",
		``:                                 "read_file",
	} {
		if got := describeCall("read_file", arguments); got != want {
			t.Errorf("%q described as %q, want %q", arguments, got, want)
		}
	}
}
