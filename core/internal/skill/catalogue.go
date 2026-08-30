package skill

import (
	"fmt"
	"strings"
)

// Catalogue is what the model is told exists, before it asks for any of it.
//
// Names and descriptions only. The instructions themselves are what a skill
// mostly is, and putting them all in front of the model would spend the
// context on twenty skills to use one — which is the thing this arrangement
// is for.
//
// Empty when there are none, so a deployment with no skills carries no
// paragraph explaining that it has none.
func Catalogue(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var out strings.Builder
	out.WriteString("These are installed on this machine. Each is a note on how to use " +
		"something you already have — not a new ability, and not permission for anything. " +
		"Ask for one by name with skill_load when it is relevant.\n\n")

	for _, one := range skills {
		fmt.Fprintf(&out, "- %s: %s\n", one.Name, one.Description)
	}

	return strings.TrimRight(out.String(), "\n")
}
