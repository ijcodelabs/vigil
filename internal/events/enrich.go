package events

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type proc struct {
	comm, exe string
	ppid      uint32
}

// procCache remembers what each process is, so an event can say which
// program did it and which program started that one. Processes that were
// already running are looked up in /proc the first time they are seen.
type procCache struct {
	root string
	m    map[uint32]*proc
}

func newProcCache() *procCache {
	return &procCache{root: envOr("HOST_PROC", "/proc"), m: map[uint32]*proc{}}
}

func (c *procCache) get(pid uint32) *proc {
	if p, ok := c.m[pid]; ok {
		return p
	}
	if len(c.m) > 50000 {
		clear(c.m)
	}
	p := &proc{}
	dir := filepath.Join(c.root, strconv.FormatUint(uint64(pid), 10))
	if b, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
		p.comm = strings.TrimSpace(string(b))
	}
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		p.exe = exe
	} else if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.exe = cstr(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "stat")); err == nil {
		// "pid (comm) state ppid ..."; comm may contain spaces and parentheses.
		if i := bytes.LastIndexByte(b, ')'); i > 0 {
			if f := strings.Fields(string(b[i+1:])); len(f) > 1 {
				n, _ := strconv.ParseUint(f[1], 10, 32)
				p.ppid = uint32(n)
			}
		}
	}
	c.m[pid] = p
	return p
}

// apply fills in the process and parent fields of ev and keeps the cache
// current with it.
func (c *procCache) apply(ev *Event) {
	switch ev.Type {
	case TypeExec:
		c.m[ev.PID] = &proc{comm: ev.Comm, exe: ev.Exe, ppid: ev.PPID}
	default:
		ev.Exe = c.get(ev.PID).exe
	}
	if ev.PPID != 0 {
		parent := c.get(ev.PPID)
		ev.ParentComm, ev.ParentExe = parent.comm, parent.exe
	}
	if ev.Type == TypeExit {
		delete(c.m, ev.PID)
	}
}

var containerIDRe = regexp.MustCompile(`[0-9a-f]{64}`)

// containers maps the cgroup id the kernel reports to a container. On
// cgroup v2 that id is the inode number of the cgroup's directory, and
// container runtimes put the container id in the directory name.
type containers struct {
	root     string
	byCgroup map[uint64]string
	scanned  time.Time
	// names resolves container ids to names (from Docker); may be nil.
	names     func() map[string]string
	byID      map[string]string
	namesRead time.Time
}

func newContainers(names func() map[string]string) *containers {
	return &containers{root: filepath.Join(envOr("HOST_SYS", "/sys"), "fs", "cgroup"),
		byCgroup: map[uint64]string{}, names: names, byID: map[string]string{}}
}

func (c *containers) scan() {
	c.scanned = time.Now()
	found := map[uint64]string{}
	filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				ids := containerIDRe.FindAllString(path, -1)
				id := ""
				if len(ids) > 0 {
					id = ids[len(ids)-1]
				}
				found[uint64(st.Ino)] = id
			}
		}
		return nil
	})
	c.byCgroup = found
}

// apply sets ev.Container and ev.ContainerID; both stay empty for a
// process that is not in a container.
func (c *containers) apply(ev *Event) {
	id, known := c.byCgroup[ev.cgroup]
	if !known && time.Since(c.scanned) > 2*time.Second {
		c.scan()
		id, known = c.byCgroup[ev.cgroup]
	}
	if !known {
		c.byCgroup[ev.cgroup] = "" // gone before we looked; do not rescan for it
	}
	if id == "" {
		return
	}
	ev.ContainerID, ev.Container = id[:12], id[:12]
	name, ok := c.byID[id]
	if !ok && c.names != nil && time.Since(c.namesRead) > 5*time.Second {
		c.namesRead = time.Now()
		for cid, n := range c.names() {
			c.byID[cid] = n
		}
		name = c.byID[id]
	}
	if name != "" {
		ev.Container = name
	}
}
