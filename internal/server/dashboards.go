package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

//go:embed dashboards/*.json
var starterDashboards embed.FS

const (
	settingSeeded    = "dashboards_seeded"
	maxDashboardBody = 1 << 20
	maxPanels        = 100
)

var (
	dashboardIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	variableRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// Dashboard is a grid of panels sharing one time range and set of variables.
type Dashboard struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Variables []Variable `json:"variables,omitempty"`
	Panels    []Panel    `json:"panels"`
}

// Variable is a drop-down above the dashboard. Its choices are the values
// of Label on the series matching Match; queries refer to it as $name.
type Variable struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Match string `json:"match,omitempty"`
}

type Panel struct {
	Title string `json:"title"`
	Type  string `json:"type"` // timeseries, stat, table, logs or map
	// Source says what the queries are: "metrics" (PromQL, the default) or
	// "logs" (LogsQL ending in | stats ...). A logs panel is always LogsQL.
	Source  string  `json:"source,omitempty"`
	W       int     `json:"w"` // width in twelfths of the row
	H       int     `json:"h"` // height in rows
	Unit    string  `json:"unit,omitempty"`
	Queries []Query `json:"queries"`

	Min   *float64 `json:"min,omitempty"`   // timeseries: fix the bottom of the y axis
	Fill  bool     `json:"fill,omitempty"`  // timeseries: fill the area under each line
	Warn  *float64 `json:"warn,omitempty"`  // stat: yellow from this value
	Crit  *float64 `json:"crit,omitempty"`  // stat: red from this value
	Limit int      `json:"limit,omitempty"` // logs: number of lines
	// table: the labels shown as columns, in this order; all of them if
	// empty. map: the labels a node's card shows.
	Columns []string `json:"columns,omitempty"`
}

// Query is one expression. In a table with several queries each one is a
// column: rows are the series, matched across the queries by their labels.
type Query struct {
	Expr   string `json:"expr"`
	Legend string `json:"legend,omitempty"` // e.g. "{{agent}} in"; in a table, the column's title
	// table: this column's unit (default: the panel's) and when a cell is
	// yellow and red. With crit below warn, low values are the bad ones.
	Unit string   `json:"unit,omitempty"`
	Warn *float64 `json:"warn,omitempty"`
	Crit *float64 `json:"crit,omitempty"`

	// map: each query is nodes, links or details. Nodes: every series is a
	// node whose id is the value of label Node, drawn in the column Tier,
	// named by label Name (default the id), under the value of label Group;
	// the value is its health, coloured by Warn and Crit. Links: every
	// series joins the node named by label From to the one named by label
	// To; at zero the link is drawn faint, and with Live a value above
	// zero moves (sessions open now). Details: lines on the card of the
	// node named by label Of, coloured like a table cell; the worst colour
	// of a node and its details is the node's. A node is the label's name
	// and value together, so the same id under two labels is two nodes.
	Node  string `json:"node,omitempty"`
	Tier  string `json:"tier,omitempty"`
	Name  string `json:"name,omitempty"`
	Group string `json:"group,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Live  bool   `json:"live,omitempty"`
	Of    string `json:"of,omitempty"`
}

var (
	panelTypes = map[string]bool{"timeseries": true, "stat": true, "table": true, "logs": true, "map": true}
	units      = map[string]bool{"": true, "percent": true, "ratio": true, "bytes": true, "bytes/s": true,
		"seconds": true, "ms": true, "cores": true, "ops/s": true, "yesno": true}
)

// parseDashboard reads and checks a dashboard definition. Unknown fields
// are errors, so a typo in hand-edited JSON does not silently do nothing.
func parseDashboard(body []byte) (*Dashboard, error) {
	d := &Dashboard{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(d); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Title) == "" || len(d.Title) > 100 {
		return nil, fmt.Errorf("title is required (up to 100 characters)")
	}
	if len(d.Panels) > maxPanels {
		return nil, fmt.Errorf("a dashboard can have at most %d panels", maxPanels)
	}
	seen := map[string]bool{}
	for _, v := range d.Variables {
		if !variableRe.MatchString(v.Name) || seen[v.Name] {
			return nil, fmt.Errorf("variable %q: names are lower-case letters, digits and _, and must be unique", v.Name)
		}
		seen[v.Name] = true
		if v.Label == "" {
			return nil, fmt.Errorf("variable %q: label is required", v.Name)
		}
	}
	for i := range d.Panels {
		p := &d.Panels[i]
		where := fmt.Sprintf("panel %d (%q)", i+1, p.Title)
		if !panelTypes[p.Type] {
			return nil, fmt.Errorf("%s: type must be timeseries, stat, table, logs or map", where)
		}
		if p.Type == "map" && p.Source == "logs" {
			return nil, fmt.Errorf("%s: a map's queries are PromQL", where)
		}
		if p.Source != "" && p.Source != "metrics" && p.Source != "logs" {
			return nil, fmt.Errorf("%s: source must be metrics or logs", where)
		}
		if !units[p.Unit] {
			return nil, fmt.Errorf("%s: unknown unit %q", where, p.Unit)
		}
		if p.W < 1 || p.W > 12 || p.H < 1 || p.H > 12 {
			return nil, fmt.Errorf("%s: w must be 1 to 12 and h 1 to 12", where)
		}
		// A map's nodes, links and details are each a query.
		most := 10
		if p.Type == "map" {
			most = 20
		}
		if len(p.Queries) == 0 || len(p.Queries) > most {
			return nil, fmt.Errorf("%s: needs between 1 and %d queries", where, most)
		}
		for _, q := range p.Queries {
			if strings.TrimSpace(q.Expr) == "" {
				return nil, fmt.Errorf("%s: a query is empty", where)
			}
			if !units[q.Unit] {
				return nil, fmt.Errorf("%s: unknown unit %q", where, q.Unit)
			}
			if p.Type != "table" && p.Type != "map" && (q.Unit != "" || q.Warn != nil || q.Crit != nil) {
				return nil, fmt.Errorf("%s: a unit and colours per query are for tables and maps", where)
			}
			if err := checkMapQuery(p.Type, q); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
		}
		if len(p.Columns) > 0 && p.Type != "table" && p.Type != "map" {
			return nil, fmt.Errorf("%s: columns are for tables and maps", where)
		}
		for _, c := range p.Columns {
			if !labelNameRe.MatchString(c) {
				return nil, fmt.Errorf("%s: column %q is not a label name", where, c)
			}
		}
		if p.Limit < 0 || p.Limit > 1000 {
			return nil, fmt.Errorf("%s: limit must be at most 1000", where)
		}
	}
	return d, nil
}

// checkMapQuery checks the fields that only a map's queries have.
func checkMapQuery(typ string, q Query) error {
	roles := 0
	for _, set := range []bool{q.Node != "", q.From != "" || q.To != "", q.Of != ""} {
		if set {
			roles++
		}
	}
	if typ != "map" {
		if roles > 0 || q.Tier != "" || q.Name != "" || q.Group != "" || q.Live {
			return errors.New("node, tier, name, group, from, to, live and of are for maps")
		}
		return nil
	}
	if roles != 1 {
		return fmt.Errorf("query %q: a map query is nodes (node and tier), links (from and to) or details (of)", q.Expr)
	}
	switch {
	case q.Node != "" && strings.TrimSpace(q.Tier) == "":
		return fmt.Errorf("query %q: nodes need a tier, the column they are drawn in", q.Expr)
	case (q.From == "") != (q.To == ""):
		return fmt.Errorf("query %q: a link needs both from and to", q.Expr)
	case q.Node == "" && (q.Tier != "" || q.Name != "" || q.Group != ""):
		return fmt.Errorf("query %q: tier, name and group are for nodes", q.Expr)
	case q.Live && q.From == "":
		return fmt.Errorf("query %q: live is for links", q.Expr)
	}
	for _, l := range []string{q.Node, q.Name, q.Group, q.From, q.To, q.Of} {
		if l != "" && !labelNameRe.MatchString(l) {
			return fmt.Errorf("query %q: %q is not a label name", q.Expr, l)
		}
	}
	return nil
}

func (s *Server) dashboardRoutes(mux *http.ServeMux) {
	s.providedRoutes(mux)
	mux.HandleFunc("GET /api/v1/dashboards", s.viewer(func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.store.Dashboards()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Say which ones come with vigil, and where a newer version is on offer.
		type item struct {
			DashboardRow
			Source string `json:"source,omitempty"`
			Update bool   `json:"update,omitempty"`
		}
		all := s.provided()
		out := make([]item, 0, len(list))
		for _, d := range list {
			it := item{DashboardRow: d}
			if p, ok := all[d.ID]; ok && s.providedState(d.ID, p) != "" {
				it.Source = p.source
				_, it.Update = s.providedUpdate(d.ID, all)
			}
			out = append(out, it)
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("GET /api/v1/dashboards/{id}", s.viewer(func(w http.ResponseWriter, r *http.Request) {
		body, found, err := s.store.Dashboard(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no such dashboard", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	mux.HandleFunc("PUT /api/v1/dashboards/{id}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !dashboardIDRe.MatchString(id) {
			http.Error(w, "dashboard ids are lower-case letters, digits and -", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDashboardBody))
		if err != nil {
			http.Error(w, "dashboard too large", http.StatusRequestEntityTooLarge)
			return
		}
		d, err := parseDashboard(body)
		if err != nil {
			http.Error(w, "invalid dashboard: "+err.Error(), http.StatusBadRequest)
			return
		}
		d.ID = id // the address decides, whatever the body says
		clean, _ := json.Marshal(d)
		if err := s.store.PutDashboard(id, d.Title, string(clean)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /api/v1/dashboards/{id}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.DeleteDashboard(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
