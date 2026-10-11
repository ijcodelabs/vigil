package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
	"github.com/ijcodelabs/vigil/internal/rules"
)

const (
	settingRules    = "rules"
	settingAlerting = "alerting"
	maxConfigBody   = 4 << 20
)

// startAlerting brings up the embedded Alertmanager with the saved config.
func (s *Server) startAlerting() error {
	am, err := alerting.New(filepath.Join(s.cfg.DataDir, "alertmanager"), s.cfg.ExternalURL, s.log)
	if err != nil {
		return fmt.Errorf("alertmanager: %w", err)
	}
	conf, err := s.store.Setting(settingAlerting, alerting.DefaultConfig)
	if err == nil {
		err = am.Apply(conf)
	}
	if err != nil {
		am.Close()
		return fmt.Errorf("alerting config: %w", err)
	}
	s.am = am
	s.rules = rules.NewManager(ruleBackend{s}, s.cfg.ExternalURL, s.log)
	return nil
}

// loadRules starts evaluating the saved rule file and the rules of the
// integrations in use.
func (s *Server) loadRules(ctx context.Context) error {
	s.runCtx = ctx
	return s.applyRules(ctx)
}

func (s *Server) alertingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/rules/config", s.viewer(s.getSetting(settingRules, "groups: []\n")))
	mux.HandleFunc("PUT /api/v1/rules/config", s.admin(s.putRules))
	mux.HandleFunc("GET /api/v1/alerting/config", s.admin(s.getSetting(settingAlerting, alerting.DefaultConfig)))
	mux.HandleFunc("PUT /api/v1/alerting/config", s.admin(s.putAlerting))

	mux.HandleFunc("GET /api/v1/rules", s.viewer(func(w http.ResponseWriter, _ *http.Request) {
		groups := s.rules.Status()
		if groups == nil {
			groups = []rules.GroupStatus{}
		}
		writeJSON(w, map[string]any{"status": "success", "data": map[string]any{"groups": groups}})
	}))
	mux.HandleFunc("GET /api/v1/alerts", s.viewer(func(w http.ResponseWriter, _ *http.Request) {
		alerts := []rules.AlertStatus{}
		for _, g := range s.rules.Status() {
			for _, r := range g.Rules {
				alerts = append(alerts, r.Alerts...)
			}
		}
		writeJSON(w, map[string]any{"status": "success", "data": map[string]any{"alerts": alerts}})
	}))

	// Alertmanager's own API v2: silences, alert groups, receivers, status.
	// Reading is open to viewers; changing silences needs an admin.
	am := s.am.Handler().ServeHTTP
	mux.HandleFunc("GET "+alerting.RoutePrefix+"/", s.viewer(am))
	mux.HandleFunc(alerting.RoutePrefix+"/", s.admin(am))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) getSetting(key, def string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		v, err := s.store.Setting(key, def)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		io.WriteString(w, v)
	}
}

func readConfigBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return "", false
	}
	return string(b), true
}

// putRules replaces the whole rule file. It is refused, and the running
// rules stay as they are, if it does not parse or a store rejects an
// expression.
func (s *Server) putRules(w http.ResponseWriter, r *http.Request) {
	text, ok := readConfigBody(w, r)
	if !ok {
		return
	}
	f, err := rules.Parse(text)
	if err != nil {
		http.Error(w, "invalid rules: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.rules.Check(r.Context(), f); err != nil {
		var qe *rules.QueryError
		if errors.As(err, &qe) && qe.Bad {
			http.Error(w, "invalid rules: "+err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "rules not saved, their expressions could not be checked: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := s.store.SetSetting(settingRules, text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.applyRules(s.runCtx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putAlerting replaces the Alertmanager configuration (routes, receivers,
// inhibit rules, time intervals).
func (s *Server) putAlerting(w http.ResponseWriter, r *http.Request) {
	text, ok := readConfigBody(w, r)
	if !ok {
		return
	}
	if err := s.am.Apply(text); err != nil {
		http.Error(w, "invalid alerting config: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SetSetting(settingAlerting, text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ruleBackend gives the rule manager its view of the server.
type ruleBackend struct{ s *Server }

func (b ruleBackend) Notify(ctx context.Context, alerts []alerting.Alert) error {
	return b.s.am.Send(ctx, alerts)
}

func (b ruleBackend) Record(ctx context.Context, samples []model.Sample) error {
	return b.s.forward(ctx, "", promwire.Encode(samples))
}

func (b ruleBackend) LoadAlertStates() (map[string][]byte, error) { return b.s.store.LoadAlertStates() }
func (b ruleBackend) SaveAlertState(k string, d []byte) error     { return b.s.store.SaveAlertState(k, d) }
func (b ruleBackend) PruneAlertStates(keep map[string]bool) error {
	return b.s.store.PruneAlertStates(keep)
}

// Query runs an instant query. Metric rules are PromQL against the metric
// store; log rules are LogsQL ending in `| stats ...` against the log store,
// which answers in the same vector format.
func (b ruleBackend) Query(ctx context.Context, typ, expr string, at time.Time) ([]rules.Series, error) {
	u := b.s.metrics.JoinPath("/api/v1/query")
	if typ == rules.TypeLogs {
		u = b.s.logs.JoinPath("/select/logsql/stats_query")
	}
	u.RawQuery = url.Values{"query": {expr}, "time": {strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.s.http.Do(req)
	if err != nil {
		return nil, &rules.QueryError{Msg: "store unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		// 4xx: the store understood the request and refused the expression.
		return nil, &rules.QueryError{Bad: resp.StatusCode/100 == 4, Msg: fmt.Sprintf("query refused (HTTP %d): %s", resp.StatusCode, msg)}
	}
	var res struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     json.RawMessage
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, &rules.QueryError{Msg: "unreadable query response: " + err.Error()}
	}
	parse := func(v [2]any) (float64, error) {
		str, _ := v[1].(string)
		return strconv.ParseFloat(str, 64)
	}
	switch res.Data.ResultType {
	case "vector":
		var rows []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(res.Data.Result, &rows); err != nil {
			return nil, &rules.QueryError{Msg: "unreadable query result: " + err.Error()}
		}
		out := make([]rules.Series, 0, len(rows))
		for _, row := range rows {
			v, err := parse(row.Value)
			if err != nil {
				continue
			}
			out = append(out, rules.Series{Labels: row.Metric, Value: v})
		}
		return out, nil
	case "scalar":
		var val [2]any
		if err := json.Unmarshal(res.Data.Result, &val); err != nil {
			return nil, &rules.QueryError{Msg: "unreadable query result: " + err.Error()}
		}
		v, err := parse(val)
		if err != nil {
			return nil, &rules.QueryError{Msg: "unreadable scalar result"}
		}
		return []rules.Series{{Labels: map[string]string{}, Value: v}}, nil
	}
	return nil, &rules.QueryError{Bad: true, Msg: fmt.Sprintf("expression returns a %s; a rule needs an instant vector", res.Data.ResultType)}
}
