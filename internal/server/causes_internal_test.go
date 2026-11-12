package server

import (
	"strings"
	"testing"
)

// A measured sign outranks words in a log, each cause names its evidence
// and comes with a check, and a bundle without any sign gives nothing.
func TestLikelyCauses(t *testing.T) {
	b := bundle{Items: []Evidence{
		{ID: "E1", Class: "metadata", Title: "The incident", Text: "title: shop-api down\n"},
		{ID: "E2", Class: "metadata", Title: "Configuration changes before the start", Text: "no profile was changed in the six hours before the incident started\n"},
		{ID: "E3", Class: "metadata", Title: "Out-of-memory events of the container (per 10 minutes)",
			Text: "query: q\nwindow: a to b\nseries agent=\"web\", container=\"shop-api\": first 0 at 09:00, min 0, max 1, last 0 at 10:10; samples: 09:00=0\n"},
		{ID: "E4", Class: "metadata", Title: "Container restarts (counter)",
			Text: "series container=\"shop-api\": first 3 at 09:00, min 3, max 7, last 7 at 10:10; samples:\n"},
		{ID: "E5", Class: "logs", Title: "Last log lines of the container before the incident", Text: strings.Join([]string{
			"2026-11-12T09:58:00Z GET /cart 200",
			"2026-11-12T09:59:00Z upstream: dial tcp 10.0.0.5:5432: i/o timeout",
			"2026-11-12T09:59:30Z context deadline exceeded while reading",
			"2026-11-12T09:59:40Z query timed out after 5s",
			"2026-11-12T10:00:00Z password authentication failed for user shop",
		}, "\n")},
	}}
	got := likelyCauses(b)
	if len(got) < 4 {
		t.Fatalf("causes: %+v", got)
	}
	if got[0].Title != "Out of memory" || got[0].Score < 4 || !strings.HasPrefix(got[0].Seen[0], "E3 ") {
		t.Errorf("first: %+v", got[0])
	}
	titles := map[string]Cause{}
	for _, c := range got {
		titles[c.Title] = c
		if c.Check == "" || len(c.Seen) == 0 {
			t.Errorf("%s without a check or evidence: %+v", c.Title, c)
		}
	}
	if c := titles["A timeout talking to something else"]; c.Score != 2 || !strings.Contains(c.Seen[0], "3 of 5 lines") {
		t.Errorf("timeout: %+v", c)
	}
	if c := titles["Credentials refused or expired"]; c.Score != 1 {
		t.Errorf("one auth line: %+v", c)
	}
	if _, ok := titles["Restarting in a loop"]; !ok {
		t.Errorf("4 restarts not seen: %+v", got)
	}
	if _, ok := titles["A change shortly before"]; ok {
		t.Errorf("no change was made, but one is listed")
	}
	if c := likelyCauses(bundle{Items: b.Items[:2]}); len(c) != 0 {
		t.Errorf("nothing to go on, still: %+v", c)
	}
	// A proxy's handshake failures are scanners, not a certificate; lines of
	// the whole host are a hint at most.
	host := bundle{Items: []Evidence{{ID: "E1", Class: "logs", Title: hostErrorLines, Text: strings.Repeat(
		"https-in/1: SSL handshake failure\nupstream timed out\n", 5)}}}
	hc := likelyCauses(host)
	if len(hc) != 1 || hc[0].Title != "A timeout talking to something else" || hc[0].Score != 1 {
		t.Errorf("host-wide lines: %+v", hc)
	}
	if !strings.Contains(causesText(got), "Check:") || !strings.Contains(causesText(nil), "found nothing") {
		t.Errorf("text: %s", causesText(got))
	}
}
