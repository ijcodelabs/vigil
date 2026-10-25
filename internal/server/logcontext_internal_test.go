package server

import (
	"regexp"
	"testing"
)

// linePattern makes the same pattern as suggestPattern in explore.js, and
// it matches the line it was made from and its siblings.
func TestLinePattern(t *testing.T) {
	for _, c := range []struct{ msg, want, sibling string }{
		{"took 31ms (status=200)", `took \d+ms \(status=\d+\)`, "took 7ms (status=503)"},
		{"job 3f9a2b7c1d done", `job [0-9a-f]+ done`, "job 77aa00ff12 done"},
		{"user 6d2c0a4e-3b1f-4c8a-9e7d-1a2b3c4d5e6f signed in", `user [0-9a-f-]+ signed in`, "user 00000000-0000-4000-8000-000000000000 signed in"},
		{"deadbeefcafe stays", `deadbeefcafe stays`, ""}, // letters only: a word, not an id
	} {
		got := linePattern(c.msg)
		if got != c.want {
			t.Errorf("linePattern(%q) = %q, want %q", c.msg, got, c.want)
		}
		re := regexp.MustCompile(got)
		if !re.MatchString(c.msg) || (c.sibling != "" && !re.MatchString(c.sibling)) {
			t.Errorf("%q does not match its line or %q", got, c.sibling)
		}
	}
}
