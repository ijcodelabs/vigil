package logs

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
)

// Positions remembers how far each source has been shipped, so a restart
// continues where the last spooled batch ended.
type Positions struct {
	path string

	mu    sync.Mutex
	m     map[string]string
	dirty bool
}

func LoadPositions(path string) (*Positions, error) {
	p := &Positions{path: path, m: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &p.m); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Positions) Get(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[key]
	return v, ok
}

// Keys lists the remembered keys that start with prefix.
func (p *Positions) Keys(prefix string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k := range p.m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func (p *Positions) Set(key, val string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m[key] != val {
		p.m[key], p.dirty = val, true
	}
}

func (p *Positions) Delete(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.m[key]; ok {
		delete(p.m, key)
		p.dirty = true
	}
}

// Save writes the file atomically if anything changed.
func (p *Positions) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.dirty {
		return nil
	}
	b, _ := json.Marshal(p.m)
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	p.dirty = false
	return nil
}
