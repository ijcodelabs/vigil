package events

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/config"
)

//go:embed builtin_rules.yaml
var builtinYAML []byte

// builtinRules is the rule pack the agent ships with.
var builtinRules = sync.OnceValue(func() []config.SecurityRule {
	var rules []config.SecurityRule
	if err := yaml.Unmarshal(builtinYAML, &rules); err != nil {
		panic("events: builtin_rules.yaml: " + err.Error()) // a test parses it
	}
	return rules
})

// builtinWatchPaths are the files the built-in rules look at. Each one is
// opened rarely; /etc/passwd is not here because every program that looks
// up a user name reads it.
var builtinWatchPaths = []string{"/etc/shadow", "/etc/sudoers", "/etc/ssh/", "/root/.ssh/", "/etc/ld.so.preload",
	"/etc/cron", "/var/spool/cron/", "/etc/systemd/system/"}

func builtinOn(cfg *config.Security) bool { return cfg.BuiltinRules == nil || *cfg.BuiltinRules }

// effectiveRules is the built-in pack, less the rules turned off or
// replaced by a configured rule of the same name, then the configured rules.
func effectiveRules(cfg *config.Security) ([]config.SecurityRule, error) {
	if !builtinOn(cfg) {
		if len(cfg.DisableRules) > 0 {
			return nil, fmt.Errorf("security.disable_rules: the built-in rules are off (builtin_rules: false)")
		}
		return cfg.Rules, nil
	}
	pack := builtinRules()
	for _, name := range cfg.DisableRules {
		if !slices.ContainsFunc(pack, func(r config.SecurityRule) bool { return r.Name == name }) {
			return nil, fmt.Errorf("security.disable_rules: %q is not a built-in rule", name)
		}
	}
	configured := map[string]bool{}
	for _, r := range cfg.Rules {
		configured[r.Name] = true
	}
	var out []config.SecurityRule
	for _, r := range pack {
		if !configured[r.Name] && !slices.Contains(cfg.DisableRules, r.Name) {
			out = append(out, r)
		}
	}
	return append(out, cfg.Rules...), nil
}

// watchPaths is what the kernel is told to report: the configured paths,
// then the built-in rules' paths not already covered, up to the kernel's
// limit. The second list holds the built-in paths that did not fit.
func watchPaths(cfg *config.Security) (paths, skipped []string) {
	paths = append(paths, cfg.WatchPaths...)
	if !builtinOn(cfg) {
		return paths, nil
	}
	for _, p := range builtinWatchPaths {
		covered := slices.ContainsFunc(paths, func(have string) bool { return strings.HasPrefix(p, have) })
		switch {
		case covered:
		case len(paths) >= maxWatchPaths:
			skipped = append(skipped, p)
		default:
			paths = append(paths, p)
		}
	}
	return paths, skipped
}
