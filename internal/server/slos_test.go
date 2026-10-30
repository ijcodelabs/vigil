package server_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSLOs(t *testing.T) {
	stores := httptest.NewServer(&alertStores{})
	defer stores.Close()
	_, central := startServer(t, stores.URL)
	token := newClient(t, central.URL)
	admin := func(method, path, body string) (int, string) {
		code, out, _ := token.do(method, path, body, "Authorization", "Bearer admin-token-secret")
		return code, out
	}
	for _, u := range [][2]string{{"ada", "admin"}, {"vera", "viewer"}} {
		if code, body := admin("PUT", "/api/v1/users/"+u[0], `{"password":"a-long-password-1","role":"`+u[1]+`"}`); code != 204 {
			t.Fatalf("user %s: %d %s", u[0], code, body)
		}
	}
	vera := newClient(t, central.URL)
	if code := vera.login("vera", "a-long-password-1"); code != 204 {
		t.Fatalf("login: %d", code)
	}

	// Rules run once the server has started them; a saved rule file does.
	if code, body := admin("PUT", "/api/v1/rules/config", "groups: []"); code != 204 {
		t.Fatalf("rule file: %d %s", code, body)
	}
	doc := "objective: 99.5\nindicator: {probe: {name: shop-home}}\nowner: team-shop\n"
	code, body := admin("PUT", "/api/v1/slos/home", doc)
	if code != 200 || !strings.Contains(body, `"version":1`) || !strings.Contains(body, "no events in the last hour from") {
		t.Fatalf("save: %d %s", code, body)
	}
	// The same document again is not a new version.
	if code, body := admin("PUT", "/api/v1/slos/home", doc); code != 200 || !strings.Contains(body, `"changed":false`) || !strings.Contains(body, `"version":1`) {
		t.Fatalf("save unchanged: %d %s", code, body)
	}
	if code, body := admin("PUT", "/api/v1/slos/home", "objective: 99.9\nindicator: {probe: {name: shop-home}}\n"); code != 200 || !strings.Contains(body, `"version":2`) {
		t.Fatalf("save changed: %d %s", code, body)
	}

	// Refused: a document that does not parse, a query the store rejects,
	// and a viewer saving.
	if code, body := admin("PUT", "/api/v1/slos/bad", "objective: 120\nindicator: {probe: {name: x}}"); code != 400 || !strings.Contains(body, "below 100") {
		t.Errorf("bad objective: %d %s", code, body)
	}
	if code, _, _ := vera.do("PUT", "/api/v1/slos/other", doc); code != 403 {
		t.Errorf("a viewer saved an SLO: %d", code)
	}

	// Viewers see the list and the generated rules.
	_, body, _ = vera.do("GET", "/api/v1/slos", "")
	var list []struct {
		Name, Kind, Window, Origin, Doc string
		Objective                       float64
		Version                         int
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) != 1 {
		t.Fatalf("list: %v %s", err, body)
	}
	if l := list[0]; l.Name != "home" || l.Kind != "probe" || l.Window != "30d" || l.Objective != 99.9 || l.Version != 2 || l.Doc != "" {
		t.Errorf("listed: %+v", l)
	}
	_, body, _ = vera.do("GET", "/api/v1/slos/home", "")
	if !strings.Contains(body, `slo_good_per_minute`) || !strings.Contains(body, `vector(0.999)`) || !strings.Contains(body, `"doc":"objective: 99.9`) {
		t.Errorf("one SLO: %s", body)
	}
	if _, body := admin("GET", "/api/v1/rules", ""); !strings.Contains(body, `"slo:home"`) {
		t.Errorf("the SLO's rule group does not run: %s", body)
	}
	_, body, _ = vera.do("GET", "/api/v1/slos/home/versions", "")
	if !strings.Contains(body, `"version":2`) || !strings.Contains(body, `"version":1`) {
		t.Errorf("versions: %s", body)
	}
	if _, body, _ := vera.do("GET", "/api/v1/slos/home/versions/1", ""); !strings.Contains(body, "99.5") {
		t.Errorf("version 1: %s", body)
	}

	// Deleting stops its rules and is recorded.
	if code, body := admin("DELETE", "/api/v1/slos/home", ""); code != 204 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if _, body := admin("GET", "/api/v1/rules", ""); strings.Contains(body, `"slo:home"`) {
		t.Errorf("the rules of a deleted SLO still run: %s", body)
	}
	if _, body, _ := vera.do("GET", "/api/v1/slos/home/versions", ""); !strings.Contains(body, `"deleted":true`) {
		t.Errorf("the deletion is not in the history: %s", body)
	}
	if code, _ := admin("DELETE", "/api/v1/slos/home", ""); code != 404 {
		t.Errorf("deleting again: %d", code)
	}
}
