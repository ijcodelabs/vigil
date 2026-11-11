package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

// What the model calls in stored spans cost: of one trace (one request, an
// agent's one action), or of everything done for one work item (a Portun
// item, a ticket) whose key the spans carry as `work_item.key`, on the span
// or on the resource. Priced with the server's `costs.tokens`.
//
// It is an estimate. It sees only stored spans (agents keep every span that
// reports tokens, whatever the sample share, but an agent older than that
// does not), prices what the price list says rather than what the invoice
// will, and a model without a price counts its tokens and no money.

const (
	workItemAttr   = "work_item.key"
	spanCostMaxAge = 31 * 24 * time.Hour
)

var workItemKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,127}$`)

// The token fields as the trace store keeps them, newest names first; the
// first one a group has a number in counts.
var spanTokenFields = map[string][]string{
	"input":       {"gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.usage.prompt_tokens"},
	"output":      {"gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.usage.completion_tokens"},
	"cache_read":  {"gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_read_input_tokens", "gen_ai.usage.cached_tokens"},
	"cache_write": {"gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_creation_input_tokens"},
}

type modelCost struct {
	Model   string             `json:"model"`
	Calls   int                `json:"calls"`
	Tokens  map[string]float64 `json:"tokens"`
	CostUSD float64            `json:"cost_usd"`
	Priced  bool               `json:"priced"`
}

type spanCost struct {
	Trace    string      `json:"trace,omitempty"`
	WorkItem string      `json:"work_item,omitempty"`
	CostUSD  float64     `json:"cost_usd"`
	Calls    int         `json:"calls"`
	Models   []modelCost `json:"models"`
	Limits   []string    `json:"limits"`
}

func (s *Server) spanCostRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/costs/spans", s.viewer(s.handleSpanCost))
}

func (s *Server) handleSpanCost(w http.ResponseWriter, r *http.Request) {
	if s.traces == nil {
		http.Error(w, "this server has no trace store; set storage.traces_url", http.StatusNotImplemented)
		return
	}
	q := r.URL.Query()
	trace, item := q.Get("trace"), q.Get("work_item")
	now := time.Now()
	from, to := now.Add(-7*24*time.Hour), now
	var filter string
	switch {
	case (trace == "") == (item == ""):
		http.Error(w, "give either trace or work_item", http.StatusBadRequest)
		return
	case trace != "":
		if !traceIDRe.MatchString(trace) {
			http.Error(w, "not a trace id", http.StatusBadRequest)
			return
		}
		filter = "trace_id:=" + strconv.Quote(strings.ToLower(trace))
		from = now.Add(-spanCostMaxAge)
	default:
		if !workItemKeyRe.MatchString(item) {
			http.Error(w, "a work item key is letters, digits and ._:/#-, at most 128", http.StatusBadRequest)
			return
		}
		k := strconv.Quote(item)
		filter = fmt.Sprintf(`("span_attr:%s":=%s OR "resource_attr:%s":=%s)`, workItemAttr, k, workItemAttr, k)
	}
	for name, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				http.Error(w, name+" is Unix seconds", http.StatusBadRequest)
				return
			}
			*dst = time.Unix(int64(f), 0)
		}
	}
	if !from.Before(to) || to.Sub(from) > spanCostMaxAge {
		http.Error(w, "from must be before to, at most 31 days apart", http.StatusBadRequest)
		return
	}
	res, err := s.spanCost(r.Context(), filter, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	res.Trace, res.WorkItem = trace, item
	writeJSON(w, res)
}

func (s *Server) spanCost(ctx context.Context, filter string, from, to time.Time) (spanCost, error) {
	sums := []string{`count() calls`}
	for typ, fields := range spanTokenFields {
		for i, f := range fields {
			sums = append(sums, fmt.Sprintf(`sum("span_attr:%s") %s_%d`, f, typ, i))
		}
	}
	query := filter + ` ("span_attr:gen_ai.response.model":* OR "span_attr:gen_ai.request.model":*)` +
		` | stats by ("span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model") ` + strings.Join(sums, ", ")
	rows, err := s.storeQuery(ctx, s.traces, query, from, to, 1000)
	if err != nil {
		return spanCost{}, err
	}
	return priceSpanRows(rows, s.cfg.Costs.Tokens), nil
}

// priceSpanRows turns the store's groups (one per model name pair) into
// costs per model.
func priceSpanRows(rows []map[string]string, prices []config.TokenPrice) spanCost {
	byModel := map[string]*modelCost{}
	for _, row := range rows {
		name := row["span_attr:gen_ai.response.model"]
		if name == "" {
			name = row["span_attr:gen_ai.request.model"]
		}
		m := byModel[name]
		if m == nil {
			m = &modelCost{Model: name, Tokens: map[string]float64{}}
			byModel[name] = m
		}
		calls, _ := strconv.Atoi(row["calls"])
		m.Calls += calls
		for typ, fields := range spanTokenFields {
			for i := range fields {
				if v, err := strconv.ParseFloat(row[fmt.Sprintf("%s_%d", typ, i)], 64); err == nil && v > 0 {
					m.Tokens[typ] += v
					break
				}
			}
		}
	}
	res := spanCost{Models: []modelCost{}, Limits: []string{
		"An estimate from stored spans at the prices in costs.tokens, not the provider's invoice.",
	}}
	unpriced := false
	for _, m := range byModel {
		if p, ok := tokenPrice(prices, m.Model); ok {
			m.Priced = true
			m.CostUSD = (m.Tokens["input"]*p.Input + m.Tokens["output"]*p.Output +
				m.Tokens["cache_read"]*p.CacheRead + m.Tokens["cache_write"]*p.CacheWrite) / 1e6
		} else {
			unpriced = true
		}
		res.CostUSD += m.CostUSD
		res.Calls += m.Calls
		res.Models = append(res.Models, *m)
	}
	sort.Slice(res.Models, func(i, j int) bool { return res.Models[i].CostUSD > res.Models[j].CostUSD })
	if unpriced {
		res.Limits = append(res.Limits, "Some models have no price in costs.tokens; their tokens are counted, their cost is not.")
	}
	return res
}
