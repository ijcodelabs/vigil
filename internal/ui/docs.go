package ui

import (
	"bytes"
	"html"
	"net/http"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"github.com/ijcodelabs/vigil/docs"
)

// pages lists the documentation in the order it is offered.
var pages = []struct{ name, title string }{
	{"README", "Overview"},
	{"install", "Install"},
	{"install-by-ai", "Install with an AI assistant"},
	{"configs", "Agent configuration"},
	{"fleet", "Fleet: profiles and integrations"},
	{"kubernetes", "Kubernetes"},
	{"relay", "Relays"},
	{"home", "Home"},
	{"explore", "Explore"},
	{"apm", "Services and traces"},
	{"gpu", "GPUs and AI machines"},
	{"finops", "AI costs"},
	{"claude-code", "Claude Code"},
	{"dashboards", "Dashboards"},
	{"alerts", "Alerts"},
	{"grafana", "Grafana"},
	{"security", "Runtime events"},
	{"metrics", "Metric reference"},
	{"api", "API"},
	{"troubleshooting", "Troubleshooting"},
}

var (
	markdown   = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithParserOptions(parser.WithAutoHeadingID()))
	docNameRe  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	mdLinkRe   = regexp.MustCompile(`href="([A-Za-z0-9_-]+)\.md(#[^"]*)?"`)
	docsHeader = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%TITLE% · vigil docs</title>
<link rel="stylesheet" href="/app.css"><link rel="stylesheet" href="/docs.css"><script src="/js/theme.js"></script>
</head><body><div class="shell">
<aside class="side"><div class="brand"><i></i>vigil docs</div><nav class="nav">%NAV%</nav>
<span class="spacer"></span><div class="who"><a href="/">← Back to vigil</a></div></aside>
<main class="main doc">`
)

// DocsHandler serves the documentation as pages: /docs/ is the overview,
// /docs/<name> one document. Links between documents written as name.md
// work both here and when the files are read in a repository.
func DocsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.Trim(strings.TrimPrefix(r.URL.Path, "/docs"), "/"), ".md")
		if name == "" {
			name = "README"
		}
		if !docNameRe.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		src, err := docs.Files.ReadFile(name + ".md")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var body bytes.Buffer
		if err := markdown.Convert(src, &body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		title := name
		var nav strings.Builder
		for _, p := range pages {
			class := ""
			if p.name == name {
				class, title = ` class="on"`, p.title
			}
			nav.WriteString(`<a` + class + ` href="/docs/` + p.name + `">` + html.EscapeString(p.title) + `</a>`)
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-cache")
		page := strings.NewReplacer("%TITLE%", html.EscapeString(title), "%NAV%", nav.String()).Replace(docsHeader)
		w.Write([]byte(page))
		w.Write(mdLinkRe.ReplaceAll(body.Bytes(), []byte(`href="/docs/$1$2"`)))
		w.Write([]byte("</main></div></body></html>"))
	})
}
