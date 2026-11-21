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
	// A container event has no process: the default names the container.
	containerSummary = "{container} started from {image}"
)

// Rule conditions are CEL expressions over these names. Fields that do not
// apply to an event's type are empty, zero or false.
var ruleVars = []cel.EnvOption{
	// exec, exit, connect, listen, open, setuid, container, namespace, mount, ptrace, module, bpf
	cel.Variable("type", cel.StringType),
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
	cel.Variable("refused", cel.BoolType),  // open: the kernel denied it
	cel.Variable("addr", cel.StringType),   // connect, listen
	cel.Variable("port", cel.IntType),      // connect, listen
	cel.Variable("unix", cel.BoolType),     // connect: to a runtime's unix socket (addr is its path)
	cel.Variable("old_euid", cel.IntType),  // setuid
	cel.Variable("new_euid", cel.IntType),  // setuid
	cel.Variable("exit_code", cel.IntType), // exit
	cel.Variable("signal", cel.IntType),    // exit
	// container
	cel.Variable("image", cel.StringType),
	cel.Variable("privileged", cel.BoolType),
	cel.Variable("host_pid", cel.BoolType),
	cel.Variable("host_network", cel.BoolType),
	cel.Variable("host_ipc", cel.BoolType),
	cel.Variable("cap_add", cel.ListType(cel.StringType)),
	cel.Variable("host_mounts", cel.ListType(cel.StringType)),
	cel.Variable("host_mounts_rw", cel.ListType(cel.StringType)),
	cel.Variable("runtime_socket", cel.StringType),
	cel.Variable("runs_as_root", cel.BoolType),
	cel.Variable("read_only_root", cel.BoolType),
	// namespace
	cel.Variable("namespaces", cel.ListType(cel.StringType)),      // mnt, uts, ipc, pid, net, cgroup
	cel.Variable("host_namespaces", cel.ListType(cel.StringType)), // of those, the ones now the host's
	cel.Variable("to_host", cel.BoolType),                         // host_namespaces is not empty
	// mount
	cel.Variable("source", cel.StringType),
	cel.Variable("fstype", cel.StringType),
	cel.Variable("target", cel.StringType),
	cel.Variable("bind", cel.BoolType),
	cel.Variable("remount", cel.BoolType),
	// ptrace
	cel.Variable("target_pid", cel.IntType),
	cel.Variable("target_comm", cel.StringType),
	// module, bpf
	cel.Variable("module", cel.StringType),
	cel.Variable("prog_name", cel.StringType),
	cel.Variable("prog_type", cel.IntType),
}

func (ev *Event) vars() map[string]any {
	v := map[string]any{
		"type": ev.Type, "pid": int64(ev.PID), "ppid": int64(ev.PPID), "uid": int64(ev.UID), "euid": int64(ev.EUID),
		"comm": ev.Comm, "exe": ev.Exe, "args": ev.Args, "cmdline": strings.Join(ev.Args, " "),
		"parent_comm": ev.ParentComm, "parent_exe": ev.ParentExe,
		"container": ev.Container, "container_id": ev.ContainerID,
		"path": ev.Path, "write": ev.Write, "refused": ev.Refused, "addr": ev.Addr, "port": int64(ev.Port),
		"old_euid": int64(0), "new_euid": int64(0), "exit_code": int64(0), "signal": int64(ev.Signal),
		"image": ev.Image, "privileged": ev.Privileged, "host_pid": ev.HostPID, "host_network": ev.HostNetwork,
		"host_ipc": ev.HostIPC, "cap_add": orEmpty(ev.CapAdd), "host_mounts": orEmpty(ev.HostMounts),
		"host_mounts_rw": orEmpty(ev.HostMountsRW), "runtime_socket": ev.RuntimeSocket,
		"runs_as_root": ev.RunsAsRoot, "read_only_root": ev.ReadOnlyRoot, "unix": ev.Unix,
		"namespaces": orEmpty(ev.Namespaces), "host_namespaces": orEmpty(ev.HostNamespaces),
		"to_host": len(ev.HostNamespaces) > 0, "source": ev.Source, "fstype": ev.FSType, "target": ev.Target,
		"bind": ev.MountFlags&msBind != 0, "remount": ev.MountFlags&msRemount != 0,
		"target_pid": int64(ev.TargetPID), "target_comm": ev.TargetComm,
		"module": ev.Module, "prog_name": ev.ProgName, "prog_type": int64(ev.ProgType),
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

// orEmpty gives CEL an empty list rather than a nil one.
func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

type rule struct {
	config.SecurityRule
	prog      cel.Program
	defaulted bool // Summary was not set
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
	for i, rc := range cfg.Rules {
		if !ruleNameRe.MatchString(rc.Name) {
			return nil, fmt.Errorf("security.rules[%d]: name must be letters, digits and _ (it becomes the alert name)", i)
		}
		if seen[rc.Name] {
			return nil, fmt.Errorf("security rule %s is defined twice", rc.Name)
		}
		seen[rc.Name] = true
	}
	all, err := effectiveRules(cfg)
	if err != nil {
		return nil, err
	}
	var out []*rule
	for _, rc := range all {
		if config.Off(rc.Enabled) {
			continue
		}
		where := "security rule " + rc.Name
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
		defaulted := rc.Summary == ""
		if defaulted {
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
		out = append(out, &rule{SecurityRule: rc, prog: prog, defaulted: defaulted})
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
	text := r.Summary
	if r.defaulted && vars["type"] == TypeContainer {
		text = containerSummary
	}
	return placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		return fmt.Sprint(vars[m[1:len(m)-1]])
	})
}
