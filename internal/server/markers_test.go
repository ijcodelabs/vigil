package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Markers come from Boop's events that name the service or the work item,
// in time order, titled from bounded fields only, and one query serves the
// charts of a page.
func TestMarkers(t *testing.T) {
	var asked atomic.Value
	var calls atomic.Int32
	now := time.Now().UTC()
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/select/logsql/query") {
			io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		calls.Add(1)
		asked.Store(r.FormValue("query"))
		at := func(m int) string { return now.Add(-time.Duration(m) * time.Minute).Format(time.RFC3339Nano) }
		for _, row := range []map[string]string{
			{"_time": at(5), "event_name": "boop.item.moved", "work_item_key": "SHOP-1", "item_service": "shop-api", "from_category": "open", "to_category": "active"},
			{"_time": at(20), "event_name": "boop.task.started", "work_item_key": "SHOP-1", "mode": "investigate", "task_id": "t1"},
			{"_time": at(30), "event_name": "boop.item.opened", "work_item_key": "SHOP-1", "item_service": "shop-api", "item_type": "incident", "severity": "sev1",
				"title": "should never be shown"},
		} {
			b, _ := json.Marshal(row)
			fmt.Fprintln(w, string(b))
		}
	}))
	defer store.Close()
	_, central := startServer(t, store.URL)
	c := newClient(t, central.URL)
	c.csrf = false
	get := func(q string) (int, string) {
		code, body, _ := c.do("GET", "/api/v1/markers?"+q, "", "Authorization", "Bearer admin-token-secret")
		return code, body
	}
	for q, want := range map[string]int{"": 400, "service=Shop%20API": 400, "work_item=shop-1": 400, "service=a&work_item=X-1": 400,
		"service=shop-api&from=1&to=99999999": 400} {
		if code, _ := get(q); code != want {
			t.Errorf("%q: %d, want %d", q, code, want)
		}
	}

	code, body := get("service=shop-api")
	var res struct {
		Markers []struct {
			Time     time.Time
			Kind     string
			Title    string
			WorkItem string `json:"work_item"`
		}
		More bool
	}
	if json.Unmarshal([]byte(body), &res); code != 200 || len(res.Markers) != 3 {
		t.Fatalf("%d %s", code, body)
	}
	if !res.Markers[0].Time.Before(res.Markers[2].Time) || res.Markers[0].Title != "SHOP-1 opened (incident, sev1)" ||
		res.Markers[1].Title != "Investigation run started for SHOP-1" || res.Markers[2].Title != "SHOP-1 → active (from open)" {
		t.Errorf("markers: %+v", res.Markers)
	}
	if strings.Contains(body, "should never be shown") {
		t.Error("a free-text field went into a marker")
	}
	q, _ := asked.Load().(string)
	if !strings.Contains(q, `logger:="boop.audit"`) || !strings.Contains(q, `item_service:="shop-api"`) || !strings.Contains(q, `event_name:in("boop.item.opened"`) {
		t.Errorf("query: %s", q)
	}
	// The other charts of the page: from the cache.
	get("service=shop-api")
	get("service=shop-api")
	if n := calls.Load(); n != 1 {
		t.Errorf("%d queries for one page", n)
	}
	if code, _ := get("work_item=SHOP-1"); code != 200 {
		t.Errorf("by work item: %d", code)
	}
	if q, _ := asked.Load().(string); !strings.Contains(q, `work_item_key:="SHOP-1"`) {
		t.Errorf("query by work item: %s", q)
	}
	// By vigil's incident: the item whose investigation read it, then its markers.
	code, body = get("incident=7")
	if code != 200 || !strings.Contains(body, `"work_item":"SHOP-1"`) {
		t.Errorf("by incident: %d %s", code, body)
	}
	if code, _ := get("incident=x"); code != 400 {
		t.Errorf("a bad incident id: %d", code)
	}
}
