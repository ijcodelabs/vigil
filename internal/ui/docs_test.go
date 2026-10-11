package ui

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/docs"
)

// Every page in the list exists and renders, every document is in the list,
// and every link from one document to another leads somewhere.
func TestDocs(t *testing.T) {
	h := DocsHandler()
	get := func(path string) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code, w.Body.String()
	}
	listed := map[string]bool{}
	link := regexp.MustCompile(`href="/docs/([A-Za-z0-9_-]+)(#[^"]*)?"`)
	for _, p := range pages {
		listed[p.name] = true
		code, body := get("/docs/" + p.name)
		if code != 200 || !strings.Contains(body, "<h1") || !strings.Contains(body, p.title) {
			t.Errorf("%s: status %d", p.name, code)
			continue
		}
		if strings.Contains(body, `.md"`) {
			t.Errorf("%s: a link to a .md file was not rewritten", p.name)
		}
		for _, m := range link.FindAllStringSubmatch(body, -1) {
			if c, _ := get("/docs/" + m[1]); c != 200 {
				t.Errorf("%s links to /docs/%s, which does not exist", p.name, m[1])
			}
		}
	}
	entries, _ := docs.Files.ReadDir(".")
	for _, e := range entries {
		if name := strings.TrimSuffix(e.Name(), ".md"); !listed[name] {
			t.Errorf("docs/%s is not in the list of pages", e.Name())
		}
	}
	for path, want := range map[string]int{"/docs/": 200, "/docs/install.md": 200, "/docs/nope": 404, "/docs/../../go.mod": 404} {
		if code, _ := get(path); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	// Tables with a | inside code must still render as tables.
	if _, body := get("/docs/explore"); !strings.Contains(body, "<table>") || strings.Contains(body, `<p>| `) {
		t.Error("explore: a table did not render")
	}
}
