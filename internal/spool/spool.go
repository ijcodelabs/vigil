// Package spool is a small on-disk FIFO of opaque batches. The agent writes
// every batch here first, so data survives restarts and server outages.
package spool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Spool struct {
	dir string
	max int64

	mu  sync.Mutex
	seq int
}

func Open(dir string, maxBytes int64) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Spool{dir: dir, max: maxBytes}, nil
}

type entry struct {
	path string
	size int64
}

// list returns the queued batches, oldest first.
func (s *Spool) list() ([]entry, int64, error) {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, 0, err
	}
	var out []entry
	var total int64
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".bin") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, entry{filepath.Join(s.dir, de.Name()), fi.Size()})
		total += fi.Size()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, total, nil
}

// Put appends a batch. When the spool is full the oldest batches are
// discarded to make room; dropped reports how many.
func (s *Spool) Put(data []byte) (dropped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, total, err := s.list()
	if err != nil {
		return 0, err
	}
	for len(entries) > 0 && total+int64(len(data)) > s.max {
		os.Remove(entries[0].path)
		total -= entries[0].size
		entries = entries[1:]
		dropped++
	}
	s.seq++
	name := fmt.Sprintf("%020d-%06d", time.Now().UnixNano(), s.seq)
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return dropped, err
	}
	return dropped, os.Rename(tmp, filepath.Join(s.dir, name+".bin"))
}

// Next returns the oldest batch without removing it; ok is false when empty.
func (s *Spool) Next() (id string, data []byte, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _, err := s.list()
	if err != nil || len(entries) == 0 {
		return "", nil, false, err
	}
	data, err = os.ReadFile(entries[0].path)
	if err != nil {
		return "", nil, false, err
	}
	return entries[0].path, data, true, nil
}

// Ack removes a batch returned by Next.
func (s *Spool) Ack(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(id)
}

// Size returns the bytes waiting to be sent.
func (s *Spool) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, total, _ := s.list()
	return total
}
