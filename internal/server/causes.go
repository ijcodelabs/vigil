package server

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Likely causes of an incident, without a language model: known patterns
// (a timeout, refused credentials, memory running out, …) looked for in
// the same evidence an explanation would get, ranked by how strong the
// sign is. Each comes with the one check that would confirm or rule it
// out, because a list of guesses is only useful if each can be settled
// quickly. The AI explanation gets the list as one more evidence item and
// may agree, disagree or add.

// Cause is one candidate.
type Cause struct {
	Title string   `json:"title"`
	Score int      `json:"score"` // higher is a stronger sign; 3 and more is a measured one
	Seen  []string `json:"seen"`  // what points to it, each naming its evidence item
	Check string   `json:"check"` // the one check that confirms or rules it out
}

type logPattern struct {
	title, check string
	re           *regexp.Regexp
}

var logPatterns = []logPattern{
	{"A timeout talking to something else",
		"Take the address the lines name and time a call to it from the same host now: fast means the other side or the network was slow at that moment, still slow means it is now.",
		regexp.MustCompile(`(?i)timed? ?out|timeout|deadline exceeded`)},
	{"Something it depends on is down or not listening",
		"Check whether the address in the lines accepts connections (a tcp check on that host and port, or nc -vz from the same host).",
		regexp.MustCompile(`(?i)connection refused|econnrefused|connection reset|no route to host|broken pipe|upstream connect error`)},
	{"A name does not resolve",
		"Resolve the name from the same host (getent hosts <name>) and compare with what it should be.",
		regexp.MustCompile(`(?i)no such host|nxdomain|name resolution|could not resolve|server misbehaving`)},
	{"Credentials refused or expired",
		"Try the same credentials by hand against the same service, and look for a password, token or key that changed or expired around the start.",
		regexp.MustCompile(`(?i)unauthori[sz]ed|forbidden|\b40[13]\b|permission denied|access denied|invalid (token|credentials|api key|password)|authentication failed|login failed|token (is )?expired`)},
	{"A certificate is not accepted",
		"Look at the certificate the other side presents (openssl s_client -connect host:port): its expiry, its name and who issued it.",
		// Not "handshake failure": a proxy logs that for every scanner that
		// speaks plain HTTP to its TLS port.
		regexp.MustCompile(`(?i)x509|certificate (has )?expired|certificate verify failed|unknown authority|bad certificate`)},
	{"Out of memory",
		"Look for the kernel's \"Killed process\" line at the start (journalctl -k), or container_oom_events_total of the container.",
		regexp.MustCompile(`(?i)out of memory|oom.?kill|killed process|cannot allocate memory|memory limit`)},
	{"A disk is full or read-only",
		"Run df -h and df -i on the host for the path in the lines.",
		regexp.MustCompile(`(?i)no space left|disk full|quota exceeded|read-only file system`)},
	{"The program crashed",
		"Read the first stack trace in the lines, and check whether the program's version or configuration changed just before.",
		regexp.MustCompile(`(?i)panic:|segmentation fault|fatal error|traceback \(most recent|core dumped|unhandled exception`)},
	{"A limit was hit (rate, open files, connections)",
		"Find which limit the lines name, and compare the use with it (ulimit -n, the pool size, the API's rate limit).",
		regexp.MustCompile(`(?i)too many open files|too many requests|rate.?limit|\b429\b|resource exhausted|throttl|too many connections`)},
}

// hostErrorLines is the title of the evidence item with error-looking lines
// of the whole host, gathered when the incident's kind calls for nothing
// more specific.
const hostErrorLines = "Error-looking log lines of the host around the start"

// seriesRe reads the summary line bundler.metric writes for each series.
var seriesRe = regexp.MustCompile(`^series [^:]*: first (\S+) at \S+, min (\S+), max (\S+), last (\S+) at`)

type seriesSummary struct{ first, min, max, last float64 }

func readSeries(text string) []seriesSummary {
	var out []seriesSummary
	for _, line := range strings.Split(text, "\n") {
		m := seriesRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var v [4]float64
		ok := true
		for i := range v {
			f, err := strconv.ParseFloat(m[i+1], 64)
			if err != nil {
				ok = false
			}
			v[i] = f
		}
		if ok {
			out = append(out, seriesSummary{v[0], v[1], v[2], v[3]})
		}
	}
	return out
}

// likelyCauses looks for the known patterns in a bundle.
func likelyCauses(b bundle) []Cause {
	found := map[string]*Cause{}
	note := func(title, check string, score int, seen string) {
		c := found[title]
		if c == nil {
			c = &Cause{Title: title, Check: check}
			found[title] = c
		}
		c.Score += score
		c.Seen = append(c.Seen, seen)
	}
	checkOf := map[string]string{}
	for _, p := range logPatterns {
		checkOf[p.title] = p.check
	}
	byTitle := map[string]Evidence{}
	for _, e := range b.Items {
		byTitle[e.Title] = e
	}
	series := func(title string) (Evidence, []seriesSummary) {
		e, ok := byTitle[title]
		if !ok {
			return e, nil
		}
		return e, readSeries(e.Text)
	}

	// Measured signs first: they count more than words in a log.
	if e, ss := series("Out-of-memory events of the container (per 10 minutes)"); anyMax(ss, func(v float64) bool { return v > 0 }) {
		note("Out of memory", checkOf["Out of memory"], 4, e.ID+" "+e.Title+": the container was killed for using too much memory")
	}
	if e, ss := series("Out-of-memory kills on the host (per 10 minutes)"); anyMax(ss, func(v float64) bool { return v > 0 }) {
		note("Out of memory", checkOf["Out of memory"], 3, e.ID+" "+e.Title+": the kernel killed processes for memory")
	}
	if eu, use := series("Container memory use (bytes)"); len(use) > 0 {
		if _, lim := series("Container memory limit (bytes)"); len(lim) > 0 && lim[0].max > 0 && use[0].max >= 0.9*lim[0].max {
			note("Out of memory", checkOf["Out of memory"], 2, fmt.Sprintf("%s %s: up to %.0f%% of its limit", eu.ID, eu.Title, 100*use[0].max/lim[0].max))
		}
	}
	if e, ss := series("Container exit code while stopped"); len(ss) > 0 {
		switch code := ss[0].max; {
		case code == 137:
			note("Out of memory", checkOf["Out of memory"], 2, e.ID+" "+e.Title+": 137, killed (out of memory, or a SIGKILL)")
		case code > 0 && code != 143:
			note("The program crashed", checkOf["The program crashed"], 3, fmt.Sprintf("%s %s: %g", e.ID, e.Title, code))
		}
	}
	if e, ss := series("Container restarts (counter)"); len(ss) > 0 && ss[0].last-ss[0].first >= 2 {
		note("Restarting in a loop", "Read the last lines before each restart: the same error every time is the cause; also check the exit code.", 3,
			fmt.Sprintf("%s %s: %g restarts in the window", e.ID, e.Title, ss[0].last-ss[0].first))
	}
	if e, ss := series("Highest pressure on the host (0-1)"); anyMax(ss, func(v float64) bool { return v >= 0.4 }) {
		note("The host was starved (CPU, memory or disk)", "Open the host's pressure and top-processes panels for the start: which process took it, and was it new?", 3,
			fmt.Sprintf("%s %s: up to %g", e.ID, e.Title, maxOf(ss)))
	}
	if e, ss := series("Certificate days left"); len(ss) > 0 {
		if lo := minOf(ss); lo <= 0 {
			note("A certificate is not accepted", checkOf["A certificate is not accepted"], 4, e.ID+" "+e.Title+": expired")
		} else if lo < 7 {
			note("A certificate is not accepted", checkOf["A certificate is not accepted"], 2, fmt.Sprintf("%s %s: %.1f days left", e.ID, e.Title, lo))
		}
	}
	if e, ss := series("HTTP status of the check"); len(ss) > 0 {
		switch code := ss[0].max; {
		case code == 401 || code == 403:
			note("Credentials refused or expired", checkOf["Credentials refused or expired"], 3, fmt.Sprintf("%s %s: %g", e.ID, e.Title, code))
		case code >= 500:
			note("The site answers with server errors", "Read the site's own error log at the start; a 502 or 504 points at what is behind it.", 3, fmt.Sprintf("%s %s: %g", e.ID, e.Title, code))
		}
	}
	if e, ss := series("DNS time (s)"); anyMax(ss, func(v float64) bool { return v >= 1 }) {
		note("A name does not resolve", checkOf["A name does not resolve"], 2, fmt.Sprintf("%s %s: up to %g s", e.ID, e.Title, maxOf(ss)))
	}
	if e, ss := series("Connect time (s)"); anyMax(ss, func(v float64) bool { return v >= 1 }) {
		note("A timeout talking to something else", checkOf["A timeout talking to something else"], 2, fmt.Sprintf("%s %s: up to %g s", e.ID, e.Title, maxOf(ss)))
	}
	if e, ok := byTitle["Configuration changes before the start"]; ok && !strings.HasPrefix(e.Text, "no profile was changed") {
		n := strings.Count(strings.TrimSpace(e.Text), "\n") + 1
		note("A change shortly before", "Compare before and after the change (Services → Did a change help?), or undo it and watch the alert.", 2,
			fmt.Sprintf("%s %s: %d in the six hours before", e.ID, e.Title, n))
	}

	// Words in the log lines.
	for _, e := range b.Items {
		if e.Class != "logs" {
			continue
		}
		lines := strings.Split(strings.TrimSpace(e.Text), "\n")
		for _, p := range logPatterns {
			n, example := 0, ""
			for _, l := range lines {
				if p.re.MatchString(l) {
					n++
					if example == "" {
						example = l
					}
				}
			}
			if n == 0 {
				continue
			}
			// One line is a hint; many are a pattern. Never as strong as a measurement.
			// Lines from the whole host (incidents of no particular kind) are
			// not about this incident in particular: a hint at most.
			score := 1
			if n >= 3 && e.Title != hostErrorLines {
				score = 2
			}
			if len(example) > 160 {
				example = example[:160] + "…"
			}
			note(p.title, p.check, score, fmt.Sprintf("%s %s: %d of %d lines, e.g. %s", e.ID, e.Title, n, len(lines), example))
		}
	}

	out := make([]Cause, 0, len(found))
	for _, c := range found {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Title < out[j].Title
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func anyMax(ss []seriesSummary, f func(float64) bool) bool {
	for _, s := range ss {
		if f(s.max) {
			return true
		}
	}
	return false
}

func maxOf(ss []seriesSummary) float64 {
	m := ss[0].max
	for _, s := range ss {
		m = max(m, s.max)
	}
	return m
}

func minOf(ss []seriesSummary) float64 {
	m := ss[0].min
	for _, s := range ss {
		m = min(m, s.min)
	}
	return m
}

// causesText is the list as an evidence item for an explanation.
func causesText(cs []Cause) string {
	if len(cs) == 0 {
		return "vigil's built-in patterns found nothing in the items above\n"
	}
	var b strings.Builder
	b.WriteString("Candidates found by vigil's built-in patterns in the items above, strongest first; confirm, rule out or add to them:\n")
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s (score %d): %s. Check: %s\n", c.Title, c.Score, strings.Join(c.Seen, "; "), c.Check)
	}
	return b.String()
}

func (s *Server) handleIncidentCauses(w http.ResponseWriter, r *http.Request) {
	in, events, ok := s.incidentFor(w, r)
	if !ok {
		return
	}
	// Built for this server only: nothing leaves it, so log lines are read
	// whatever ai.data allows to be sent.
	b := s.bundleFor(r.Context(), in, events, true)
	writeJSON(w, map[string]any{"causes": likelyCauses(b)})
}
