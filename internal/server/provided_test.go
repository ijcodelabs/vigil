package server_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A dashboard that comes with an integration follows the integration as
// long as nobody changed it; after that the newer version is only offered.
func TestProvidedDashboardUpdates(t *testing.T) {
	_, central := startServer(t, "http://127.0.0.1:1")
	c := newClient(t, central.URL)
	if code := c.login("admin", "admin-token-secret"); code != 204 {
		t.Fatalf("login: %d", code)
	}
	integration := func(panel string) {
		t.Helper()
		doc := fmt.Sprintf("title: Shop\ndescription: test\ndetect: {process: [shopd]}\nconfig: {labels: {shop: \"yes\"}}\n"+
			"dashboard:\n  title: Shop\n  panels:\n    - {title: %s, type: stat, w: 3, h: 2, queries: [{expr: up}]}\n", panel)
		if code, body, _ := c.do("PUT", "/api/v1/fleet/integrations/shop", doc); code/100 != 2 {
			t.Fatalf("save integration: %d %s", code, body)
		}
	}
	type row struct {
		ID, Title, Source string
		Update            bool
	}
	list := func() map[string]row {
		t.Helper()
		_, body, _ := c.do("GET", "/api/v1/dashboards", "")
		var rows []row
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		out := map[string]row{}
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	panel := func(id string) string {
		_, body, _ := c.do("GET", "/api/v1/dashboards/"+id, "")
		var d struct{ Panels []struct{ Title string } }
		json.Unmarshal([]byte(body), &d)
		if len(d.Panels) == 0 {
			return ""
		}
		return d.Panels[0].Title
	}
	update := func(id, body string) int {
		code, _, _ := c.do("POST", "/api/v1/dashboards/"+id+"/update", body)
		return code
	}

	integration("One")
	if _, ok := list()["shop"]; ok {
		t.Fatal("installed before any profile uses the integration")
	}
	if code, body, _ := c.do("PUT", "/api/v1/fleet/profiles/shop", "integrations: [shop]\n"); code != 200 {
		t.Fatalf("profile: %d %s", code, body)
	}
	if r := list()["shop"]; panel("shop") != "One" || r.Source != "integration shop" || r.Update {
		t.Fatalf("after first use: %+v, panel %q", r, panel("shop"))
	}
	if r := list()["hosts"]; r.Source != "starter" || r.Update {
		t.Errorf("starter: %+v", r)
	}

	// Untouched: a newer version simply replaces it.
	integration("Two")
	if r := list()["shop"]; panel("shop") != "Two" || r.Update {
		t.Fatalf("untouched dashboard was not updated: %+v, panel %q", r, panel("shop"))
	}

	// Changed by the user: left alone, the newer version is offered.
	mine := `{"title":"Shop","panels":[{"title":"Mine","type":"stat","w":3,"h":2,"queries":[{"expr":"up"}]}]}`
	if code, body, _ := c.do("PUT", "/api/v1/dashboards/shop", mine); code != 204 {
		t.Fatalf("edit: %d %s", code, body)
	}
	if r := list()["shop"]; r.Update {
		t.Errorf("an edit alone is not an update on offer: %+v", r)
	}
	if code := update("shop", `{"action":"apply"}`); code != 409 {
		t.Errorf("apply with nothing on offer: %d", code)
	}
	integration("Three")
	if r := list()["shop"]; panel("shop") != "Mine" || !r.Update {
		t.Fatalf("edited dashboard: %+v, panel %q", r, panel("shop"))
	}
	if code, body, _ := c.do("GET", "/api/v1/dashboards/shop/update", ""); code != 200 || !strings.Contains(body, `"Three"`) {
		t.Errorf("the version on offer: %d %s", code, body)
	}
	// Declined: no longer offered, until there is yet another version.
	if code := update("shop", `{"action":"keep"}`); code != 204 {
		t.Fatalf("keep: %d", code)
	}
	if r := list()["shop"]; panel("shop") != "Mine" || r.Update {
		t.Errorf("after keep: %+v", r)
	}
	integration("Four")
	if r := list()["shop"]; !r.Update {
		t.Fatalf("the next version should be offered again: %+v", r)
	}
	// Taken, with the user's version kept as a copy.
	if code := update("shop", `{"action":"apply","copy":true}`); code != 204 {
		t.Fatalf("apply: %d", code)
	}
	l := list()
	if panel("shop") != "Four" || l["shop"].Update || panel("shop-mine") != "Mine" || l["shop-mine"].Title != "Shop (mine)" {
		t.Fatalf("after apply: %+v, panels %q %q", l, panel("shop"), panel("shop-mine"))
	}
	// Having taken it, the dashboard follows the integration again.
	integration("Five")
	if panel("shop") != "Five" {
		t.Errorf("after taking the update it should follow again: %q", panel("shop"))
	}

	// Deleted stays deleted, through new versions too; it can be put back.
	c.do("DELETE", "/api/v1/dashboards/shop", "")
	integration("Six")
	if _, ok := list()["shop"]; ok {
		t.Fatal("a deleted dashboard came back")
	}
	if code := update("shop", `{"action":"restore"}`); code != 204 || panel("shop") != "Six" {
		t.Errorf("restore: %d, panel %q", code, panel("shop"))
	}
	if code := update("shop", `{"action":"explode"}`); code != 400 {
		t.Errorf("unknown action: %d", code)
	}
	if code := update("nothing-like-it", `{"action":"apply"}`); code != 409 {
		t.Errorf("update of a dashboard vigil does not provide: %d", code)
	}
}
