package ytm

import "testing"

// Quick regression + live smoke check against InnerTube (network required).
func TestSearchYTMusic(t *testing.T) {
	tracks, err := SearchYTMusic("daft punk")
	if err != nil {
		t.Skipf("search failed (network?): %v", err)
	}
	if len(tracks) == 0 {
		t.Fatal("got 0 tracks")
	}
	for i, tr := range tracks {
		if tr.VideoID == "" || tr.Title == "" {
			t.Fatalf("track %d missing Title/VideoID: %+v", i, tr)
		}
	}
	for i, tr := range tracks[:4] {
		t.Logf("%d: %s - %s [%s] (%s) id=%s", i, tr.Title, tr.Artist, tr.Album, tr.Duration, tr.VideoID)
	}
}
