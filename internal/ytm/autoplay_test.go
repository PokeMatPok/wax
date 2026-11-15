package ytm

import "testing"

// TestAutoplay runs live against YouTube Music; skip when offline.
func TestAutoplay(t *testing.T) {
	tracks, err := Autoplay("4c7KPV1naEQ") // Daft Punk - Instant Crush
	if err != nil {
		t.Skipf("live request failed: %v", err)
	}
	if len(tracks) < 3 {
		t.Fatalf("expected several radio tracks, got %d", len(tracks))
	}
	for _, tr := range tracks {
		if tr.VideoID == "" || tr.Title == "" {
			t.Fatalf("malformed track in radio: %+v", tr)
		}
	}
	t.Logf("first 5 radio picks after seed:")
	for i, tr := range tracks {
		if i >= 5 {
			break
		}
		t.Logf("  %s — %s (%s)", tr.Title, tr.Artist, tr.Duration)
	}
}
