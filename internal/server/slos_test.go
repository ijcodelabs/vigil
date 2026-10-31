package server_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// Where it stands: 9995 good of 10000 at 99.9 % is half the budget
	// left; 10 bad in 1000 in the last hour burns at 10.
	_, body, _ = vera.do("GET", "/api/v1/slos?status=1", "")
	var withStatus struct {
		SLOs   []struct{ Name string }
		Status []struct {
			Name                           string
			Achieved, BudgetLeft, Coverage *float64
			Burn                           map[string]float64
			Firing                         []string
		}
	}
	if err := json.Unmarshal([]byte(strings.ReplaceAll(body, `"budget_left"`, `"budgetleft"`)), &withStatus); err != nil || len(withStatus.Status) != 1 {
		t.Fatalf("status: %v %s", err, body)
	}
	near := func(p *float64, want float64) bool { return p != nil && *p > want-1e-9 && *p < want+1e-9 }
	if st := withStatus.Status[0]; st.Name != "home" || !near(st.Achieved, 99.95) || !near(st.BudgetLeft, 0.5) ||
		len(st.Burn) != 1 || st.Burn["1h"] < 10-1e-9 || st.Burn["1h"] > 10+1e-9 || st.Coverage == nil || *st.Coverage < 0.99 || st.Firing == nil {
		t.Errorf("status: %s", body)
	}
	if _, body, _ := vera.do("GET", "/api/v1/slos/home", ""); !strings.Contains(body, `"budget_left":0.5`) || !strings.Contains(body, `"incidents":[]`) {
		t.Errorf("one SLO's status and incidents: %s", body)
	}
	// The SLA report of a closed month is worked out once and stored.
	_, body, _ = vera.do("GET", "/api/v1/slos/report?period=2026-08", "")
	var rep struct {
		Period  string
		Final   bool
		Created string
		SLOs    []map[string]any
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil || len(rep.SLOs) != 1 || !rep.Final {
		t.Fatalf("report: %v %s", err, body)
	}
	if r := rep.SLOs[0]; r["name"] != "home" || r["achieved"] != 99.95 || r["met"] != true || r["budget_used"].(float64) < 0.5-1e-9 ||
		r["budget_used"].(float64) > 0.5+1e-9 || r["coverage"] != 1.0 || r["counting"] != nil || fmt.Sprint(r["worst_day"]) != "map[achieved:99.9 bad:5 day:2026-08-01]" {
		t.Errorf("report row: %v", r)
	}
	if _, again, _ := vera.do("GET", "/api/v1/slos/report?period=2026-08", ""); again != body {
		t.Errorf("a stored report changed:\n%s\n%s", body, again)
	}
	if _, list, _ := vera.do("GET", "/api/v1/slos/reports", ""); !strings.Contains(list, `"period":"2026-08"`) {
		t.Errorf("stored reports: %s", list)
	}
	if _, csv, _ := vera.do("GET", "/api/v1/slos/report?period=2026-08&format=csv", ""); !strings.HasPrefix(csv, "period,final,slo,") ||
		!strings.Contains(csv, "\n2026-08,true,home,,,probe,99.9,99.95,true,9995,10000,5,") {
		t.Errorf("csv: %s", csv)
	}
	// The month in progress is not final and not stored.
	month := time.Now().UTC().Format("2006-01")
	if _, body, _ := vera.do("GET", "/api/v1/slos/report?period="+month, ""); !strings.Contains(body, `"final":false`) {
		t.Errorf("current month: %s", body)
	}
	if _, list, _ := vera.do("GET", "/api/v1/slos/reports", ""); strings.Contains(list, `"period":"`+month) {
		t.Errorf("the current month was stored: %s", list)
	}
	// A finished period without SLO counts is answered, not stored.
	if _, body, _ := vera.do("GET", "/api/v1/slos/report?period=2026-02", ""); !strings.Contains(body, `"final":true`) || !strings.Contains(body, `"slos":[]`) {
		t.Errorf("empty month: %s", body)
	}
	if _, list, _ := vera.do("GET", "/api/v1/slos/reports", ""); strings.Contains(list, "2026-02") {
		t.Errorf("an empty report was stored: %s", list)
	}
	if code, body, _ := vera.do("GET", "/api/v1/slos/report?period=2026-13", ""); code != 400 {
		t.Errorf("bad period: %d %s", code, body)
	}
	if code, body := admin("PUT", "/api/v1/slos/report", "objective: 99\nindicator: {probe: {name: x}}\n"); code != 400 || !strings.Contains(body, "taken by the API") {
		t.Errorf("an SLO called report: %d %s", code, body)
	}

	// Templates: one offer for each check that exists, filled in.
	code, body = admin("GET", "/api/v1/slos/templates", "")
	var offers []struct{ Template, Name, Doc, Value string }
	if err := json.Unmarshal([]byte(body), &offers); err != nil || code != 200 || len(offers) != 1 {
		t.Fatalf("templates: %d %v %s", code, err, body)
	}
	if o := offers[0]; o.Template != "vigil/check" || o.Name != "check-shop-home" || !strings.HasPrefix(o.Doc, "name: check-shop-home\n") ||
		!strings.Contains(o.Doc, `indicator: {probe: {name: "shop-home"}}`) {
		t.Errorf("offer: %+v", o)
	}
	if code, _, _ := vera.do("GET", "/api/v1/slos/templates", ""); code != 403 {
		t.Errorf("a viewer got the templates: %d", code)
	}

	if _, body := admin("GET", "/api/v1/rules", ""); !strings.Contains(body, `"slo:home"`) || !strings.Contains(body, `"slo:home:alerts"`) ||
		!strings.Contains(body, `"SLOBurnFast"`) || !strings.Contains(body, `"SLOBudgetExhausted"`) {
		t.Errorf("the SLO's rule groups do not run: %s", body)
	}
	// An objective too low for a page to ever fire is saved, with a warning.
	if code, body := admin("PUT", "/api/v1/slos/loose", "objective: 90\nindicator: {probe: {name: x}}\n"); code != 200 || !strings.Contains(body, "can never fire") {
		t.Errorf("save loose: %d %s", code, body)
	}
	admin("DELETE", "/api/v1/slos/loose", "")
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
	if _, body := admin("GET", "/api/v1/rules", ""); strings.Contains(body, `"slo:home`) {
		t.Errorf("the rules of a deleted SLO still run: %s", body)
	}
	if _, body, _ := vera.do("GET", "/api/v1/slos/home/versions", ""); !strings.Contains(body, `"deleted":true`) {
		t.Errorf("the deletion is not in the history: %s", body)
	}
	if code, _ := admin("DELETE", "/api/v1/slos/home", ""); code != 404 {
		t.Errorf("deleting again: %d", code)
	}
}
