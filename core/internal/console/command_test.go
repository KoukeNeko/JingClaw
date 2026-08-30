package console

import (
	"errors"
	"strings"
	"testing"
)

func TestAVerbAndItsArguments(t *testing.T) {
	command, ok, err := Parse("approve apr_01M174")
	if err != nil || !ok {
		t.Fatalf("approve was not understood: ok=%v err=%v", ok, err)
	}
	if command.Verb != "approve" {
		t.Errorf("the verb is %q", command.Verb)
	}
	if command.Arg(0) != "apr_01M174" {
		t.Errorf("the argument is %q", command.Arg(0))
	}
}

// An answer is a sentence. The chat channel's parser refuses anything longer
// than two words, and a console that inherited that could not take one.
func TestAnAnswerMayBeASentence(t *testing.T) {
	command, ok, err := Parse("answer qst_1 use the staging cluster, not production")
	if err != nil || !ok {
		t.Fatalf("a sentence was refused: ok=%v err=%v", ok, err)
	}
	if command.Verb != "answer" {
		t.Errorf("the verb is %q", command.Verb)
	}
	if command.Arg(0) != "qst_1" {
		t.Errorf("the question is %q", command.Arg(0))
	}
	if rest := command.Rest(); rest != "use the staging cluster, not production" {
		t.Errorf("the answer is %q", rest)
	}
}

func TestAnAliasMeansItsVerb(t *testing.T) {
	for typed, want := range map[string]string{
		"yes":     "approve",
		"allow":   "approve",
		"no":      "deny",
		"pending": "approvals",
		"ls":      "sessions",
		"exit":    "quit",
		"?":       "help",
	} {
		command, ok, err := Parse(typed + " x")
		if err != nil || !ok {
			t.Errorf("%q was not understood: %v", typed, err)
			continue
		}
		if command.Verb != want {
			t.Errorf("%q means %q, want %q", typed, command.Verb, want)
		}
	}
}

func TestCaseDoesNotMatter(t *testing.T) {
	command, ok, _ := Parse("APPROVE abc")
	if !ok || command.Verb != "approve" {
		t.Errorf("APPROVE gave %q (ok=%v)", command.Verb, ok)
	}
}

// Pressing return on an empty line is not a mistake worth a message.
func TestAnEmptyLineIsNotAnError(t *testing.T) {
	for _, line := range []string{"", "   ", "\t"} {
		command, ok, err := Parse(line)
		if err != nil {
			t.Errorf("%q was an error: %v", line, err)
		}
		if ok {
			t.Errorf("%q became the command %q", line, command.Verb)
		}
	}
}

// Everything typed here is a command, so something that is not one has to say
// so rather than being sent anywhere.
func TestSomethingThatIsNotACommandSaysSo(t *testing.T) {
	_, ok, err := Parse("please deploy the thing")
	if ok {
		t.Fatal("a sentence was accepted as a command")
	}

	var unknown ErrUnknown
	if !errors.As(err, &unknown) {
		t.Fatalf("the error is %v, want one naming what was typed", err)
	}
	if unknown.Typed != "please" {
		t.Errorf("it blames %q", unknown.Typed)
	}
	if !strings.Contains(err.Error(), "help") {
		t.Errorf("the message does not say what to type instead: %v", err)
	}
}

// Help and the parser read the same table, so a command cannot exist without
// being listed or be listed without existing.
func TestHelpListsEveryCommandAndNothingElse(t *testing.T) {
	help := Help()

	for _, command := range known {
		if !strings.Contains(help, command.Verb) {
			t.Errorf("%q is a command and is not in the help", command.Verb)
		}
	}

	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if _, ok := verbs[fields[0]]; !ok {
			t.Errorf("the help lists %q, which is not a command", fields[0])
		}
	}
}

// A command that takes nothing and one that takes something both have to be
// reachable; a table that lost the difference would be a parser that did too.
func TestEveryCommandInTheTableParses(t *testing.T) {
	for _, command := range known {
		parsed, ok, err := Parse(command.Verb + " x y z")
		if err != nil || !ok {
			t.Errorf("%q does not parse: %v", command.Verb, err)
			continue
		}
		if parsed.Verb != command.Verb {
			t.Errorf("%q parsed as %q", command.Verb, parsed.Verb)
		}
	}
}
