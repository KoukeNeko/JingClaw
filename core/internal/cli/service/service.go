// Package service installs JingClaw as something the machine keeps running.
//
// Running it from a terminal is the normal way to use it, and closing that
// terminal is then the normal way to stop it. A service is for the other
// case: nobody is at the keyboard, the laptop woke up on its own, and the
// question is whether anyone answers in chat.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// label names the job to launchd. Reverse domain because that is the
// convention launchd sorts and reports by, and a name that collides with
// somebody else's job is a name that uninstalls theirs.
const label = "dev.jingclaw.agent"

// Names of what the service writes when there is no terminal to write to.
const (
	outputName = "jingclaw.out"
	errorName  = "jingclaw.err"
)

// darwin is the only platform with an implementation so far.
const darwin = "darwin"

// Commands are the subcommands for installing and removing the service.
func Commands() []*cobra.Command {
	var showOnly bool

	installCommand := &cobra.Command{
		Use:   "install",
		Short: "Keep JingClaw running, including across logins",
		RunE:  func(*cobra.Command, []string) error { return install(showOnly) },
	}
	installCommand.Flags().BoolVar(&showOnly, "print", false,
		"print what would be installed and change nothing")

	service := &cobra.Command{
		Use:   "service",
		Short: "Install JingClaw as a background service",
	}
	service.AddCommand(
		installCommand,
		&cobra.Command{
			Use:   "uninstall",
			Short: "Stop keeping JingClaw running",
			RunE:  func(*cobra.Command, []string) error { return uninstall() },
		},
		&cobra.Command{
			Use:   "status",
			Short: "Say whether the service is installed and loaded",
			RunE:  func(*cobra.Command, []string) error { return status() },
		},
	)
	return []*cobra.Command{service}
}

// job is everything the plist needs, gathered before any of it is written.
//
// Gathered first so that a machine which cannot be described — no home
// directory, no executable path — fails before it has changed anything.
type job struct {
	// Source is the program this command was run from; Executable is the copy
	// of it the service runs, under the home directory.
	Source     string
	Executable string
	Home       string
	Output     string
	Error      string
	Path       string
}

// binaryName is what the service's copy of the program is called.
const binaryName = "jingclaw"

func describe() (job, error) {
	if runtime.GOOS != darwin {
		return job{}, fmt.Errorf("service: only implemented for macOS, not %s", runtime.GOOS)
	}

	dir, found := home.Resolve()
	if !found {
		return job{}, fmt.Errorf("service: %s is set to none, so there is nothing to run", home.EnvVar)
	}

	// The path of this file, not the name on PATH. A machine commonly has an
	// installed copy and a freshly built one, and a service that quietly runs
	// the other one is the hardest kind of wrong to see.
	source, err := os.Executable()
	if err != nil {
		return job{}, fmt.Errorf("service: find this program: %w", err)
	}

	// The service runs a copy under the home directory, never this file
	// where it stands. launchd cannot open a program inside a folder macOS
	// protects, and a checkout is usually in one: the service hangs in the
	// loader before main, with nothing written anywhere to say why. That took
	// a deployment down for days once and for hours a second time.
	return job{
		Source:     source,
		Executable: filepath.Join(dir.Bin(), binaryName),
		Home:       dir.Root,
		Output:     filepath.Join(dir.Log(), outputName),
		Error:      filepath.Join(dir.Log(), errorName),
		Path:       searchPath(),
	}, nil
}

// pathTimeout is how long the login shell gets to say what PATH it sets.
const pathTimeout = 5 * time.Second

// searchPath is the PATH a service should run with.
//
// Asked of the login shell rather than taken from this process. The shell a
// person opens is where they installed their tools, and this process may be
// something else entirely — an editor, another agent — carrying a PATH full
// of directories that will not exist tomorrow. Baking one of those into a
// service produces a tool that is found today and missing next week, with
// nothing to connect the two.
//
// The shell failing is not worth refusing to install over: what this process
// has is a worse answer, not no answer.
func searchPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return os.Getenv("PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), pathTimeout)
	defer cancel()

	// Login and interactive both, because that is the pair a terminal window
	// is. A login shell alone reads .zprofile but not .zshrc, and .zshrc is
	// where a PATH is most often extended — the tools it adds would be the
	// ones missing from the service and from nowhere else.
	asking := exec.CommandContext(ctx, shell, "-l", "-i", "-c", `printf %s "$PATH"`)
	asking.Env = minimalEnvironment()

	said, err := asking.Output()
	if answered := strings.TrimSpace(string(said)); err == nil && answered != "" {
		return answered
	}
	return os.Getenv("PATH")
}

// minimalEnvironment is what the login shell is given to start from.
//
// Deliberately without this process's PATH. A profile almost always builds on
// the PATH it was handed rather than replacing it, so passing ours through
// would come back with ours still in it — and the answer we want is what a
// fresh terminal has, not what this process happens to be carrying.
func minimalEnvironment() []string {
	kept := []string{"TERM=dumb"}
	for _, name := range []string{"HOME", "USER", "LOGNAME", "SHELL", "LANG"} {
		if value := os.Getenv(name); value != "" {
			kept = append(kept, name+"="+value)
		}
	}
	return kept
}

// plist is the job as launchd wants to read it.
//
// The two environment variables are written out rather than inherited on
// purpose. A service does not get the environment of the shell that installed
// it: PATH would be the short system one, and every tool the agent runs by
// name would stop being found. Recording them here is what makes the service
// behave the way the terminal did.
func (j job) plist() string {
	var out strings.Builder
	out.WriteString(xmlHeader)
	fmt.Fprintf(&out, "\t<key>Label</key>\n\t<string>%s</string>\n", escape(label))
	out.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	fmt.Fprintf(&out, "\t\t<string>%s</string>\n", escape(j.Executable))
	out.WriteString("\t</array>\n")
	out.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	fmt.Fprintf(&out, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", home.EnvVar, escape(j.Home))
	fmt.Fprintf(&out, "\t\t<key>PATH</key>\n\t\t<string>%s</string>\n", escape(j.Path))
	out.WriteString("\t</dict>\n")
	fmt.Fprintf(&out, "\t<key>WorkingDirectory</key>\n\t<string>%s</string>\n", escape(j.Home))
	fmt.Fprintf(&out, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", escape(j.Output))
	fmt.Fprintf(&out, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", escape(j.Error))
	out.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	out.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	out.WriteString("</dict>\n</plist>\n")
	return out.String()
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`

// escape makes a string safe to sit between plist tags.
//
// Paths can contain an ampersand, and a home directory named "Tom & Jerry"
// producing a plist that launchd refuses to parse is a failure nobody would
// connect to the cause.
func escape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

// plistPath is where launchd looks for a job belonging to one user.
func plistPath() (string, error) {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: find your home directory: %w", err)
	}
	return filepath.Join(dir, "Library", "LaunchAgents", label+".plist"), nil
}

// target names the job to launchctl once it is loaded.
func target() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
}

// domain names the session a job is loaded into.
func loginSession() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func install(showOnly bool) error {
	described, err := describe()
	if err != nil {
		return err
	}

	if showOnly {
		fmt.Print(described.plist())
		return nil
	}

	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("service: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(described.Output), 0o700); err != nil {
		return fmt.Errorf("service: create the log directory: %w", err)
	}

	copied, err := stage(described)
	if err != nil {
		return err
	}

	// Unloaded first. Writing over the file of a loaded job leaves launchd
	// running the previous one until the next login, which looks exactly like
	// the install having done nothing.
	if loaded() {
		if err := bootout(); err != nil {
			return err
		}
	}

	if err := os.WriteFile(path, []byte(described.plist()), 0o644); err != nil {
		return fmt.Errorf("service: write %s: %w", path, err)
	}
	if err := launchctl("bootstrap", loginSession(), path); err != nil {
		return err
	}

	fmt.Printf("installed %s\n", path)
	if copied {
		fmt.Printf("copied    %s\n          -> %s\n", described.Source, described.Executable)
	}
	fmt.Printf("running   %s\n", described.Executable)
	fmt.Printf("logging   %s\n", filepath.Dir(described.Output))
	return nil
}

// stage puts the copy of the program in place, and says whether it had to.
//
// Nothing to do when the program is already the copy — installing from the
// copy itself is how a service is repaired when the checkout has gone. The
// copy is written beside its final name and renamed into place, so a service
// that starts halfway through never sees half a program.
func stage(described job) (bool, error) {
	if sameFile(described.Source, described.Executable) {
		return false, nil
	}

	program, err := os.ReadFile(described.Source)
	if err != nil {
		return false, fmt.Errorf("service: read this program: %w", err)
	}

	bin := filepath.Dir(described.Executable)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return false, fmt.Errorf("service: create %s: %w", bin, err)
	}

	staging := described.Executable + ".installing"
	if err := os.WriteFile(staging, program, 0o755); err != nil {
		return false, fmt.Errorf("service: write %s: %w", staging, err)
	}
	if err := os.Rename(staging, described.Executable); err != nil {
		_ = os.Remove(staging)
		return false, fmt.Errorf("service: put the program in place: %w", err)
	}
	return true, nil
}

// sameFile reports whether two paths are one file, however they are spelled.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	first, err := os.Stat(a)
	if err != nil {
		return false
	}
	second, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(first, second)
}

// protectedFolders are the ones macOS gates behind a permission a background
// service has no way to ask for.
var protectedFolders = []string{"Documents", "Desktop", "Downloads"}

// underProtectedFolder reports a path launchd will not be able to open.
func underProtectedFolder(home, path string) bool {
	if home == "" || path == "" {
		return false
	}
	for _, folder := range protectedFolders {
		root := filepath.Join(home, folder)
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// installedProgram reads which program the plist on disk actually names.
//
// From the file rather than from describe(), because status is about what is
// installed, and the two come apart exactly when somebody needs to know.
func installedProgram(plist string) (string, bool) {
	_, after, found := strings.Cut(plist, "<key>ProgramArguments</key>")
	if !found {
		return "", false
	}
	_, after, found = strings.Cut(after, "<string>")
	if !found {
		return "", false
	}
	program, _, found := strings.Cut(after, "</string>")
	if !found {
		return "", false
	}
	return strings.TrimSpace(program), true
}

func uninstall() error {
	path, err := plistPath()
	if err != nil {
		return err
	}

	// Both are reported, because they come apart: a file removed by hand
	// leaves a job launchd is still running, and saying "not installed" after
	// stopping one would describe the state before this command rather than
	// after it.
	unloaded := false
	if loaded() {
		if err := bootout(); err != nil {
			return err
		}
		unloaded = true
	}

	removed := true
	if err := os.Remove(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("service: remove %s: %w", path, err)
		}
		removed = false
	}

	switch {
	case removed:
		fmt.Printf("removed %s\n", path)
	case unloaded:
		fmt.Printf("stopped the running service; there was no %s to remove\n", path)
	default:
		fmt.Println("not installed")
	}
	return nil
}

func status() error {
	path, err := plistPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("not installed")
		return nil
	} else if err != nil {
		return fmt.Errorf("service: read %s: %w", path, err)
	}

	fmt.Printf("installed %s\n", path)
	if loaded() {
		fmt.Println("loaded    yes")
	} else {
		fmt.Println("loaded    no (it will load at your next login)")
	}

	written, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("service: read %s: %w", path, err)
	}
	program, found := installedProgram(string(written))
	if !found {
		return nil
	}
	fmt.Printf("running   %s\n", program)

	// Said here because nothing else will say it. A service pointed into one
	// of these folders hangs in the loader before main, and launchd reports it
	// as running.
	userHome, _ := os.UserHomeDir()
	if underProtectedFolder(userHome, program) {
		fmt.Println("warning   that is inside a folder macOS protects; launchd cannot open it,")
		fmt.Println("          and the service will hang before it starts, looking like it is running.")
		fmt.Println("          Run `jingclaw service install` from a build to copy it somewhere it can.")
	}
	return nil
}

// loaded asks launchd whether it currently knows the job.
//
// Its absence is an answer rather than a failure, so the exit status is read
// and the error is not passed on: "launchctl said no" and "launchctl is
// broken" would otherwise reach the operator as the same sentence.
func loaded() bool {
	return exec.Command("launchctl", "print", target()).Run() == nil
}

func bootout() error {
	return launchctl("bootout", target())
}

// launchctl runs one launchctl subcommand and turns its noise into a sentence.
func launchctl(args ...string) error {
	command := exec.Command("launchctl", args...)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}

	said := strings.TrimSpace(string(output))
	if said == "" {
		said = "no output"
	}
	return fmt.Errorf("service: launchctl %s: %w: %s",
		strings.Join(args, " "), err, said)
}
