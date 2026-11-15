package main

import (
	"context"
	"testing"
	"time"

	"ytm-client/internal/extract"
	"ytm-client/internal/player"
)

// TestResolveAndPlay is the end-to-end smoke: yt-dlp resolves a real stream
// and mpv plays a couple of seconds of it. Skips without network/bins.
func TestResolveAndPlay(t *testing.T) {
	x, err := extract.NewYtDLP("../../bin/yt-dlp.exe")
	if err != nil {
		t.Skip(err)
	}
	s, err := x.Resolve(context.Background(), "khnokW3Mw24")
	if err != nil {
		t.Skipf("resolve (network?): %v", err)
	}

	e, err := player.NewMpv() // resolves ./bin/mpv.exe or PATH
	if err != nil {
		t.Skipf("mpv not found: %v", err)
	}
	defer e.Close()
	if err := e.Start(); err != nil {
		t.Skipf("mpv start: %v", err)
	}
	if err := e.Play(s.URL); err != nil {
		t.Fatalf("play real stream: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := e.Status()
		if st.State == player.StatePlaying && st.Position > time.Second {
			t.Logf("playing real stream: pos=%s dur=%s state=%s", st.Position, st.Duration, st.State)
			e.Stop()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("did not reach 1s of playback: %+v", e.Status())
}
