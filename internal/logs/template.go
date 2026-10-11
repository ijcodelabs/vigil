// Package logs is the agent's log pipeline: sources read raw lines, a
// template turns them into entries, and the manager batches them out.
package logs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

type maskRule struct {
	re   *regexp.Regexp
	repl string
}

// Template is a compiled config.LogTemplate. The zero value passes lines
// through unchanged.
type Template struct {
	start      *regexp.Regexp
	parse      string
	re         *regexp.Regexp
	msgField   string
	tsField    string
	tsLayout   string
	labels     []string
	add        map[string]string
	keep, drop []*regexp.Regexp
	mask       []maskRule
}

func Compile(name string, c config.LogTemplate) (*Template, error) {
	t := &Template{parse: c.Parse, msgField: c.MessageField, tsField: c.Timestamp.Field,
		tsLayout: c.Timestamp.Layout, labels: c.Labels, add: c.Add}
	re := func(what, expr string) (*regexp.Regexp, error) {
		r, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("template %s: %s: %w", name, what, err)
		}
		return r, nil
	}
	var err error
	if c.Multiline.Start != "" {
		if t.start, err = re("multiline.start", c.Multiline.Start); err != nil {
			return nil, err
		}
	}
	switch c.Parse {
	case "", "none":
		t.parse = ""
	case "json", "logfmt":
	case "regex":
		if c.Pattern == "" {
			return nil, fmt.Errorf("template %s: parse: regex needs a pattern", name)
		}
		if t.re, err = re("pattern", c.Pattern); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("template %s: unknown parse %q (use json, logfmt or regex)", name, c.Parse)
	}
	for _, e := range c.Keep {
		r, err := re("keep", e)
		if err != nil {
			return nil, err
		}
		t.keep = append(t.keep, r)
	}
	for _, e := range c.Drop {
		r, err := re("drop", e)
		if err != nil {
			return nil, err
		}
		t.drop = append(t.drop, r)
	}
	for _, m := range c.Mask {
		r, err := re("mask", m.Pattern)
		if err != nil {
			return nil, err
		}
		t.mask = append(t.mask, maskRule{r, m.Replace})
	}
	return t, nil
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, r := range res {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

// Process applies the template to one (possibly multi-line) raw entry.
// ok is false when the entry is filtered out. ts is zero when the template
// found no timestamp. Masking runs first, so secrets never reach a field.
func (t *Template) Process(raw string) (msg string, ts time.Time, labels, fields map[string]string, ok bool) {
	for _, m := range t.mask {
		raw = m.re.ReplaceAllString(raw, m.repl)
	}
	if (len(t.keep) > 0 && !matchAny(t.keep, raw)) || matchAny(t.drop, raw) {
		return "", ts, nil, nil, false
	}
	fields, labels = map[string]string{}, map[string]string{}
	for k, v := range t.add {
		labels[k] = v
	}
	msg = raw
	msgKeys := []string{"message", "msg", "log"}
	switch t.parse {
	case "json":
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var obj map[string]any
		if dec.Decode(&obj) == nil {
			flatten("", obj, fields)
		}
	case "logfmt":
		parseLogfmt(raw, fields)
	case "regex":
		if m := t.re.FindStringSubmatch(raw); m != nil {
			for i, n := range t.re.SubexpNames() {
				if n != "" {
					fields[n] = m[i]
				}
			}
		}
		msgKeys = []string{"message"}
	}
	if t.msgField != "" {
		msgKeys = []string{t.msgField}
	}
	for _, k := range msgKeys {
		if v := fields[k]; v != "" {
			msg = v
			delete(fields, k)
			break
		}
	}
	if v, found := fields[t.tsField]; found && t.tsField != "" {
		if p, err := parseTime(v, t.tsLayout); err == nil {
			ts = p
			delete(fields, t.tsField)
		}
	}
	for _, l := range t.labels {
		if v, found := fields[l]; found {
			labels[l] = v
			delete(fields, l)
		}
	}
	return msg, ts, labels, fields, true
}

func parseTime(v, layout string) (time.Time, error) {
	switch layout {
	case "", "rfc3339":
		return time.Parse(time.RFC3339Nano, v)
	case "unix":
		f, err := strconv.ParseFloat(v, 64)
		return time.Unix(0, int64(f*1e9)), err
	case "unix_ms":
		n, err := strconv.ParseInt(v, 10, 64)
		return time.UnixMilli(n), err
	}
	p, err := time.ParseInLocation(layout, v, time.Local)
	if err == nil && p.Year() == 0 { // syslog-style layouts carry no year
		p = p.AddDate(time.Now().Year(), 0, 0)
	}
	return p, err
}

// flatten turns nested JSON into dotted keys with string values.
func flatten(prefix string, obj map[string]any, out map[string]string) {
	for k, v := range obj {
		key := prefix + k
		switch x := v.(type) {
		case nil:
		case string:
			out[key] = x
		case json.Number:
			out[key] = x.String()
		case bool:
			out[key] = strconv.FormatBool(x)
		case map[string]any:
			flatten(key+".", x, out)
		default: // arrays stay as JSON text
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			enc.Encode(x)
			out[key] = strings.TrimSpace(b.String())
		}
	}
}

// parseLogfmt reads key=value pairs; values may be double-quoted.
func parseLogfmt(s string, out map[string]string) {
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return
		}
		end := strings.IndexAny(s, "= \t")
		if end < 0 {
			out[s] = ""
			return
		}
		key := s[:end]
		if s[end] != '=' {
			if key != "" {
				out[key] = ""
			}
			s = s[end:]
			continue
		}
		s = s[end+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					if s[i] == 'n' {
						b.WriteByte('\n')
						continue
					}
				}
				b.WriteByte(s[i])
			}
			val = b.String()
			if i < len(s) {
				i++
			}
			s = s[i:]
		} else {
			i := strings.IndexAny(s, " \t")
			if i < 0 {
				i = len(s)
			}
			val, s = s[:i], s[i:]
		}
		if key != "" {
			out[key] = val
		}
	}
}
