package lyrics

import (
	"context"
	"testing"
	"time"
)

// Live test against lrclib.net (skips without network).
func TestGetSynced(t *testing.T) {
	c := NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l, err := c.Get(ctx, "Daft Punk", "Instant Crush", 338*time.Second)
	if err != nil {
		t.Skipf("lookup failed (network?): %v", err)
	}
	if len(l.Synced) < 5 {
		t.Fatalf("expected synced lines, got %d; plain=%d bytes", len(l.Synced), len(l.Plain))
	}
	inOrder := true
	for i := 1; i < len(l.Synced); i++ {
		if l.Synced[i].Start < l.Synced[i-1].Start {
			inOrder = false
		}
	}
	if !inOrder {
		t.Fatal("synced lines not sorted by time")
	}
	t.Logf("%s - %s: %d synced lines, first=%s %q", l.Artist, l.Track, len(l.Synced), l.Synced[0].Start, l.Synced[0].Text)
}

func TestParseLRC(t *testing.T) {
	lrc := "[ar:x]\n[01:02.50]hello\n[00:10.00]first\n[01:02.50][00:20.00]double\n"
	lines := parseLRC(lrc)
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d: %+v", len(lines), lines)
	}
	if lines[0].Start != 10*time.Second || lines[0].Text != "first" {
		t.Fatalf("line0 wrong: %+v", lines[0])
	}
	if lines[1].Start != 20*time.Second || lines[1].Text != "double" {
		t.Fatalf("line1 wrong: %+v", lines[1])
	}
	if lines[3].Start != 62*time.Second+500*time.Millisecond {
		t.Fatalf("line3 wrong: %+v", lines[3])
	}
}
