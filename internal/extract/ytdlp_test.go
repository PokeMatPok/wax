package extract

import (
	"context"
	"testing"
)

// Live test: resolves a real YouTube stream through yt-dlp (network needed).
func TestYtDLPResolve(t *testing.T) {
	x, err := NewYtDLP("../../bin/yt-dlp.exe")
	if err != nil {
		t.Skipf("yt-dlp not available: %v", err)
	}
	s, err := x.Resolve(context.Background(), "khnokW3Mw24") // Daft Punk - Instant Crush
	if err != nil {
		t.Skipf("resolve failed (network?): %v", err)
	}
	if s.URL == "" {
		t.Fatal("empty stream URL")
	}
	t.Logf("resolved: %s | %s | %s | dur=%s, url=%.60s...", s.Title, s.Format, s.VideoID, s.Duration, s.URL)
}
