package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ijcodelabs/vigil/docs"
)

// The catalog is what the model knows about this vigil when it writes a
// view: the metric reference and the query primers from vigil's own docs,
// and the dashboards that come with vigil and with the built-in
// integrations in use, as examples. All of it is text vigil ships, so none
// of it was written by whoever runs the hosts; no series, label values,
// host names or log lines are in it. A host the operator names is added
// per turn as a reference (views.go).

const maxCatalog = 96 << 10

// exploreSections are the parts of docs/explore.md that teach the query
// languages; the rest is about the interface.
var exploreSections = []string{"PromQL in five minutes", "Things that surprise people", "What a log line has", "LogsQL in five minutes", "Queries to start from"}

type viewCatalog struct {
	Text        string `json:"text"`
	Fingerprint string `json:"fingerprint"`
	Bytes       int    `json:"bytes"`
}

func (s *Server) viewCatalog() viewCatalog {
	var b strings.Builder
	if md, err := docs.Files.ReadFile("metrics.md"); err == nil {
		b.WriteString("<metrics>\n")
		b.Write(md)
		b.WriteString("\n</metrics>\n\n")
	}
	if md, err := docs.Files.ReadFile("explore.md"); err == nil {
		b.WriteString("<queries>\n")
		for _, h := range exploreSections {
			for _, sec := range mdSections(string(md), h) {
				b.WriteString(sec)
				b.WriteString("\n")
			}
		}
		b.WriteString("</queries>\n\n")
	}
	b.WriteString("<examples>\n")
	all := s.provided()
	ids := make([]string, 0, len(all))
	for id, p := range all {
		// Only what ships with vigil: a starter, or a built-in integration's.
		if p.source == "starter" || s.builtinIntegration(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		var d Dashboard
		if json.Unmarshal([]byte(all[id].body), &d) != nil {
			continue
		}
		fmt.Fprintf(&b, "Dashboard %q:\n", d.Title)
		for _, p := range d.Panels {
			if p.Type == "map" {
				continue
			}
			src := p.Source
			if src == "" || p.Type == "logs" {
				src = map[bool]string{true: "logs", false: "metrics"}[p.Type == "logs"]
			}
			fmt.Fprintf(&b, "- %s [%s, %s%s]\n", p.Title, p.Type, src, map[bool]string{true: ", unit " + p.Unit}[p.Unit != ""])
			for _, q := range p.Queries {
				fmt.Fprintf(&b, "    %s\n", q.Expr)
			}
		}
		if b.Len() > maxCatalog {
			break
		}
	}
	b.WriteString("</examples>\n")
	text := b.String()
	if len(text) > maxCatalog {
		text = text[:maxCatalog]
	}
	sum := sha256.Sum256([]byte(text))
	return viewCatalog{Text: text, Fingerprint: hex.EncodeToString(sum[:8]), Bytes: len(text)}
}

// builtinIntegration says whether the integration of that name is in use
// and is vigil's own, not a local one or a local change of one.
func (s *Server) builtinIntegration(name string) bool {
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.catalog[name]
	return in != nil && in.Builtin
}

// mdSections returns every section of a Markdown page under a heading with
// that text, down to the next heading of the same or a higher level.
func mdSections(md, heading string) []string {
	lines := strings.Split(md, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		level := strings.IndexFunc(lines[i], func(r rune) bool { return r != '#' })
		if level < 1 || strings.TrimSpace(lines[i][level:]) != heading {
			continue
		}
		j := i + 1
		for ; j < len(lines); j++ {
			if l := strings.IndexFunc(lines[j], func(r rune) bool { return r != '#' }); l >= 1 && l <= level && strings.HasPrefix(lines[j][l:], " ") {
				break
			}
		}
		out = append(out, strings.Join(lines[i:j], "\n"))
		i = j - 1
	}
	return out
}
