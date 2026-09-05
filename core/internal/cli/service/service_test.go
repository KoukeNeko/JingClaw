package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// plutil is macOS's own, and this file is only ever read by launchd,
	// which is also macOS's own. Elsewhere there is nothing to check it with
	// and nothing that would read it — said as a skip rather than passed
	// quietly, so a run that could not check this does not look like one that
	// did.
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("no plutil here, and it is what launchd's own parser is")
	}

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
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("no plutil here, and it is what launchd's own parser is")
	}

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

// launchd cannot open a program inside a folder macOS protects, and it does
// not fail: it hangs in the loader before main, with nothing written anywhere.
func TestABinaryUnderAProtectedFolderIsNoticed(t *testing.T) {
	home := "/Users/someone"
	for _, path := range []string{
		"/Users/someone/Documents/GitHub/JingClaw/core/bin/jingclaw",
		"/Users/someone/Desktop/jingclaw",
		"/Users/someone/Downloads/build/jingclaw",
	} {
		if !underProtectedFolder(home, path) {
			t.Errorf("%s was not recognised as somewhere launchd cannot open", path)
		}
	}
	for _, path := range []string{
		"/Users/someone/.jingclaw/bin/jingclaw",
		"/usr/local/bin/jingclaw",
		"/Users/someone/Documentsx/jingclaw",
	} {
		if underProtectedFolder(home, path) {
			t.Errorf("%s was called protected, which would warn about a path that works", path)
		}
	}
}

// The service runs a copy under the home directory, put in place whole.
func TestStagingCopiesTheProgramIntoHome(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "checkout", "jingclaw")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("#!/bin/sh\necho built\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	described := job{Source: source, Executable: filepath.Join(dir, "home", "bin", "jingclaw")}

	copied, err := stage(described)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !copied {
		t.Fatal("a program outside the home directory was not copied in")
	}

	installed, err := os.ReadFile(described.Executable)
	if err != nil {
		t.Fatalf("the copy is not there: %v", err)
	}
	if string(installed) != "#!/bin/sh\necho built\n" {
		t.Errorf("the copy differs from the source: %q", installed)
	}
	info, err := os.Stat(described.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the copy is not executable: %v", info.Mode())
	}
	if _, err := os.Stat(described.Executable + ".installing"); !errors.Is(err, os.ErrNotExist) {
		t.Error("the staging file was left behind")
	}
}

// Installing from the copy itself is how a service is repaired when the
// checkout is gone, and it must not try to copy a file onto itself.
func TestInstallingFromTheCopyItselfCopiesNothing(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "bin", "jingclaw")
	if err := os.MkdirAll(filepath.Dir(program), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	copied, err := stage(job{Source: program, Executable: program})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if copied {
		t.Error("the program was copied onto itself")
	}
}

// Status reports what is installed, which is read from the file rather than
// recomputed, because the two come apart exactly when somebody needs to know.
func TestStatusReadsTheProgramFromThePlist(t *testing.T) {
	program, found := installedProgram(aJob().plist())
	if !found {
		t.Fatal("the program was not found in the plist")
	}
	if program != "/usr/local/bin/jingclaw" {
		t.Errorf("read %q from the plist", program)
	}
	if _, found := installedProgram("<plist></plist>"); found {
		t.Error("a program was read from a plist that names none")
	}
}

// bootout returns before launchd has finished with the job. Installing waits
// for it to be gone rather than bootstrapping into a job still being torn
// down, which fails with an I/O error that names nothing.
func TestInstallWaitsForTheOldJobToBeGone(t *testing.T) {
	remaining := 3
	stillThere := func() bool {
		remaining--
		return remaining > 0
	}
	if !waitUntilGone(stillThere, time.Millisecond, time.Second) {
		t.Fatal("gave up on a job that was about to be gone")
	}

	forever := func() bool { return true }
	if waitUntilGone(forever, time.Millisecond, 20*time.Millisecond) {
		t.Fatal("claimed a job was gone that never went")
	}
}

// A bootstrap that fails once, while launchd is still catching up, is tried
// again; one that keeps failing is reported as it stands after the last try.
func TestBootstrapIsRetriedALittle(t *testing.T) {
	calls := 0
	flaky := func() error {
		calls++
		if calls < 3 {
			return errors.New("Bootstrap failed: 5: Input/output error")
		}
		return nil
	}
	if err := withRetries(5, time.Millisecond, flaky); err != nil {
		t.Fatalf("a bootstrap that succeeds on the third try was reported as failed: %v", err)
	}
	if calls != 3 {
		t.Errorf("tried %d times, want 3", calls)
	}

	calls = 0
	hopeless := func() error { calls++; return errors.New("still no") }
	if err := withRetries(3, time.Millisecond, hopeless); err == nil {
		t.Fatal("a bootstrap that never succeeds was reported as fine")
	}
	if calls != 3 {
		t.Errorf("tried %d times, want exactly the 3 allowed", calls)
	}
}
