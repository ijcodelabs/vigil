package fleet

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/events"
	"github.com/ijcodelabs/vigil/internal/logs"
)

// keyed lists are merged item by item, matched on the named field, instead
// of being replaced as a whole. An item with `enabled: false` is removed
// from the result.
var keyed = map[string]string{
	"scrape.static":  "job",
	"logs.files":     "name",
	"security.rules": "name",
	"probes":         "name",
	"processes":      "name",
}

// Origin says which profiles contributed one part of a resolved config.
type Origin struct {
	Path     string   `json:"path"`     // e.g. interval, scrape.static[node], logs.templates.nginx
	Profiles []string `json:"profiles"` // in merge order; the last one decided
}

// Resolved is the config of one agent.
type Resolved struct {
	YAML     string   // empty when no profile applies: the agent uses its own file
	Hash     string   // config.Hash(YAML)
	Profiles []string // the profiles merged, in order
	// Integrations switched on for the agent by those profiles.
	Integrations []string
	Origins      []Origin
}

// Resolve merges the matching profiles and validates the result exactly as
// the agent will.
func Resolve(profiles []*Profile, catalog Catalog, attrs map[string]string) (*Resolved, error) {
	matched := Matching(profiles, attrs)
	r := &Resolved{}
	if len(matched) == 0 {
		return r, nil
	}
	merged := map[string]any{}
	origins := map[string][]string{}
	used := map[string]bool{}
	for _, p := range matched {
		r.Profiles = append(r.Profiles, p.Name)
		names := make([]string, 0, len(p.Integrations))
		for name := range p.Integrations {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			in := catalog[name]
			if in == nil {
				return nil, fmt.Errorf("profile %s uses integration %q, which does not exist", p.Name, name)
			}
			frag, err := in.Fragment(p.Integrations[name])
			if err != nil {
				return nil, fmt.Errorf("profile %s: %w", p.Name, err)
			}
			mergeMap(merged, frag, "", name+" (integration)", origins)
			if !used[name] {
				used[name] = true
				r.Integrations = append(r.Integrations, name)
			}
		}
		mergeMap(merged, clone(p.Config).(map[string]any), "", p.Name, origins)
	}
	dropDisabled(merged, origins)

	out, err := yaml.Marshal(merged) // map keys are written sorted, so the text is stable
	if err != nil {
		return nil, err
	}
	r.YAML = string(out)
	r.Hash = config.Hash(r.YAML)
	for path, names := range origins {
		r.Origins = append(r.Origins, Origin{Path: path, Profiles: names})
	}
	sort.Slice(r.Origins, func(i, j int) bool { return r.Origins[i].Path < r.Origins[j].Path })

	c, err := config.ParseCollection(r.YAML)
	if err == nil {
		err = c.Check()
	}
	if err == nil {
		err = logs.Validate(c)
	}
	if err == nil {
		err = events.Validate(&c.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("profiles %s together give an invalid config: %w", strings.Join(r.Profiles, ", "), err)
	}
	return r, nil
}

func clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clone(e)
		}
		return out
	}
	return v
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// note records that profile set the value at path. Deep paths are recorded
// at the level a person thinks in: a top-level setting, a field of a
// section, or one named item.
func note(origins map[string][]string, path, profile string) {
	if names := origins[path]; len(names) == 0 || names[len(names)-1] != profile {
		origins[path] = append(origins[path], profile)
	}
}

// mergeMap merges src into dst. Maps merge key by key, keyed lists item by
// item; everything else is replaced by the later profile.
func mergeMap(dst, src map[string]any, path, profile string, origins map[string][]string) {
	for k, sv := range src {
		p := join(path, k)
		switch s := sv.(type) {
		case map[string]any:
			d, ok := dst[k].(map[string]any)
			if !ok {
				d = map[string]any{}
				dst[k] = d
			}
			if strings.Count(p, ".") >= 2 || isLeafMap(p) {
				// e.g. logs.templates.nginx or labels: one unit to a person
				note(origins, p, profile)
				mergeMap(d, s, p, profile, map[string][]string{})
			} else {
				mergeMap(d, s, p, profile, origins)
			}
		case []any:
			field, isKeyed := keyed[p]
			if !isKeyed {
				dst[k] = s
				note(origins, p, profile)
				continue
			}
			existing, _ := dst[k].([]any)
			dst[k] = mergeKeyed(existing, s, field, p, profile, origins)
		default:
			dst[k] = sv
			note(origins, p, profile)
		}
	}
}

// isLeafMap marks maps that are a single setting rather than a section.
func isLeafMap(path string) bool { return path == "labels" }

func mergeKeyed(dst, src []any, field, path, profile string, origins map[string][]string) []any {
	index := map[string]int{}
	for i, item := range dst {
		if m, ok := item.(map[string]any); ok {
			if key, _ := m[field].(string); key != "" {
				index[key] = i
			}
		}
	}
	for _, item := range src {
		m, ok := item.(map[string]any)
		key := ""
		if ok {
			key, _ = m[field].(string)
		}
		if key == "" { // an item without a name cannot be matched; it is added
			dst = append(dst, item)
			note(origins, path+"[]", profile)
			continue
		}
		note(origins, path+"["+key+"]", profile)
		if i, found := index[key]; found {
			mergeMap(dst[i].(map[string]any), m, "", profile, map[string][]string{})
		} else {
			index[key] = len(dst)
			dst = append(dst, m)
		}
	}
	return dst
}

// dropDisabled removes keyed items that ended up with enabled: false, and
// the flag itself from the ones that stay.
func dropDisabled(merged map[string]any, origins map[string][]string) {
	for path := range keyed {
		section, name, nested := strings.Cut(path, ".")
		parent := merged
		if nested {
			var ok bool
			if parent, ok = merged[section].(map[string]any); !ok {
				continue
			}
		} else {
			name = section
		}
		list, ok := parent[name].([]any)
		if !ok {
			continue
		}
		kept := list[:0]
		for _, item := range list {
			m, isMap := item.(map[string]any)
			if isMap {
				if on, set := m["enabled"].(bool); set {
					delete(m, "enabled")
					if !on {
						continue
					}
				}
			}
			kept = append(kept, item)
		}
		parent[name] = kept
	}
}
