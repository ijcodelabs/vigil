package server_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSavedQueries(t *testing.T) {
	stores := httptest.NewServer(&alertStores{})
	defer stores.Close()
	_, central := startServer(t, stores.URL)
	token := newClient(t, central.URL)
	admin := func(method, path, body string) (int, string) {
		code, out, _ := token.do(method, path, body, "Authorization", "Bearer admin-token-secret")
		return code, out
	}
	for _, u := range [][2]string{{"ada", "admin"}, {"vera", "viewer"}, {"viktor", "viewer"}} { // the first account has to be an admin
		if code, body := admin("PUT", "/api/v1/users/"+u[0], `{"password":"a-long-password-1","role":"`+u[1]+`"}`); code != 204 {
			t.Fatalf("user %s: %d %s", u[0], code, body)
		}
	}
	as := func(name string) *client {
		c := newClient(t, central.URL)
		if code := c.login(name, "a-long-password-1"); code != 204 {
			t.Fatalf("login %s: %d", name, code)
		}
		return c
	}
	ada, vera, viktor := as("ada"), as("vera"), as("viktor")
	type saved struct {
		ID, Name, Type, Query, Owner, Source, Origin string
		Shared, Editable                             bool
	}
	list := func(c *client) (own []saved, provided []saved) {
		_, body, _ := c.do("GET", "/api/v1/queries", "")
		var all []saved
		if err := json.Unmarshal([]byte(body), &all); err != nil {
			t.Fatalf("list: %v: %s", err, body)
		}
		for _, q := range all {
			if q.Source != "" {
				provided = append(provided, q)
			} else {
				own = append(own, q)
			}
		}
		return own, provided
	}
	names := func(l []saved) string {
		var out []string
		for _, q := range l {
			out = append(out, q.Name)
		}
		return strings.Join(out, ",")
	}
	save := func(c *client, body string) saved {
		t.Helper()
		code, out, _ := c.do("POST", "/api/v1/queries", body)
		if code != 200 {
			t.Fatalf("save %s: %d %s", body, code, out)
		}
		var q saved
		json.Unmarshal([]byte(out), &q)
		return q
	}

	// A new server offers the starter queries, to everyone, read-only.
	own, provided := list(vera)
	if len(own) != 0 || len(provided) < 15 || provided[0].Source != "starter" || provided[0].Editable || provided[0].ID != "" {
		t.Fatalf("a new server: %d saved, %d provided, first %+v", len(own), len(provided), provided)
	}
	kinds := map[string]int{}
	for _, q := range provided {
		kinds[q.Type]++
	}
	if kinds["metrics"] == 0 || kinds["logs"] == 0 || len(kinds) != 2 {
		t.Errorf("starter queries by type: %v", kinds)
	}

	// A viewer saves for herself; nobody else sees it, not even an admin.
	mine := save(vera, `{"name":" Load ","query":"host_load1\n"}`)
	if mine.Name != "Load" || mine.Type != "metrics" || mine.Query != "host_load1" || mine.Shared || mine.Owner != "vera" || !mine.Editable {
		t.Errorf("saved: %+v", mine)
	}
	if own, _ := list(vera); names(own) != "Load" {
		t.Errorf("vera sees %q", names(own))
	}
	for name, c := range map[string]*client{"viktor": viktor, "ada": ada} {
		if own, _ := list(c); len(own) != 0 {
			t.Errorf("%s sees vera's query", name)
		}
		for _, method := range []string{"PUT", "DELETE"} {
			if code, _, _ := c.do(method, "/api/v1/queries/"+mine.ID, `{"name":"x"}`); code != 404 {
				t.Errorf("%s %s on vera's query: %d, want 404", name, method, code)
			}
		}
	}
	// The same name is taken for her, in that type only, and free for others.
	if code, body, _ := vera.do("POST", "/api/v1/queries", `{"name":"load","query":"up"}`); code != 409 || !strings.Contains(body, "already have") {
		t.Errorf("same name again: %d %s", code, body)
	}
	save(vera, `{"name":"Load","type":"logs","query":"load"}`)
	save(viktor, `{"name":"Load","query":"host_load5"}`)

	for body, want := range map[string]int{
		`{"name":"","query":"up"}`:                 400,
		`{"name":"x","query":" "}`:                 400,
		`{"name":"x","type":"traces","query":"x"}`: 400,
		`{"name":"x","query":"up","shared":true}`:  403, // a viewer does not share
		`not json`: 400,
	} {
		if code, out, _ := vera.do("POST", "/api/v1/queries", body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, out, want)
		}
	}

	// Only what is given changes.
	if code, body, _ := vera.do("PUT", "/api/v1/queries/"+mine.ID, `{"query":"host_load15"}`); code != 200 || !strings.Contains(body, `"name":"Load"`) || !strings.Contains(body, "host_load15") {
		t.Errorf("update: %d %s", code, body)
	}
	if code, _, _ := vera.do("PUT", "/api/v1/queries/"+mine.ID, `{"shared":true}`); code != 403 {
		t.Errorf("a viewer shared her query: %d", code)
	}

	// An admin shares: everyone sees it, only admins change it.
	team := save(ada, `{"name":"Errors","type":"logs","query":"error","shared":true}`)
	own, _ = list(viktor)
	if names(own) != "Errors,Load" || own[0].Editable || !own[0].Shared || own[0].Owner != "ada" {
		t.Errorf("viktor sees %+v", own)
	}
	for _, method := range []string{"PUT", "DELETE"} {
		if code, body, _ := viktor.do(method, "/api/v1/queries/"+team.ID, `{"name":"x"}`); code != 403 || !strings.Contains(body, "admin") {
			t.Errorf("a viewer's %s on a shared query: %d %s", method, code, body)
		}
	}
	if code, body, _ := ada.do("POST", "/api/v1/queries", `{"name":"errors","type":"logs","query":"x","shared":true}`); code != 409 || !strings.Contains(body, "There already is") {
		t.Errorf("a second shared query of that name: %d %s", code, body)
	}
	// Taken back, it is the admin's own.
	if code, _, _ := ada.do("PUT", "/api/v1/queries/"+team.ID, `{"shared":false}`); code != 200 {
		t.Errorf("unshare: %d", code)
	}
	if own, _ := list(viktor); names(own) != "Load" {
		t.Errorf("after unsharing viktor sees %q", names(own))
	}
	if own, _ := list(ada); names(own) != "Errors" || own[0].Shared {
		t.Errorf("after unsharing ada has %+v", own)
	}
	ada.do("PUT", "/api/v1/queries/"+team.ID, `{"shared":true}`)

	// From files: what vigil apply sends, with the admin token.
	code, body, _ := token.do("POST", "/api/v1/queries", `{"name":"From git","query":"up","shared":true}`,
		"Authorization", "Bearer admin-token-secret", "X-Vigil-Origin", "files")
	if code != 200 || !strings.Contains(body, `"origin":"files"`) {
		t.Errorf("from files: %d %s", code, body)
	}

	// An integration in use brings its queries.
	if code, body := admin("PUT", "/api/v1/fleet/profiles/base", "integrations: [docker]\n"); code != 200 {
		t.Fatalf("profile: %d %s", code, body)
	}
	_, provided = list(vera)
	var docker int
	for _, q := range provided {
		if q.Source == "integration docker" {
			docker++
		}
	}
	if docker < 4 {
		t.Errorf("queries of the docker integration: %d", docker)
	}

	// A deleted account takes its own queries with it and leaves what it shared.
	save(ada, `{"name":"Scratch","query":"up"}`)
	admin("PUT", "/api/v1/users/bea", `{"password":"a-long-password-1","role":"admin"}`) // the last admin cannot be deleted
	for _, name := range []string{"vera", "ada"} {
		if code, body := admin("DELETE", "/api/v1/users/"+name, ""); code != 204 {
			t.Fatalf("delete %s: %d %s", name, code, body)
		}
	}
	admin("PUT", "/api/v1/users/ada", `{"password":"a-long-password-1","role":"admin"}`)
	if own, _ := list(as("ada")); names(own) != "Errors,From git" {
		t.Errorf("a new account called ada sees %q", names(own))
	}
	if code, _, _ := viktor.do("DELETE", "/api/v1/queries/"+mine.ID, ""); code != 404 {
		t.Errorf("vera's query outlived her: %d", code)
	}
}

// An alert made from a form lands in the rule file next to what is there.
func TestAddAlert(t *testing.T) {
	stores := httptest.NewServer(&alertStores{})
	defer stores.Close()
	_, central := startServer(t, stores.URL)
	c := newClient(t, central.URL)
	admin := func(method, path, body string) (int, string) {
		code, out, _ := c.do(method, path, body, "Authorization", "Bearer admin-token-secret")
		return code, out
	}
	file := "# kept in git\ngroups:\n  - name: hosts\n    rules:\n      - alert: Busy # the old one\n        expr: cpu_high\n"
	if code, body := admin("PUT", "/api/v1/rules/config", file); code != 204 {
		t.Fatalf("rule file: %d %s", code, body)
	}
	add := func(body string) (int, string) { return admin("POST", "/api/v1/rules/config/alerts", body) }

	if code, body := add(`{"group":"hosts","alert":"Loaded","expr":"host_load1 > 4","for":"5m","severity":"warning","summary":"{{ $labels.agent }} is loaded"}`); code != 204 {
		t.Fatalf("add: %d %s", code, body)
	}
	if code, body := add(`{"group":"log-alerts","type":"logs","alert":"Errors","expr":"_time:5m error | stats count() as n | filter n:>0"}`); code != 204 {
		t.Fatalf("add to a new group: %d %s", code, body)
	}
	_, text := admin("GET", "/api/v1/rules/config", "")
	for _, want := range []string{"# kept in git", "# the old one", "alert: Loaded", "expr: 'host_load1 > 4'", "for: 5m", "severity: warning",
		"summary: '{{ $labels.agent }} is loaded'", "name: log-alerts", "type: logs", "alert: Errors"} {
		if !strings.Contains(text, want) {
			t.Errorf("the rule file lacks %q:\n%s", want, text)
		}
	}
	if _, body := admin("GET", "/api/v1/rules/config/groups", ""); body != `[{"name":"hosts","type":"metrics"},{"name":"log-alerts","type":"logs"}]` {
		t.Errorf("groups: %s", body)
	}
	if _, body := admin("GET", "/api/v1/rules", ""); !strings.Contains(body, `"name":"Loaded"`) || !strings.Contains(body, `"name":"Errors"`) {
		t.Errorf("the new rules do not run: %s", body)
	}

	for body, want := range map[string]string{
		`{"group":"hosts","alert":"Loaded","expr":"up == 0"}`:            "already has an alert called Loaded",
		`{"group":"log-alerts","alert":"X","expr":"up == 0"}`:            "holds logs rules",
		`{"group":"hosts","alert":"HostDown","expr":"up == 0"}`:          "comes with vigil",
		`{"group":"builtin","alert":"X","expr":"up == 0"}`:               "taken by rules that come with vigil",
		`{"group":"hosts","alert":"X","expr":"bad("}`:                    "invalid rules",
		`{"group":"hosts","alert":"X","expr":"up == 0","for":"soon"}`:    "invalid rules",
		`{"group":"hosts","alert":"","expr":"up == 0"}`:                  "are needed",
		`{"group":"hosts","type":"traces","alert":"X","expr":"up == 0"}`: "type must be",
		`{"group":"hosts","alert":"X","expr":"up","summary":"{{ .Nope"}`: "invalid rules",
	} {
		if code, out := add(body); code < 400 || !strings.Contains(out, want) {
			t.Errorf("%s: %d %s, want an error with %q", body, code, out, want)
		}
	}
	if _, after := admin("GET", "/api/v1/rules/config", ""); after != text {
		t.Errorf("a refused alert changed the rule file:\n%s", after)
	}

	// On a server without a rule file yet.
	_, fresh := startServer(t, stores.URL)
	c2 := newClient(t, fresh.URL)
	if code, body, _ := c2.do("POST", "/api/v1/rules/config/alerts", `{"group":"alerts","alert":"Down","expr":"up == 0"}`, "Authorization", "Bearer admin-token-secret"); code != 204 {
		t.Fatalf("first rule of a server: %d %s", code, body)
	}
	if _, body, _ := c2.do("GET", "/api/v1/rules/config", "", "Authorization", "Bearer admin-token-secret"); !strings.Contains(body, "- name: alerts") || !strings.Contains(body, "alert: Down") {
		t.Errorf("first rule file: %s", body)
	}
}
