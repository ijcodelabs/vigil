package spool

import "testing"

func TestFIFOAndCap(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"aaaa", "bbbb"} {
		if d, err := s.Put([]byte(b)); err != nil || d != 0 {
			t.Fatalf("put: dropped=%d err=%v", d, err)
		}
	}
	if d, _ := s.Put([]byte("cccc")); d != 1 { // 12 bytes > 10: oldest goes
		t.Fatalf("dropped=%d, want 1", d)
	}

	s, _ = Open(dir, 10) // survives reopen
	for _, want := range []string{"bbbb", "cccc"} {
		id, data, ok, err := s.Next()
		if err != nil || !ok || string(data) != want {
			t.Fatalf("next: %q ok=%v err=%v, want %q", data, ok, err, want)
		}
		if err := s.Ack(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok, _ := s.Next(); ok {
		t.Fatal("expected empty spool")
	}
}
