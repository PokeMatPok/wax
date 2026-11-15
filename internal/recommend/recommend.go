// Package recommend computes the Home tab's "For you" picks from the
// listening history.
//
// Model: history entries are scored as radio seeds (recency with a ~7 day
// half-life, boosted by play count, diversified to at most two seeds per
// artist). Each seed's radio (related tracks) result is cached on disk with
// a 24h TTL so refreshes normally cost zero network requests. Candidates
// are then ranked by consensus — tracks that several different seeds'
// radios surface rank highest — and anything already in the recent
// history is excluded.
package recommend

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"ytm-client/internal/history"
	"ytm-client/internal/ytm"
)

const (
	radioTTL   = 24 * time.Hour // cached radio results are considered fresh
	maxSeeds   = 6              // seeds consulted per refresh
	perArtist  = 2              // seed diversity cap per artist
	maxFetches = 3              // network radio fetches allowed per refresh
	maxPicks   = 18
)

// FetchFunc resolves a seed's radio tracks. Network-backed in the app,
// stubbed in tests.
type FetchFunc func(seedID string) ([]ytm.Track, error)

type radioEntry struct {
	Tracks    []ytm.Track `json:"tracks"`
	FetchedAt time.Time   `json:"fetchedAt"`
}

// Store is the on-disk radio cache (recommend.json in the config dir).
// Safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	path  string
	radio map[string]radioEntry
}

func Open() *Store {
	dir, err := os.UserConfigDir()
	if err != nil {
		return &Store{radio: map[string]radioEntry{}}
	}
	s := &Store{
		path:  filepath.Join(dir, "ytm-client", "recommend.json"),
		radio: map[string]radioEntry{},
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var disk struct {
		Radio map[string]radioEntry `json:"radio"`
	}
	if json.Unmarshal(data, &disk) == nil && disk.Radio != nil {
		s.radio = disk.Radio
	}
	return s
}

func (s *Store) save() {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	out := struct {
		Radio map[string]radioEntry `json:"radio"`
	}{Radio: s.radio}
	s.mu.Unlock()
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("recommend: save: %v", err)
	}
}

// seedScore ranks a history entry as a radio seed: recency (7-day
// half-life) scaled by a logarithmic play-count boost.
func seedScore(e history.Entry, now time.Time) float64 {
	age := now.Sub(e.LastPlayed)
	if age < 0 {
		age = 0
	}
	recency := math.Pow(0.5, age.Hours()/(7*24))
	count := 1.0
	if e.Count > 1 {
		count = 1 + math.Log2(float64(e.Count))
	}
	return recency * count
}

// Seeds picks the best-scoring history entries as radio seeds, capped at
// two per artist for diversity.
func Seeds(listens []history.Entry, now time.Time) []history.Entry {
	scored := make([]history.Entry, len(listens))
	copy(scored, listens)
	sort.SliceStable(scored, func(i, j int) bool {
		return seedScore(scored[i], now) > seedScore(scored[j], now)
	})
	var out []history.Entry
	perArtistCount := map[string]int{}
	for _, e := range scored {
		if e.VideoID == "" {
			continue
		}
		if perArtistCount[e.Artist] >= perArtist {
			continue
		}
		perArtistCount[e.Artist]++
		out = append(out, e)
		if len(out) == maxSeeds {
			break
		}
	}
	return out
}

// Refresh computes ranked picks for the given listening history. Radio
// results come from the cache when fresh; fetch is called for at most
// maxFetches stale or missing seeds (a failed fetch falls back to a
// stale cache entry when one exists). Deterministic for the same inputs.
func (s *Store) Refresh(listens []history.Entry, fetch FetchFunc, now time.Time) []ytm.Track {
	if s == nil || len(listens) == 0 {
		return nil
	}
	seeds := Seeds(listens, now)

	played := make(map[string]bool, len(listens))
	for _, e := range listens {
		played[e.VideoID] = true
	}

	fetches := 0
	type candidate struct {
		track     ytm.Track
		score     float64
		firstSeen int // stable tie-break: earlier seeds win
	}
	cands := map[string]*candidate{}

	for si, seed := range seeds {
		entry, ok := s.radio[seed.VideoID]
		stale := !ok || now.Sub(entry.FetchedAt) > radioTTL
		if stale && fetches < maxFetches && fetch != nil {
			if tracks, err := fetch(seed.VideoID); err == nil && len(tracks) > 0 {
				entry = radioEntry{Tracks: tracks, FetchedAt: now}
				s.mu.Lock()
				s.radio[seed.VideoID] = entry
				s.mu.Unlock()
				fetches++
			} else if ok {
				// keep the stale result rather than nothing
				stale = false
			}
		}
		if len(entry.Tracks) == 0 {
			continue
		}
		seedW := 1 + seedScore(seed, now)
		for _, t := range entry.Tracks {
			if t.VideoID == "" || played[t.VideoID] {
				continue
			}
			c, ok := cands[t.VideoID]
			if !ok {
				c = &candidate{track: t, firstSeen: si}
				cands[t.VideoID] = c
			}
			// consensus: each additional seed surfacing the track counts
			// double, weighted by that seed's own score
			c.score += seedW
			if si > 0 {
				c.score += seedW
			}
		}
	}
	if fetches > 0 {
		s.save()
	}

	out := make([]*candidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].firstSeen < out[j].firstSeen
	})
	picks := make([]ytm.Track, 0, maxPicks)
	for _, c := range out {
		picks = append(picks, c.track)
		if len(picks) == maxPicks {
			break
		}
	}
	return picks
}
