package ytm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSearchYTMusic is the live smoke check against InnerTube (network
// required). With WAX_DUMP_FIXTURE=1 it also (re)captures the raw response
// into testdata/search.json for the offline parser tests.
func TestSearchYTMusic(t *testing.T) {
	root, err := searchRaw("daft punk")
	if err != nil {
		t.Skipf("search failed (network?): %v", err)
	}
	tracks, err := tracksFromResponse(root, "daft punk")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) == 0 {
		t.Fatal("got 0 tracks")
	}
	for i, tr := range tracks {
		if tr.VideoID == "" || tr.Title == "" {
			t.Fatalf("track %d missing Title/VideoID: %+v", i, tr)
		}
	}
	// degraded-parse guard: top results should always carry an artist (it
	// is derivable from columns or the accessibility fallback); a missing
	// duration is legitimate for top-result card rows, so only note it.
	for _, tr := range tracks[:4] {
		if tr.Artist == "" {
			t.Errorf("degraded parse (no artist): %+v", tr)
		}
		if tr.Duration == "" {
			t.Logf("note: no duration on %+v (top-result card rows ship without one)", tr)
		}
	}
	for i, tr := range tracks[:4] {
		t.Logf("%d: %s - %s [%s] (%s) id=%s", i, tr.Title, tr.Artist, tr.Album, tr.Duration, tr.VideoID)
	}

	if os.Getenv("WAX_DUMP_FIXTURE") != "" {
		data, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		p := filepath.Join("testdata", "search.json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Logf("fixture captured: %s (%d bytes)", p, len(data))
	}
}
