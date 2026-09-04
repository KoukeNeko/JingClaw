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

// A tool from a server names its arguments however it likes, and the working
// line is posted to a room other people read. The keys that mean "the thing
// being worked on" are shown; anything else — which may be a credential — is
// not, whatever it is called.
func TestACallFromAServerShowsItsSubjectAndNeverItsSecrets(t *testing.T) {
	for arguments, want := range map[string]string{
		`{"explain":true,"text":"報告江董，小弟已經復活"}`: "mcp_zhtw_zhtw 報告江董，小弟已經復活",
		`{"content":"a paragraph"}`:             "mcp_zhtw_zhtw a paragraph",
		`{"api_key":"sk-live-do-not-print"}`:    "mcp_zhtw_zhtw",
		`{"token":"abc","explain":true}`:        "mcp_zhtw_zhtw",
		`{"text":"   "}`:                        "mcp_zhtw_zhtw",
	} {
		if got := describeCall("mcp_zhtw_zhtw", arguments); got != want {
			t.Errorf("%q described as %q, want %q", arguments, got, want)
		}
	}
}
