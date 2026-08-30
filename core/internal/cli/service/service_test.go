package service

import (
	"os/exec"
	"strings"
	"testing"
)

func aJob() job {
	return job{
		Executable: "/usr/local/bin/jingclaw",
		Home:       "/Users/someone/.jingclaw",
		Output:     "/Users/someone/.jingclaw/log/jingclaw.out",
		Error:      "/Users/someone/.jingclaw/log/jingclaw.err",
		Path:       "/opt/homebrew/bin:/usr/bin",
	}
}

// launchd reads this file, and a file it cannot parse is a service that
// silently never starts.
func TestThePlistIsWellFormed(t *testing.T) {
	written := aJob().plist()

	command := exec.Command("plutil", "-lint", "-")
	command.Stdin = strings.NewReader(written)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("plutil rejected the plist: %v\n%s\n%s", err, out, written)
	}
}

// The service runs with the environment written here rather than the one the
// terminal had, so anything left out is missing at run time and nowhere else.
func TestThePlistCarriesWhatTheServiceNeeds(t *testing.T) {
	written := aJob().plist()

	for _, wanted := range []string{
		"<string>" + label + "</string>",
		"<string>/usr/local/bin/jingclaw</string>",
		"<key>JINGCLAW_HOME</key>",
		"<string>/Users/someone/.jingclaw</string>",
		"<key>PATH</key>",
		"<string>/opt/homebrew/bin:/usr/bin</string>",
		"<string>/Users/someone/.jingclaw/log/jingclaw.out</string>",
		"<string>/Users/someone/.jingclaw/log/jingclaw.err</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(written, wanted) {
			t.Errorf("the plist does not contain %s:\n%s", wanted, written)
		}
	}
}

// A home directory is allowed to have an ampersand in it, and a plist that
// stops parsing at one is a service that never runs for that person alone.
func TestAPathWithMarkupInItDoesNotBreakThePlist(t *testing.T) {
	awkward := aJob()
	awkward.Home = "/Users/tom & jerry/.jingclaw"

	written := awkward.plist()
	if strings.Contains(written, "jerry/.jingclaw</string>") == false {
		t.Fatalf("the home directory is not in the plist:\n%s", written)
	}
	if strings.Contains(written, "tom & jerry") {
		t.Errorf("the ampersand was written raw:\n%s", written)
	}

	command := exec.Command("plutil", "-lint", "-")
	command.Stdin = strings.NewReader(written)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("plutil rejected the plist: %v\n%s", err, out)
	}
}
