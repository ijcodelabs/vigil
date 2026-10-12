package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/dockerapi"
)

const (
	dockerKeyPrefix = "docker:"
	dockerRescan    = 10 * time.Second
	maxDockerFrame  = 16 << 20
)

// Docker follows the output of every running container through the Docker
// API, which works whatever log driver the daemon uses to keep it readable.
type Docker struct {
	Client    *dockerapi.Client
	Default   *Template
	Templates map[string]*Template // by name, for the vigil.logs.template label
	// ByContainer maps a pattern for a container's or image's name to a
	// template name; see config.Docker.ContainerTemplates.
	ByContainer map[string]string
	// Collect maps such a pattern to "true" or "false": whether the
	// container's logs are wanted; see config.Docker.ContainerLogs.
	Collect map[string]string
	Labels  map[string]string
	Pos     *Positions
	Out     chan<- Record
	Log     *slog.Logger

	mu     sync.Mutex
	active map[string]bool
	last   map[string]string // newest position read per container, saved or not
}

func (d *Docker) Run(ctx context.Context) {
	d.active, d.last = map[string]bool{}, map[string]string{}
	initial, failing := true, false
	for {
		containers, err := d.Client.List(ctx)
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			d.Log.Warn("cannot list docker containers, will keep trying", "err", err)
			failing = true
		case err == nil:
			failing = false
			d.sync(ctx, containers, initial)
			initial = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(dockerRescan):
		}
	}
}

func (d *Docker) sync(ctx context.Context, containers []dockerapi.Container, initial bool) {
	exists := map[string]bool{}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range containers {
		exists[dockerKeyPrefix+c.ID] = true
		if c.State != "running" || d.active[c.ID] || !d.wanted(c) {
			continue
		}
		// The container's own label decides first, then the config's
		// patterns, then the default.
		tmpl := d.Default
		if name := c.Labels["vigil.logs.template"]; name != "" {
			if t, ok := d.Templates[name]; ok {
				tmpl = t
			} else {
				d.Log.Warn("container asks for an unknown log template", "container", c.Name(), "template", name)
			}
		} else if name := containerTemplate(d.ByContainer, c.Name(), c.Image); name != "" {
			if t, ok := d.Templates[name]; ok {
				tmpl = t
			}
		}
		d.active[c.ID] = true
		go func() {
			if err := d.stream(ctx, c, tmpl, initial); err != nil && ctx.Err() == nil && !errors.Is(err, dockerapi.ErrGone) {
				d.Log.Warn("container log stream ended", "container", c.Name(), "err", err)
			}
			d.mu.Lock()
			delete(d.active, c.ID)
			d.mu.Unlock()
		}()
	}
	// A stopped container keeps its position (it may start again); a removed one does not.
	for _, key := range d.Pos.Keys(dockerKeyPrefix) {
		if !exists[key] {
			delete(d.last, strings.TrimPrefix(key, dockerKeyPrefix))
			select {
			case d.Out <- Record{Key: key, Delete: true}:
			case <-ctx.Done():
			}
		}
	}
}

func (d *Docker) stream(ctx context.Context, c dockerapi.Container, tmpl *Template, initial bool) error {
	key := dockerKeyPrefix + c.ID
	d.mu.Lock()
	pos, ok := d.last[c.ID]
	d.mu.Unlock()
	if !ok {
		pos, ok = d.Pos.Get(key)
	}
	// Resume after the last line shipped; a container already running when
	// the agent first sees it starts from now, a new one from its first line.
	since := "0"
	if ns, err := strconv.ParseInt(pos, 10, 64); ok && err == nil {
		ns++
		since = fmt.Sprintf("%d.%09d", ns/1e9, ns%1e9)
	} else if initial {
		since = strconv.FormatInt(time.Now().Unix(), 10)
	}
	tty, err := d.Client.HasTTY(ctx, c.ID)
	if err != nil {
		return err
	}
	body, err := d.Client.Logs(ctx, c.ID, since)
	if err != nil {
		return err
	}
	defer body.Close()

	stream := map[string]string{"source": "docker", "container": c.Name(), "image": c.Image}
	for k, v := range d.Labels {
		stream[k] = v
	}
	s := &sink{ctx: ctx, out: d.Out, key: key, tmpl: tmpl, stream: stream}
	lines := make(chan block, 256)
	var readErr error
	go func() {
		defer close(lines)
		readErr = readDockerStream(body, tty, func(name, line string) bool {
			b := block{text: line, fields: map[string]string{"stream": name}}
			if i := strings.IndexByte(line, ' '); i > 0 {
				if ts, err := time.Parse(time.RFC3339Nano, line[:i]); err == nil {
					b.text, b.time, b.pos = line[i+1:], ts, strconv.FormatInt(ts.UnixNano(), 10)
				}
			}
			select {
			case lines <- b:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	if last := pump(s, lines); last != "" {
		d.mu.Lock()
		d.last[c.ID] = last
		d.mu.Unlock()
	}
	if ctx.Err() != nil || readErr == io.EOF {
		return nil // shutdown, or the container stopped
	}
	return readErr
}

// readDockerStream calls fn for every output line. Without a TTY, Docker
// frames stdout and stderr as [stream, 0, 0, 0, size(4, big endian)] payload.
func readDockerStream(r io.Reader, tty bool, fn func(stream, line string) bool) error {
	if tty {
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, err := br.ReadString('\n')
			if line != "" && !fn("stdout", strings.TrimRight(line, "\r\n")) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	names := map[byte]string{1: "stdout", 2: "stderr"}
	bufs := map[byte][]byte{}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size > maxDockerFrame {
			return fmt.Errorf("docker log frame of %d bytes", size)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		name, ok := names[hdr[0]]
		if !ok {
			continue
		}
		buf := append(bufs[hdr[0]], payload...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			if !fn(name, strings.TrimSuffix(string(buf[:i]), "\r")) {
				return nil
			}
			buf = buf[i+1:]
		}
		bufs[hdr[0]] = append([]byte(nil), buf...)
	}
}

// imageName is an image reference without registry, tag and digest:
// "registry.example/library/haproxy:3.0@sha256:..." is "haproxy".
// wanted says whether a container's logs are collected. Its own label
// decides first (vigil.logs=false or true), then the config's patterns;
// without either, they are.
func (d *Docker) wanted(c dockerapi.Container) bool {
	switch c.Labels["vigil.logs"] {
	case "false":
		return false
	case "true":
		return true
	}
	return matchTemplate(d.Collect, c.Name(), imageName(c.Image)) != "false"
}

func imageName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	ref = ref[strings.LastIndexByte(ref, '/')+1:]
	ref, _, _ = strings.Cut(ref, ":")
	return ref
}

// containerTemplate picks the template for a container from patterns on its
// name or its image's name. The most specific pattern wins: an exact name
// before a wildcard, a longer pattern before a shorter one.
func containerTemplate(patterns map[string]string, name, image string) string {
	return matchTemplate(patterns, name, imageName(image))
}

// matchTemplate picks the template whose pattern matches one of the names;
// the most specific pattern wins.
func matchTemplate(patterns map[string]string, names ...string) string {
	best, tmpl := "", ""
	for pat, t := range patterns {
		matched := false
		for _, n := range names {
			if ok, _ := path.Match(pat, n); ok && n != "" {
				matched = true
			}
		}
		if !matched {
			continue
		}
		wild, bestWild := strings.ContainsAny(pat, "*?["), strings.ContainsAny(best, "*?[")
		if best == "" || (!wild && bestWild) || (wild == bestWild && (len(pat) > len(best) || (len(pat) == len(best) && pat < best))) {
			best, tmpl = pat, t
		}
	}
	return tmpl
}
