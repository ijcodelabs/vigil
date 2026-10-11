package events

import (
	"fmt"
	"regexp"
	"strings"

	"cel.dev/cel-go/cel"

	"github.com/ijcodelabs/vigil/internal/config"
)

const (
	maxWatchPaths  = 16 // MAX_PREFIXES in bpf/events.c
	maxWatchLen    = 63 // PREFIX_LEN - 1
	defaultPerMin  = 10
	defaultSummary = "{type} by {comm}"
)

// Rule conditions are CEL expressions over these names. Fields that do not
// apply to an event's type are empty, zero or false.
var ruleVars = []cel.EnvOption{
	cel.Variable("type", cel.StringType), // exec, exit, connect, listen, open, setuid
	cel.Variable("pid", cel.IntType),
	cel.Variable("ppid", cel.IntType),
	cel.Variable("uid", cel.IntType),
	cel.Variable("euid", cel.IntType),
	cel.Variable("comm", cel.StringType), // short process name
	cel.Variable("exe", cel.StringType),  // program file
	cel.Variable("args", cel.ListType(cel.StringType)),
	cel.Variable("cmdline", cel.StringType), // args joined with spaces
	cel.Variable("parent_comm", cel.StringType),
	cel.Variable("parent_exe", cel.StringType),
	cel.Variable("container", cel.StringType), // name or short id; "" on the host
	cel.Variable("container_id", cel.StringType),
	cel.Variable("path", cel.StringType),   // open
	cel.Variable("write", cel.BoolType),    // open
	cel.Variable("addr", cel.StringType),   // connect, listen
	cel.Variable("port", cel.IntType),      // connect, listen
	cel.Variable("old_euid", cel.IntType),  // setuid
	cel.Variable("new_euid", cel.IntType),  // setuid
	cel.Variable("exit_code", cel.IntType), // exit
	cel.Variable("signal", cel.IntType),    // exit
}

func (ev *Event) vars() map[string]any {
	v := map[string]any{
		"type": ev.Type, "pid": int64(ev.PID), "ppid": int64(ev.PPID), "uid": int64(ev.UID), "euid": int64(ev.EUID),
		"comm": ev.Comm, "exe": ev.Exe, "args": ev.Args, "cmdline": strings.Join(ev.Args, " "),
		"parent_comm": ev.ParentComm, "parent_exe": ev.ParentExe,
		"container": ev.Container, "container_id": ev.ContainerID,
		"path": ev.Path, "write": ev.Write, "addr": ev.Addr, "port": int64(ev.Port),
		"old_euid": int64(0), "new_euid": int64(0), "exit_code": int64(0), "signal": int64(ev.Signal),
	}
	if ev.Args == nil {
		v["args"] = []string{}
	}
	if ev.OldEUID != nil {
		v["old_euid"], v["new_euid"] = int64(*ev.OldEUID), int64(*ev.NewEUID)
	}
	if ev.ExitCode != nil {
		v["exit_code"] = int64(*ev.ExitCode)
	}
	return v
}

type rule struct {
	config.SecurityRule
	prog cel.Program
}

var (
	ruleNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	placeholderRe = regexp.MustCompile(`\{([a-z_]+)\}`)
)

func compileRules(cfg *config.Security) ([]*rule, error) {
	env, err := cel.NewEnv(ruleVars...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []*rule
	for i, rc := range cfg.Rules {
		if config.Off(rc.Enabled) {
			continue
		}
		where := fmt.Sprintf("security.rules[%d]", i)
		if !ruleNameRe.MatchString(rc.Name) {
			return nil, fmt.Errorf("%s: name must be letters, digits and _ (it becomes the alert name)", where)
		}
		where = "security rule " + rc.Name
		if seen[rc.Name] {
			return nil, fmt.Errorf("%s is defined twice", where)
		}
		seen[rc.Name] = true
		switch rc.Severity {
		case "":
			rc.Severity = "warning"
		case "info", "warning", "critical":
		default:
			return nil, fmt.Errorf("%s: severity must be info, warning or critical", where)
		}
		if rc.MaxPerMinute == 0 {
			rc.MaxPerMinute = defaultPerMin
		}
		if rc.Summary == "" {
			rc.Summary = defaultSummary
		}
		known := (&Event{}).vars()
		for _, m := range placeholderRe.FindAllStringSubmatch(rc.Summary, -1) {
			if _, ok := known[m[1]]; !ok {
				return nil, fmt.Errorf("%s: summary uses {%s}, which is not an event field", where, m[1])
			}
		}
		ast, iss := env.Compile(rc.When)
		if iss != nil && iss.Err() != nil {
			return nil, fmt.Errorf("%s: when: %w", where, iss.Err())
		}
		if !ast.OutputType().IsExactType(cel.BoolType) {
			return nil, fmt.Errorf("%s: when must be true or false, but it is a %s", where, ast.OutputType())
		}
		prog, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("%s: when: %w", where, err)
		}
		out = append(out, &rule{SecurityRule: rc, prog: prog})
	}
	return out, nil
}

// Validate checks a security section without loading anything into the
// kernel, so the server can refuse a bad managed config.
func Validate(cfg *config.Security) error {
	if len(cfg.WatchPaths) > maxWatchPaths {
		return fmt.Errorf("security.watch_paths: at most %d paths", maxWatchPaths)
	}
	for _, p := range cfg.WatchPaths {
		if !strings.HasPrefix(p, "/") || len(p) > maxWatchLen {
			return fmt.Errorf("security.watch_paths: %q must be an absolute path of at most %d characters", p, maxWatchLen)
		}
	}
	_, err := compileRules(cfg)
	return err
}

func (r *rule) matches(vars map[string]any) bool {
	out, _, err := r.prog.Eval(vars)
	if err != nil {
		return false
	}
	b, _ := out.Value().(bool)
	return b
}

func (r *rule) summary(vars map[string]any) string {
	return placeholderRe.ReplaceAllStringFunc(r.Summary, func(m string) string {
		return fmt.Sprint(vars[m[1:len(m)-1]])
	})
}
