// Package redact masks secrets in text before the agent sends it.
//
// It exists because the content an agent forwards is not under its
// control: an AI coding tool that reports its tool output sends whatever a
// command printed, `.env` files included, and an application log may carry
// a token in a URL. Masking on the agent means a secret is never stored.
package redact

import (
	"fmt"
	"regexp"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/model"
)

const mask = "[redacted]"

type rule struct {
	re      *regexp.Regexp
	replace string
}

// builtin are credentials common enough to mask everywhere. Where a name
// or prefix says what the value is, it is kept and only the value masked,
// so a reader still sees that a password was there.
var builtin = []rule{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), mask},
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`), "sk-ant-" + mask},
	{regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}`), "sk-" + mask},
	{regexp.MustCompile(`\b(gh[pousr]_)[A-Za-z0-9]{30,}`), "${1}" + mask},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}`), "github_pat_" + mask},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`), "glpat-" + mask},
	{regexp.MustCompile(`\b(xox[abprs]-)[A-Za-z0-9-]{10,}`), "${1}" + mask},
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "${1}" + mask},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), "AIza" + mask},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), mask}, // a JWT
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`), "${1} " + mask},
	{regexp.MustCompile(`(://[^/\s:@]+:)[^/\s@]+@`), "${1}" + mask + "@"}, // password in a URL
	// A value named as a secret: DB_PASSWORD=..., "api_key": "...",
	// token: ... The name has to end in the word, so input_tokens=12 and
	// password_policy=strict stay as they are.
	{regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|auth)["']?\s*[:=]\s*["']?)([^\s"',;&}]{4,})`), "${1}" + mask},
}

// Redactor masks what its rules match. The zero value and nil mask nothing.
type Redactor struct {
	rules []rule
}

// New compiles the built-in rules (unless switched off) and the
// configured ones. The configuration is checked when it is loaded, so an
// error here means it was not.
func New(c config.Redact) (*Redactor, error) {
	r := &Redactor{}
	if c.Builtin == nil || *c.Builtin {
		r.rules = append(r.rules, builtin...)
	}
	for _, cr := range c.Rules {
		if config.Off(cr.Enabled) {
			continue
		}
		re, err := regexp.Compile(cr.Match)
		if err != nil {
			return nil, fmt.Errorf("redact rule %s: %v", cr.Name, err)
		}
		replace := cr.Replace
		if replace == "" {
			replace = mask
		}
		r.rules = append(r.rules, rule{re, replace})
	}
	return r, nil
}

// String returns s with every match masked.
func (r *Redactor) String(s string) string {
	if r == nil || len(s) < 8 {
		return s
	}
	for _, ru := range r.rules {
		if ru.re.MatchString(s) {
			s = ru.re.ReplaceAllString(s, ru.replace)
		}
	}
	return s
}

// Entries masks the message and fields of log entries in place. Stream
// labels are left alone: they name sources, and masking them would split
// a stream.
func (r *Redactor) Entries(batch []model.LogEntry) {
	if r == nil || len(r.rules) == 0 {
		return
	}
	for i := range batch {
		e := &batch[i]
		e.Msg = r.String(e.Msg)
		for k, v := range e.Fields {
			if nv := r.String(v); nv != v {
				e.Fields[k] = nv
			}
		}
	}
}

// Active says whether there is anything to mask with.
func (r *Redactor) Active() bool { return r != nil && len(r.rules) > 0 }
