// Package scrape fetches and parses Prometheus text-format endpoints.
package scrape

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ijcodelabs/vigil/internal/model"
)

// Parse reads the Prometheus text exposition format (0.0.4). Histograms and
// summaries arrive already flattened into _bucket/_sum/_count series, so every
// line maps to one sample. Timestamps in the payload are ignored; ts is used.
func Parse(r io.Reader, ts int64) ([]model.Sample, error) {
	var out []model.Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		s, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		s.TS = ts
		out = append(out, s)
	}
	return out, sc.Err()
}

func parseLine(line string) (model.Sample, error) {
	s := model.Sample{Labels: map[string]string{}}
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return s, fmt.Errorf("no metric name in %q", line)
	}
	s.Name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		var err error
		if rest, err = parseLabels(rest[1:], s.Labels); err != nil {
			return s, err
		}
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return s, fmt.Errorf("no value for %s", s.Name)
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return s, fmt.Errorf("bad value %q for %s", f[0], s.Name)
	}
	s.Value = v
	return s, nil
}

// parseLabels consumes `k="v",...}` and returns what follows the brace.
func parseLabels(in string, labels map[string]string) (string, error) {
	for {
		in = strings.TrimLeft(in, " \t,")
		if in == "" {
			return "", fmt.Errorf("unterminated label set")
		}
		if in[0] == '}' {
			return in[1:], nil
		}
		eq := strings.IndexByte(in, '=')
		if eq <= 0 {
			return "", fmt.Errorf("bad label near %q", in)
		}
		name := strings.TrimSpace(in[:eq])
		in = strings.TrimLeft(in[eq+1:], " \t")
		if in == "" || in[0] != '"' {
			return "", fmt.Errorf("label %s: value not quoted", name)
		}
		var b strings.Builder
		j := 1
		for ; j < len(in) && in[j] != '"'; j++ {
			if in[j] == '\\' && j+1 < len(in) {
				j++
				switch in[j] {
				case 'n':
					b.WriteByte('\n')
				default: // \\ and \"
					b.WriteByte(in[j])
				}
				continue
			}
			b.WriteByte(in[j])
		}
		if j >= len(in) {
			return "", fmt.Errorf("label %s: unterminated value", name)
		}
		labels[name] = b.String()
		in = in[j+1:]
	}
}
