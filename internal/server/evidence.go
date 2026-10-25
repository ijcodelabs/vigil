package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/incidents"
	"github.com/ijcodelabs/vigil/internal/redact"
)

// The evidence bundle is everything an explanation may rest on, chosen by
// vigil by incident kind: no model decides what to query. Every item has an
// id the answer must cite, a data class (metadata, or logs when the server
// allows them), and a size cap. All text is masked again here before it
// leaves, whatever the agent masked already.

const (
	itemChars   = 4000
	bundleChars = 24000
	logLineMax  = 300
)

// Evidence is one item of a bundle.
type Evidence struct {
	ID    string `json:"id"`
	Class string `json:"class"` // metadata or logs
	Title string `json:"title"`
	Text  string `json:"text"`
}

type bundle struct {
	Items []Evidence `json:"items"`
	Hash  string     `json:"hash"`
	Bytes int        `json:"bytes"`
	// Left out: classes the server does not allow, and why an item is missing.
	Withheld []string `json:"withheld,omitempty"`
}

var (
	outboundMask, _ = redact.New(config.Redact{})
	emailRe         = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// clean masks secrets and e-mail addresses and caps the length.
func clean(s string, limit int) string {
	s = emailRe.ReplaceAllString(outboundMask.String(s), "[email]")
	if len(s) > limit {
		s = s[:limit] + " …[cut]"
	}
	return s
}

type bundler struct {
	s     *Server
	ctx   context.Context
	b     bundle
	total int
}

func (bb *bundler) add(class, title, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	text = clean(text, itemChars)
	if bb.total+len(text) > bundleChars {
		bb.b.Withheld = append(bb.b.Withheld, title+" (the bundle is full)")
		return
	}
	bb.total += len(text)
	bb.b.Items = append(bb.b.Items, Evidence{ID: fmt.Sprintf("E%d", len(bb.b.Items)+1), Class: class, Title: title, Text: text})
}

// buildBundle gathers the evidence for one incident.
func (s *Server) buildBundle(ctx context.Context, in incidents.Incident, events []incidents.Event) bundle {
	bb := &bundler{s: s, ctx: ctx}
	l := in.Labels
	agent := in.Agent
	at := in.FirstSeen // what happened around the start matters most

	// The incident itself, its history and what hangs on it.
	var meta strings.Builder
	fmt.Fprintf(&meta, "title: %s\nkind: %s\nresource: %s\nseverity: %s\nstate: %s\nsilenced: %v\nfirst seen: %s\nlast seen: %s\ncame back: %d times\n",
		in.Title, in.Kind, in.Resource, in.Severity, in.State, in.Silenced, in.FirstSeen.Format(time.RFC3339), in.LastSeen.Format(time.RFC3339), in.Reopened)
	fmt.Fprintf(&meta, "labels: %s\n", sortedPairs(l))
	ann := map[string]string{}
	for k, v := range in.Annotations {
		if k != "capture" { // a link, not evidence
			ann[k] = v
		}
	}
	fmt.Fprintf(&meta, "annotations: %s\n", sortedPairs(ann))
	bb.add("metadata", "The incident", meta.String())
	var hist strings.Builder
	for _, e := range events {
		fmt.Fprintf(&hist, "%s %s %s\n", e.Time.Format(time.RFC3339), e.Type, e.Detail)
	}
	bb.add("metadata", "History of the incident", hist.String())
	if open, err := s.incidents.Open(ctx); err == nil {
		var rel strings.Builder
		var walk func(list []incidents.Incident, parent string)
		walk = func(list []incidents.Incident, parent string) {
			for _, o := range list {
				if o.ID != in.ID && (o.Agent == agent && agent != "") {
					fmt.Fprintf(&rel, "- [%s] %s (%s, since %s)%s\n", o.Severity, o.Title, o.Resource, o.FirstSeen.Format(time.RFC3339), parent)
				}
				walk(o.Children, " (held by: "+o.Title+")")
			}
		}
		walk(open, "")
		bb.add("metadata", "Other open incidents on the same host", rel.String())
	}

	// The rule behind the alert.
	if name := l["alertname"]; name != "" && in.Kind != incidents.KindSecurity {
		for _, g := range s.rules.Status() {
			for _, r := range g.Rules {
				if r.Name == name && r.Type == "alerting" {
					bb.add("metadata", "The alert rule", fmt.Sprintf("group: %s\nexpression: %s\nfor: %gs\nhealth: %s %s", g.Name, r.Query, r.Duration, r.Health, r.LastError))
				}
			}
		}
	}

	// The agent: is it reporting, what runs there, what changed recently.
	if agent != "" {
		bb.agentEvidence(agent)
	}
	bb.changes(at)

	// What the kind of incident calls for.
	q := func(s string) string { return strconv.Quote(s) }
	window := func(expr, title string) { bb.metric(expr, title, at.Add(-time.Hour), at.Add(10*time.Minute)) }
	switch {
	case l["container"] != "" && agent != "":
		sel := fmt.Sprintf(`{agent=%s, container=%s}`, q(agent), q(l["container"]))
		window(`container_running`+sel, "Container running (1 = yes)")
		window(`container_exit_code`+sel, "Container exit code while stopped")
		window(`container_restarts_total`+sel, "Container restarts (counter)")
		window(`container_memory_usage_bytes`+sel, "Container memory use (bytes)")
		window(`container_memory_limit_bytes`+sel, "Container memory limit (bytes)")
		window(`increase(container_oom_events_total`+sel+`[10m])`, "Out-of-memory events of the container (per 10 minutes)")
		window(`increase(host_oom_kills_total{agent=`+q(agent)+`}[10m])`, "Out-of-memory kills on the host (per 10 minutes)")
		bb.logs(fmt.Sprintf(`agent:=%s container:=%s`, q(agent), q(l["container"])), "Last log lines of the container before the incident", at, 40)
	case in.Kind == incidents.KindHostDown:
		window(`vigil_agent_up{agent=`+q(agent)+`}`, "Agent reporting (1 = yes)")
		window(`host_memory_available_bytes{agent=`+q(agent)+`}`, "Host memory available (bytes)")
		window(`max by (agent) (host_pressure_ratio{agent=`+q(agent)+`})`, "Highest pressure on the host (0-1)")
		window(`vigil_agent_memory_bytes{agent=`+q(agent)+`}`, "Memory of the vigil agent (bytes)")
		bb.logs(fmt.Sprintf(`agent:=%s`, q(agent)), "Last log lines of the host before it went silent", at, 30)
	case l["probe"] != "":
		sel := fmt.Sprintf(`{probe=%s}`, q(l["probe"]))
		window(`probe_success`+sel, "Check answered (1 = yes)")
		window(`probe_http_status_code`+sel, "HTTP status of the check")
		window(`probe_duration_seconds`+sel, "Time to answer (s)")
		window(`probe_dns_seconds`+sel, "DNS time (s)")
		window(`probe_connect_seconds`+sel, "Connect time (s)")
		window(`probe_tls_cert_expiry_seconds`+sel+` / 86400`, "Certificate days left")
	case l["proxy"] != "":
		sel := fmt.Sprintf(`{proxy=%s}`, q(l["proxy"]))
		window(`haproxy_server_status`+sel+` == 1`, "HAProxy server states of the backend (1 = in that state)")
		window(`sum by (proxy) (rate(haproxy_backend_http_responses_total`+sel+`[5m]))`, "Responses per second of the backend")
		bb.logs(fmt.Sprintf(`app:=haproxy %s`, l["proxy"]), "HAProxy log lines naming the backend", at, 30)
	case in.Kind == incidents.KindSecurity:
		bb.detections(agent, l["alertname"], at)
	default:
		if agent != "" {
			bb.logs(fmt.Sprintf(`agent:=%s (error OR fail OR fatal OR panic OR killed)`, q(agent)), "Error-looking log lines of the host around the start", at, 30)
		}
	}

	data, _ := json.Marshal(bb.b.Items)
	sum := sha256.Sum256(data)
	bb.b.Hash, bb.b.Bytes = hex.EncodeToString(sum[:8]), len(data)
	return bb.b
}

func sortedPairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strconv.Quote(m[k])
	}
	return strings.Join(parts, ", ")
}

func (bb *bundler) agentEvidence(agent string) {
	s := bb.s
	agents, err := s.store.Agents(time.Now(), s.cfg.HeartbeatTimeout.Std())
	if err != nil {
		return
	}
	for _, a := range agents {
		if a.ID != agent {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "reporting now: %v\nlast report: %s\nversion: %s\n", a.Up, a.LastSeen.Format(time.RFC3339), a.Version)
		f := &s.fleet
		f.mu.Lock()
		attrs := f.attrs[agent]
		f.mu.Unlock()
		facts := map[string]string{}
		for _, k := range []string{"os", "distro", "kernel", "arch", "cpus", "memory_gb", "runtime", "kubernetes", "env", "role"} {
			if v := attrs[k]; v != "" {
				facts[k] = v
			}
		}
		fmt.Fprintf(&b, "facts and labels: %s\n", sortedPairs(facts))
		bb.add("metadata", "The agent "+agent, b.String())
	}
}

// changes lists configuration changes in the six hours before the start:
// most incidents follow a change.
func (bb *bundler) changes(at time.Time) {
	rows, err := bb.s.store.db.QueryContext(bb.ctx, `SELECT name, version, author, time, deleted FROM profile_versions
		WHERE time BETWEEN ? AND ? ORDER BY time`, at.Add(-6*time.Hour).Unix(), at.Add(10*time.Minute).Unix())
	if err != nil {
		return
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var (
			name, author string
			version, ts  int64
			deleted      bool
		)
		if rows.Scan(&name, &version, &author, &ts, &deleted) == nil {
			what := "saved"
			if deleted {
				what = "deleted"
			}
			fmt.Fprintf(&b, "%s profile %s v%d %s by %s\n", time.Unix(ts, 0).UTC().Format(time.RFC3339), name, version, what, author)
		}
	}
	if b.Len() == 0 {
		b.WriteString("no profile was changed in the six hours before the incident started\n")
	}
	bb.add("metadata", "Configuration changes before the start", b.String())
}

// metric summarises a query over a window: per series the first, lowest,
// highest and last value and a sample every ten minutes.
func (bb *bundler) metric(expr, title string, from, to time.Time) {
	series, err := bb.s.queryRange(bb.ctx, expr, from, to, time.Minute)
	if err != nil || len(series) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "query: %s\nwindow: %s to %s\n", expr, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	for i, sr := range series {
		if i == 8 {
			fmt.Fprintf(&b, "… %d more series\n", len(series)-8)
			break
		}
		if len(sr.Points) == 0 {
			continue
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range sr.Points {
			lo, hi = math.Min(lo, p.V), math.Max(hi, p.V)
		}
		delete(sr.Labels, "__name__")
		fmt.Fprintf(&b, "series %s: first %s at %s, min %s, max %s, last %s at %s; samples:", sortedPairs(sr.Labels),
			num(sr.Points[0].V), sr.Points[0].T.UTC().Format("15:04"), num(lo), num(hi),
			num(sr.Points[len(sr.Points)-1].V), sr.Points[len(sr.Points)-1].T.UTC().Format("15:04"))
		for j, p := range sr.Points {
			if j%10 == 0 {
				fmt.Fprintf(&b, " %s=%s", p.T.UTC().Format("15:04"), num(p.V))
			}
		}
		b.WriteString("\n")
	}
	bb.add("metadata", title, b.String())
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

// logs adds the last lines before (and just after) the start, when log
// excerpts may be sent at all.
func (bb *bundler) logs(query, title string, at time.Time, limit int) {
	if !bb.s.cfg.AI.Allows("logs") {
		bb.b.Withheld = append(bb.b.Withheld, title+" (log excerpts are not allowed: ai.data)")
		return
	}
	lines, err := bb.s.logLines(bb.ctx, query, at.Add(-30*time.Minute), at.Add(5*time.Minute), limit)
	if err != nil || len(lines) == 0 {
		return
	}
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %s\n", l["_time"], clean(l["_msg"], logLineMax))
	}
	bb.add("logs", title, b.String())
}

// detections adds the latest runtime events of the rule, without command
// lines: those are never sent.
func (bb *bundler) detections(agent, rule string, at time.Time) {
	q := fmt.Sprintf(`source:=security agent:=%s rule:=%s`, strconv.Quote(agent), strconv.Quote(rule))
	lines, err := bb.s.logLines(bb.ctx, q, at.Add(-time.Hour), time.Now(), 20)
	if err != nil || len(lines) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d latest detections of rule %s (command lines left out):\n", len(lines), rule)
	for _, l := range lines {
		var parts []string
		for _, k := range []string{"type", "comm", "exe", "parent_comm", "parent_exe", "uid", "euid", "container", "path", "addr", "port", "write", "old_euid", "new_euid"} {
			if v := l[k]; v != "" {
				parts = append(parts, k+"="+strconv.Quote(v))
			}
		}
		fmt.Fprintf(&b, "%s %s: %s\n", l["_time"], clean(l["_msg"], 200), strings.Join(parts, " "))
	}
	bb.add("metadata", "Runtime detections", b.String())
}

// logLines runs a LogsQL query against the log store, newest lines last.
func (s *Server) logLines(ctx context.Context, query string, from, to time.Time, limit int) ([]map[string]string, error) {
	out, err := s.logQuery(ctx, query+" | sort by (_time desc)", from, to, limit)
	// Oldest first, as a person reads a log.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

// logQuery runs a LogsQL query against the log store and returns its rows
// in the order the store gives them.
func (s *Server) logQuery(ctx context.Context, query string, from, to time.Time, limit int) ([]map[string]string, error) {
	form := url.Values{"query": {query}, "start": {from.UTC().Format(time.RFC3339Nano)},
		"end": {to.UTC().Format(time.RFC3339Nano)}, "limit": {strconv.Itoa(limit)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.logs.JoinPath("/select/logsql/query").String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("log query refused (HTTP %d): %s", resp.StatusCode, msg)
	}
	var out []map[string]string
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 8<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}
