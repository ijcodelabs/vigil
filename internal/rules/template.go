package rules

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"text/template"
	"time"
)

// Labels and annotations are Go templates with Prometheus' variables:
// {{ $labels.instance }}, {{ $value }}, {{ $externalURL }}.
const preamble = `{{$labels := .Labels}}{{$value := .Value}}{{$externalURL := .ExternalURL}}`

type tmplData struct {
	Labels      map[string]string
	Value       float64
	ExternalURL string
}

var funcs = template.FuncMap{
	"toUpper": strings.ToUpper,
	"toLower": strings.ToLower,
	"humanize": func(v float64) string {
		return siFormat(v, 1000, []string{"", "k", "M", "G", "T", "P"})
	},
	"humanize1024": func(v float64) string {
		return siFormat(v, 1024, []string{"", "ki", "Mi", "Gi", "Ti", "Pi"})
	},
	"humanizePercentage": func(v float64) string { return fmt.Sprintf("%.4g%%", v*100) },
	"humanizeDuration": func(v float64) string {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Sprint(v)
		}
		return time.Duration(v * float64(time.Second)).Round(time.Millisecond).String()
	},
}

func siFormat(v, base float64, units []string) string {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) < 1 {
		return fmt.Sprintf("%.4g", v)
	}
	i := 0
	for math.Abs(v) >= base && i < len(units)-1 {
		v /= base
		i++
	}
	return fmt.Sprintf("%.4g%s", v, units[i])
}

var tmplCache sync.Map // text -> *template.Template

func compile(text string) (*template.Template, error) {
	if t, ok := tmplCache.Load(text); ok {
		return t.(*template.Template), nil
	}
	t, err := template.New("").Funcs(funcs).Option("missingkey=zero").Parse(preamble + text)
	if err != nil {
		return nil, err
	}
	tmplCache.Store(text, t)
	return t, nil
}

// expand renders text; on failure the result says so instead of dropping
// the alert, as Prometheus does.
func expand(text string, d tmplData) string {
	if !strings.Contains(text, "{{") {
		return text
	}
	t, err := compile(text)
	if err != nil {
		return "<template error: " + err.Error() + ">"
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "<template error: " + err.Error() + ">"
	}
	return b.String()
}
