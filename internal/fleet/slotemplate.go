package fleet

import (
	"fmt"
	"regexp"
	"strings"
)

// SLOTemplate is an SLO an integration offers: one for each value of a
// label on its metrics (each HAProxy backend, say). The SLOs page offers
// them; none is made by itself. Name and Doc refer to the value as
// ${label}; Doc is an SLO document without its name.
type SLOTemplate struct {
	ID          string `yaml:"id" json:"id"`
	Title       string `yaml:"title" json:"title"`
	Description string `yaml:"description" json:"description"`
	Each        struct {
		Match string `yaml:"match" json:"match"` // a series selector
		Label string `yaml:"label" json:"label"`
	} `yaml:"each" json:"each"`
	Name string `yaml:"name" json:"name"`
	Doc  string `yaml:"doc" json:"doc"`
}

var (
	templateIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// TemplateValueRe is what a label value must look like to go into an
	// SLO document: no quotes, braces or spaces that would change the
	// YAML or the queries around it.
	TemplateValueRe = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,100}$`)
	nonSlugRe       = regexp.MustCompile(`[^a-z0-9]+`)
)

// CheckSLOTemplates says what is wrong with a list of templates. Whether
// the documents they make are valid SLOs is checked where SLOs are known
// (the server's tests fill every template).
func CheckSLOTemplates(list []SLOTemplate) error {
	seen := map[string]bool{}
	for _, t := range list {
		switch {
		case !templateIDRe.MatchString(t.ID):
			return fmt.Errorf("SLO template id %q: lower-case letters, digits and dashes", t.ID)
		case seen[t.ID]:
			return fmt.Errorf("SLO template %s is there twice", t.ID)
		case t.Title == "":
			return fmt.Errorf("SLO template %s has no title", t.ID)
		case t.Each.Match == "" || !labelNameRe.MatchString(t.Each.Label):
			return fmt.Errorf("SLO template %s: each needs a match and a label name", t.ID)
		case !strings.Contains(t.Name, "${"+t.Each.Label+"}") || !strings.Contains(t.Doc, "${"+t.Each.Label+"}"):
			return fmt.Errorf("SLO template %s: name and doc must use ${%s}", t.ID, t.Each.Label)
		}
		seen[t.ID] = true
	}
	return nil
}

// Fill makes the SLO's name and document for one value of the label. The
// name is turned into a valid SLO name (lower case, dashes).
func (t SLOTemplate) Fill(value string) (name, doc string) {
	ref := "${" + t.Each.Label + "}"
	name = nonSlugRe.ReplaceAllString(strings.ToLower(strings.ReplaceAll(t.Name, ref, value)), "-")
	name = strings.Trim(name, "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name, "name: " + name + "\n" + strings.ReplaceAll(t.Doc, ref, value)
}
