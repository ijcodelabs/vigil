package fleet

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/rules"
)

// Integration is everything needed to monitor one technology: how to
// recognise it on a host, what to collect, which alerts to raise and a
// dashboard. A profile switches it on with
//
//	integrations:
//	  postgres: {metrics_target: "localhost:9187"}
type Integration struct {
	Name        string `yaml:"name,omitempty" json:"name"`
	Title       string `yaml:"title" json:"title"`
	Description string `yaml:"description" json:"description"`
	Detect      Detect `yaml:"detect" json:"detect"`
	// Params are the values a profile may set; every one has a default.
	// The config refers to them as ${name}.
	Params map[string]Param `yaml:"params" json:"params"`
	// Config is a fragment of agent config, as in a profile.
	Config map[string]any `yaml:"config" json:"-"`
	// Rules are alert and recording rule groups, in the rule-file format.
	Rules []map[string]any `yaml:"rules" json:"-"`
	// Dashboard is installed the first time the integration is used.
	Dashboard map[string]any `yaml:"dashboard" json:"-"`
	// Queries are offered in Explore under "Saved" while the integration
	// is in use. They cannot be changed there; a copy can be saved.
	Queries []Query `yaml:"queries" json:"-"`
	// SLOs are SLO templates the SLOs page offers while the integration
	// is in use, one for each value of a label.
	SLOs []SLOTemplate `yaml:"slos" json:"-"`

	Builtin bool `yaml:"-" json:"builtin"`
}

type Param struct {
	Default     string `yaml:"default" json:"default"`
	Description string `yaml:"description" json:"description"`
}

// Detect lists signs that the technology runs on a host. Any one is enough.
type Detect struct {
	Process []string `yaml:"process" json:"process,omitempty"` // process names
	Image   []string `yaml:"image" json:"image,omitempty"`     // container image names (without registry and tag)
	Port    []int    `yaml:"port" json:"port,omitempty"`       // listening TCP ports; a weak sign, used only if nothing else is listed
}

var (
	integrationNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	paramRe           = regexp.MustCompile(`\$\{([a-z][a-z0-9_]*)\}`)
)

// ParseIntegration reads and checks one integration document.
func ParseIntegration(name, text string) (*Integration, error) {
	if !integrationNameRe.MatchString(name) {
		return nil, errors.New("integration names are lower-case letters, digits and _, starting with a letter")
	}
	in := &Integration{}
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(in); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if in.Name != "" && in.Name != name {
		return nil, fmt.Errorf("the document says name: %s, but it is being saved as %s", in.Name, name)
	}
	in.Name = name
	if in.Title == "" {
		in.Title = name
	}
	// With the defaults filled in, the config must be a valid fragment.
	frag, err := in.Fragment(nil)
	if err != nil {
		return nil, err
	}
	out, _ := yaml.Marshal(frag)
	fdec := yaml.NewDecoder(bytes.NewReader(out))
	fdec.KnownFields(true)
	if err := fdec.Decode(&config.Collection{}); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	if _, err := in.RuleGroups(nil); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	if err := CheckQueries(in.Queries); err != nil {
		return nil, fmt.Errorf("queries: %w", err)
	}
	if err := CheckSLOTemplates(in.SLOs); err != nil {
		return nil, fmt.Errorf("slos: %w", err)
	}
	return in, nil
}

// Fragment returns the config with ${param} replaced by the given values,
// or by the defaults where none is given.
func (in *Integration) Fragment(params map[string]string) (map[string]any, error) {
	for k := range params {
		if _, ok := in.Params[k]; !ok {
			known := make([]string, 0, len(in.Params))
			for p := range in.Params {
				known = append(known, p)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("integration %s has no parameter %q (it has: %s)", in.Name, k, strings.Join(known, ", "))
		}
	}
	filled, missing := in.fill(in.Config, params)
	if missing != "" {
		return nil, fmt.Errorf("integration %s: config uses ${%s}, which is not declared under params", in.Name, missing)
	}
	frag, _ := filled.(map[string]any)
	if frag == nil {
		frag = map[string]any{}
	}
	return frag, nil
}

// fill replaces ${param} in every string of v (and in map keys) by the
// given value, or the parameter's default. missing names a parameter that
// is used but not declared.
func (in *Integration) fill(v any, params map[string]string) (out any, missing string) {
	sub := func(s string) string {
		return paramRe.ReplaceAllStringFunc(s, func(m string) string {
			k := m[2 : len(m)-1]
			if v, ok := params[k]; ok {
				return v
			}
			if p, ok := in.Params[k]; ok {
				return p.Default
			}
			missing = k
			return m
		})
	}
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return sub(x)
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, e := range x {
				out[sub(k)] = walk(e) // a parameter may also name a key
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e)
			}
			return out
		}
		return v
	}
	return walk(v), missing
}

// RuleGroups returns the integration's rule groups with the given
// parameters filled in (defaults for the rest), named after it so they
// cannot collide with the user's own.
func (in *Integration) RuleGroups(params map[string]string) ([]rules.Group, error) {
	if len(in.Rules) == 0 {
		return nil, nil
	}
	groups := make([]any, len(in.Rules))
	for i, g := range in.Rules {
		groups[i] = map[string]any(g)
	}
	filled, missing := in.fill(groups, params)
	if missing != "" {
		return nil, fmt.Errorf("integration %s: rules use ${%s}, which is not declared under params", in.Name, missing)
	}
	text, _ := yaml.Marshal(map[string]any{"groups": filled})
	f, err := rules.Parse(string(text))
	if err != nil {
		return nil, err
	}
	for i := range f.Groups {
		f.Groups[i].Name = in.Name + "/" + f.Groups[i].Name
	}
	return f.Groups, nil
}

// imageName strips the registry and the tag: docker.io/library/postgres:16 → postgres.
func imageName(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	if i := strings.IndexAny(image, ":@"); i >= 0 {
		image = image[:i]
	}
	return image
}

// Detected reports whether the inventory shows this technology.
func (in *Integration) Detected(inv *model.Inventory) bool {
	if inv == nil {
		return false
	}
	for _, want := range in.Detect.Process {
		for _, p := range inv.Processes {
			if p == want {
				return true
			}
		}
	}
	for _, want := range in.Detect.Image {
		for _, img := range inv.Images {
			if imageName(img) == want {
				return true
			}
		}
	}
	if len(in.Detect.Process) == 0 && len(in.Detect.Image) == 0 {
		for _, want := range in.Detect.Port {
			for _, p := range inv.Ports {
				if p == want {
					return true
				}
			}
		}
	}
	return false
}

// Catalog is the set of known integrations, by name.
type Catalog map[string]*Integration

// Names lists the integrations in order.
func (c Catalog) Names() []string {
	out := make([]string, 0, len(c))
	for n := range c {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Detect returns the names of the integrations an inventory shows.
func (c Catalog) Detect(inv *model.Inventory) []string {
	var out []string
	for _, n := range c.Names() {
		if c[n].Detected(inv) {
			out = append(out, n)
		}
	}
	return out
}

//go:embed integrations/*.yaml
var builtinFiles embed.FS

// Builtin returns the integrations shipped with vigil.
func Builtin() (Catalog, error) {
	out := Catalog{}
	entries, _ := builtinFiles.ReadDir("integrations")
	for _, e := range entries {
		text, err := builtinFiles.ReadFile("integrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		in, err := ParseIntegration(name, string(text))
		if err != nil {
			return nil, fmt.Errorf("built-in integration %s: %w", name, err)
		}
		in.Builtin = true
		out[name] = in
	}
	return out, nil
}

// BuiltinText returns the document of a built-in integration, to read or
// to start a customised copy from.
func BuiltinText(name string) (string, bool) {
	text, err := builtinFiles.ReadFile("integrations/" + name + ".yaml")
	return string(text), err == nil
}
