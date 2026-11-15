package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ytm-client/internal/ytm"
)

func TestAddLoadRecent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AppData", dir) // os.UserConfigDir on Windows

	s := Load()
	if s.path == "" {
		t.Skip("no UserConfigDir override available")
	}
	t1 := ytm.Track{Title: "A", Artist: "x", VideoID: "v1", Thumbnail: "u1"}
	t2 := ytm.Track{Title: "B", VideoID: "v2"}
	now := time.Now()
	s.Add(t1, now)
	s.Add(t1, now.Add(time.Second))
	s.Add(t2, now.Add(2*time.Second))
	// wait for the background save
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(s.path)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	s2 := Load()
	rec := s2.Recent(10)
	if len(rec) != 2 {
		t.Fatalf("want 2 entries, got %d", len(rec))
	}
	if rec[0].VideoID != "v2" || rec[1].VideoID != "v1" || rec[1].Count != 2 {
		t.Fatalf("unexpected order/counts: %+v", rec)
	}
}
