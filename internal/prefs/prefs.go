// Package prefs stores the user's app preferences (settings toggles) as
// JSON in the same config directory as the listening history. Missing or
// unreadable values fall back to the defaults: everything on.
package prefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"ytm-client/internal/ytm"
)

type Prefs struct {
	Recording    bool `json:"recording"`    // record listening history
	LyricsBubble bool `json:"lyricsBubble"` // glass lyrics overlay
	// VinylHideMin: minutes of no playback before the vinyl record
	// auto-hides (reappears on playback). -1 = never hide.
	VinylHideMin int `json:"vinylHideMin"`
	// Queue: playback queue, autosaved so it survives restarts (even
	// hard power-offs, thanks to the periodic snapshot).
	Queue []ytm.Track `json:"queue,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	path string
	p    Prefs
}

func Load() *Store {
	s := &Store{p: Prefs{Recording: true, LyricsBubble: true}}
	dir, err := os.UserConfigDir()
	if err != nil {
		return s
	}
	s.path = filepath.Join(dir, "ytm-client", "settings.json")
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var p Prefs
	if err := json.Unmarshal(data, &p); err != nil {
		return s
	}
	if p.VinylHideMin == 0 {
		p.VinylHideMin = 3 // missing key / invalid = default 3 min
	}
	s.p = p
	return s
}

// SaveQueue persists the playback queue as part of the prefs file so
// it survives restarts. Safe to call from any goroutine; writes are
// the same background-save pattern as Set.
func (s *Store) SaveQueue(q []ytm.Track) {
	s.mu.Lock()
	s.p.Queue = q
	p := s.p
	path := s.path
	s.mu.Unlock()
	if path == "" {
		return
	}
	go func() {
		if data, err := json.MarshalIndent(p, "", "  "); err == nil {
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, data, 0o644)
		}
	}()
}

// Get returns a copy of the current preferences.
func (s *Store) Get() Prefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p
}

// Set applies new preferences and schedules a background save.
func (s *Store) Set(p Prefs) {
	s.mu.Lock()
	s.p = p
	path := s.path
	s.mu.Unlock()
	if path == "" {
		return
	}
	go func() {
		if data, err := json.MarshalIndent(p, "", "  "); err == nil {
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, data, 0o644)
		}
	}()
}
