// Package history keeps a persistent record of listened tracks: every
// distinct VideoID with its play count and the time it was last played.
// The store lives in the user's config directory (Windows: %AppData%)
// under ytm-client/history.json.
package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"ytm-client/internal/ytm"
)

// Entry is one distinct track's aggregated listening record. Track is
// embedded so the JSON round-trips the metadata we need to re-render a
// playable row.
type Entry struct {
	ytm.Track
	Count      int       `json:"count"`
	LastPlayed time.Time `json:"lastPlayed"`
}

// Store is safe for concurrent use; file writes happen in the background.
type Store struct {
	mu      sync.Mutex
	byID    map[string]Entry // keyed by VideoID ("" never stored)
	path    string
	pending bool // a save goroutine is running
	dirty   bool // another Add arrived during the save goroutine
}

type fileFormat struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Load reads the store from disk, creating an empty one if the file is
// missing or unreadable. A nil store is valid for use (nothing recorded,
// no persistence).
func Load() *Store {
	s := &Store{byID: map[string]Entry{}}
	dir, err := os.UserConfigDir()
	if err != nil {
		return s // no config dir: keep in memory only
	}
	s.path = filepath.Join(dir, "ytm-client", "history.json")
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return s
	}
	for id, e := range f.Entries {
		if id != "" && e.VideoID != "" {
			s.byID[id] = e
		}
	}
	return s
}

// Add records a listen: bumps the count, refreshes the timestamp and
// metadata, and schedules a background save.
func (s *Store) Add(t ytm.Track, at time.Time) {
	if t.VideoID == "" {
		return
	}
	s.mu.Lock()
	e, ok := s.byID[t.VideoID]
	if !ok {
		e = Entry{Track: t, Count: 0}
	}
	e.Track = t
	e.Count++
	e.LastPlayed = at
	s.byID[t.VideoID] = e
	s.dirty = true
	if !s.pending {
		s.pending = true
		s.dirty = false
	}
	s.mu.Unlock()
	go s.save()
}

// save writes the file; coalesces concurrent adds via the dirty flag.
func (s *Store) save() {
	for {
		s.mu.Lock()
		path := s.path
		if path == "" {
			s.pending = false
			s.mu.Unlock()
			return
		}
		f := fileFormat{Version: 1, Entries: s.byID}
		s.dirty = false
		s.mu.Unlock()

		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if data, err := json.MarshalIndent(f, "", "  "); err == nil {
				tmp := path + ".tmp"
				if err := os.WriteFile(tmp, data, 0o644); err == nil {
					os.Rename(tmp, path)
				}
			}
		}

		s.mu.Lock()
		if !s.dirty {
			s.pending = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}

// Clear wipes every entry and deletes the file on disk. A save that is
// already in flight may recreate an empty file; harmless.
func (s *Store) Clear() {
	s.mu.Lock()
	s.byID = map[string]Entry{}
	s.dirty = false
	s.pending = false
	path := s.path
	s.mu.Unlock()
	if path != "" {
		os.Remove(path)
	}
}

// Recent returns up to n entries, newest first. The result is a copy.
func (s *Store) Recent(n int) []Entry {
	if s == nil || n <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]Entry, 0, len(s.byID))
	for _, e := range s.byID {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if !a.LastPlayed.Equal(b.LastPlayed) {
			return a.LastPlayed.After(b.LastPlayed)
		}
		return a.Count > b.Count
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}
