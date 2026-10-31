package server

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/fleet"
	"github.com/ijcodelabs/vigil/internal/slo"
)

// SLO templates: SLOs the SLOs page offers for what exists, one for each
// check, traced service or HAProxy backend. vigil's own are in
// slo_templates.yaml; integrations in use add theirs (fleet.SLOTemplate).
// An offer is a filled-in document for the admin to look at and save;
// nothing is made by itself.

//go:embed slo_templates.yaml
var builtinSLOTemplatesYAML string

var builtinSLOTemplates = func() []fleet.SLOTemplate {
	var list []fleet.SLOTemplate
	if err := yaml.Unmarshal([]byte(builtinSLOTemplatesYAML), &list); err != nil {
		panic("slo_templates.yaml: " + err.Error())
	}
	if err := fleet.CheckSLOTemplates(list); err != nil {
		panic("slo_templates.yaml: " + err.Error())
	}
	return list
}()

// maxOffers caps the offers of one template: a label with hundreds of
// values (services in a large fleet) is better served from git.
const maxOffers = 50

// SLOOffer is one template filled in for one value.
type SLOOffer struct {
	Template    string `json:"template"` // "vigil/check", "haproxy/backend-errors"
	Source      string `json:"source"`   // "vigil" or the integration's title
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Value       string `json:"value"`
	Name        string `json:"name"`
	Doc         string `json:"doc"`
	Exists      bool   `json:"exists"` // an SLO of that name exists
}

type sourcedTemplate struct {
	prefix, source string
	fleet.SLOTemplate
}

func (s *Server) sloTemplates() []sourcedTemplate {
	var out []sourcedTemplate
	for _, t := range builtinSLOTemplates {
		out = append(out, sourcedTemplate{"vigil", "vigil", t})
	}
	f := &s.fleet
	f.mu.Lock()
	used, catalog := f.usedIntegrations(), f.catalog
	f.mu.Unlock()
	sort.Strings(used)
	for _, name := range used {
		if in := catalog[name]; in != nil {
			for _, t := range in.SLOs {
				out = append(out, sourcedTemplate{name, in.Title, t})
			}
		}
	}
	return out
}

func (s *Server) sloOffers(ctx context.Context) ([]SLOOffer, error) {
	rows, err := s.store.SLOs(defaultTenant)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Name] = true
	}
	out := []SLOOffer{}
	for _, t := range s.sloTemplates() {
		// What reported in the last day: a backend idle overnight still counts.
		series, err := ruleBackend{s}.Query(ctx, "", fmt.Sprintf("group by (%s) (last_over_time(%s[1d]))", t.Each.Label, t.Each.Match), time.Now())
		if err != nil {
			return nil, err
		}
		var values []string
		for _, sr := range series {
			if v := sr.Labels[t.Each.Label]; fleet.TemplateValueRe.MatchString(v) {
				values = append(values, v)
			}
		}
		sort.Strings(values)
		if len(values) > maxOffers {
			values = values[:maxOffers]
		}
		for _, v := range values {
			name, doc := t.Fill(v)
			if _, err := slo.Parse(name, doc); err != nil {
				s.log.Warn("SLO template makes an SLO that does not parse", "template", t.prefix+"/"+t.ID, "value", v, "err", err)
				continue
			}
			out = append(out, SLOOffer{Template: t.prefix + "/" + t.ID, Source: t.source, Title: t.Title, Description: t.Description,
				Value: v, Name: name, Doc: doc, Exists: have[name]})
		}
	}
	return out, nil
}

func (s *Server) sloTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/slos/templates", s.admin(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.sloOffers(r.Context())
		if err != nil {
			http.Error(w, "the metric store did not answer: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, list)
	}))
}
