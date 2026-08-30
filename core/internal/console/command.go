package console

import (
	"fmt"
	"sort"
	"strings"
)

// Command is something typed at the console.
//
// Its own parser rather than the one a chat channel uses. That one refuses
// anything longer than two words, because a channel is also where people talk
// and a sentence must not be swallowed as an instruction. Nothing is said to
// the agent here, so the rule does not apply — and inheriting it would mean a
// console that could not take an argument with a space in it.
type Command struct {
	Verb string
	Args []string
}

// Rest is everything after the first argument, for a command whose argument
// is a sentence.
func (c Command) Rest() string { return strings.Join(c.Args[1:], " ") }

// Arg is the nth argument, or "" if it was not given.
func (c Command) Arg(n int) string {
	if n < len(c.Args) {
		return c.Args[n]
	}
	return ""
}

// known is every verb, its aliases, and what it is for.
//
// One table rather than a switch, so that help, completion and the parser
// cannot disagree about what exists — the way to end up with a command that
// works and is undocumented, or documented and removed.
var known = []struct {
	Verb    string
	Aliases []string
	Takes   string
	What    string
}{
	{Verb: "approvals", Aliases: []string{"pending"}, What: "what is waiting for a decision"},
	{Verb: "approve", Aliases: []string{"allow", "yes"}, Takes: "<id>", What: "allow a waiting call"},
	{Verb: "deny", Aliases: []string{"refuse", "no"}, Takes: "<id>", What: "refuse a waiting call"},
	{Verb: "questions", Aliases: []string{"asked"}, What: "what the agent has stopped to ask"},
	{Verb: "answer", Takes: "<id> <text>", What: "answer a question it is waiting on"},
	{Verb: "sessions", Aliases: []string{"ls"}, What: "the conversations there are"},
	{Verb: "processes", Aliases: []string{"ps"}, What: "programs the agent has running"},
	{Verb: "focus", Takes: "[session]", What: "show one session only, or all of them again"},
	{Verb: "interrupt", Takes: "<session>", What: "ask a run to stop"},
	{Verb: "help", Aliases: []string{"?"}, What: "this"},
	{Verb: "quit", Aliases: []string{"exit"}, What: "leave the console; the agent keeps running"},
	{Verb: "stop", What: "stop JingClaw"},
}

// verbs maps every name and alias to the verb it means.
var verbs = func() map[string]string {
	all := make(map[string]string, len(known)*2)
	for _, command := range known {
		all[command.Verb] = command.Verb
		for _, alias := range command.Aliases {
			all[alias] = command.Verb
		}
	}
	return all
}()

// ErrUnknown says a line was not one of the commands.
type ErrUnknown struct{ Typed string }

func (e ErrUnknown) Error() string {
	return fmt.Sprintf("there is no %q; type help to see what there is", e.Typed)
}

// Parse reads a typed line.
//
// An empty line is not an error and not a command; it is somebody pressing
// return.
func Parse(line string) (Command, bool, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return Command{}, false, nil
	}

	verb, known := verbs[strings.ToLower(fields[0])]
	if !known {
		return Command{}, false, ErrUnknown{Typed: fields[0]}
	}
	return Command{Verb: verb, Args: fields[1:]}, true, nil
}

// Help is the list of commands, for somebody who typed help.
func Help() string {
	widest := 0
	for _, command := range known {
		if width := len(command.Verb) + len(command.Takes) + 1; width > widest {
			widest = width
		}
	}

	lines := make([]string, 0, len(known))
	for _, command := range known {
		shown := command.Verb
		if command.Takes != "" {
			shown += " " + command.Takes
		}
		lines = append(lines, fmt.Sprintf("  %-*s  %s", widest, shown, command.What))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
