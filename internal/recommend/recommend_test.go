package recommend

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"ytm-client/internal/history"
	"ytm-client/internal/ytm"
)

func entry(id, artist string, daysAgo int, count int, now time.Time) history.Entry {
	return history.Entry{
		Track:      ytm.Track{Title: "T" + id, Artist: artist, VideoID: id},
		LastPlayed: now.Add(-time.Duration(daysAgo) * 24 * time.Hour),
		Count:      count,
	}
}

func TestSeedsRecencyAndDiversity(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	listens := []history.Entry{
		entry("old", "Artist A", 30, 10, now), // old but played a lot
		entry("new1", "Artist A", 0, 1, now),  // fresh
		entry("new2", "Artist A", 1, 1, now),  // fresh, same artist
		entry("new3", "Artist A", 2, 1, now),  // fresh, same artist (over cap)
		entry("mid", "Artist B", 5, 2, now),
	}
	seeds := Seeds(listens, now)
	if len(seeds) == 0 {
		t.Fatal("no seeds")
	}
	// artist A capped at 2
	countA := 0
	for _, s := range seeds {
		if s.Artist == "Artist A" {
			countA++
		}
	}
	if countA > perArtist {
		t.Errorf("artist A has %d seeds, cap is %d", countA, perArtist)
	}
	// the freshest A entry must be in
	found := false
	for _, s := range seeds {
		if s.VideoID == "new1" {
			found = true
		}
	}
	if !found {
		t.Errorf("freshest listen not seeded: %+v", seeds)
	}
}

func TestRefreshConsensusRanking(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := &Store{radio: map[string]radioEntry{}}
	listens := []history.Entry{
		entry("s1", "A", 0, 3, now),
		entry("s2", "B", 1, 2, now),
	}
	fetched := map[string]bool{}
	fetch := func(seedID string) ([]ytm.Track, error) {
		fetched[seedID] = true
		switch seedID {
		case "s1":
			return []ytm.Track{{Title: "Common", VideoID: "c1"}, {Title: "Only1", VideoID: "o1"}}, nil
		case "s2":
			return []ytm.Track{{Title: "Common", VideoID: "c1"}, {Title: "Only2", VideoID: "o2"}}, nil
		}
		return nil, nil
	}
	picks := s.Refresh(listens, fetch, now)
	if len(picks) == 0 {
		t.Fatal("no picks")
	}
	// "Common" is surfaced by both seeds -> must rank first
	if picks[0].VideoID != "c1" {
		t.Errorf("consensus track not first: %+v", picks)
	}
	if len(fetched) != 2 {
		t.Errorf("expected 2 fetches, got %v", fetched)
	}
}

func TestRefreshUsesCacheAndSkipsPlayed(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := &Store{radio: map[string]radioEntry{
		"s1": {
			Tracks: []ytm.Track{
				{Title: "Played", VideoID: "p1"},
				{Title: "Fresh", VideoID: "f1"},
			},
			FetchedAt: now.Add(-time.Hour), // fresh cache
		},
		"p1": {
			Tracks:    []ytm.Track{{Title: "Other", VideoID: "f2"}},
			FetchedAt: now.Add(-time.Hour),
		},
	}}
	listens := []history.Entry{
		entry("s1", "A", 0, 1, now),
		entry("p1", "A", 2, 1, now), // recently played -> excluded from picks
	}
	fetchCalls := 0
	fetch := func(seedID string) ([]ytm.Track, error) {
		fetchCalls++
		return nil, nil
	}
	picks := s.Refresh(listens, fetch, now)
	if fetchCalls != 0 {
		t.Errorf("fresh cache was not used: %d fetches", fetchCalls)
	}
	var hasF1, hasPlayed bool
	for _, p := range picks {
		if p.VideoID == "f1" {
			hasF1 = true
		}
		if p.VideoID == "p1" {
			hasPlayed = true
		}
	}
	if !hasF1 {
		t.Errorf("f1 missing from picks: %+v", picks)
	}
	if hasPlayed {
		t.Errorf("recently played track leaked into picks: %+v", picks)
	}
}

func TestRefreshCapsFetches(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := &Store{radio: map[string]radioEntry{}}
	var listens []history.Entry
	for i := 0; i < maxSeeds; i++ {
		listens = append(listens, entry(string(rune('a'+i)), "Artist"+string(rune('a'+i)), i, 1, now))
	}
	fetchCalls := 0
	fetch := func(seedID string) ([]ytm.Track, error) {
		fetchCalls++
		return []ytm.Track{{Title: "X" + seedID, VideoID: "v" + seedID}}, nil
	}
	s.Refresh(listens, fetch, now)
	if fetchCalls > maxFetches {
		t.Errorf("%d fetches, cap is %d", fetchCalls, maxFetches)
	}
}

func TestStorePersistsRadioCache(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.UserConfigDir()
	// UserConfigDir can't be redirected portably; exercise save/load via
	// the file directly instead.
	_ = old
	s := &Store{path: dir + string(os.PathSeparator) + "recommend.json", radio: map[string]radioEntry{}}
	s.mu.Lock()
	s.radio["seed1"] = radioEntry{
		Tracks:    []ytm.Track{{Title: "Cached", VideoID: "c1"}},
		FetchedAt: time.Now(),
	}
	s.mu.Unlock()
	s.save()

	s2 := &Store{path: s.path, radio: map[string]radioEntry{}}
	data, err := os.ReadFile(s2.path)
	if err != nil {
		t.Fatalf("save wrote nothing: %v", err)
	}
	var disk struct {
		Radio map[string]radioEntry `json:"radio"`
	}
	if json.Unmarshal(data, &disk) != nil || disk.Radio["seed1"].Tracks[0].VideoID != "c1" {
		t.Errorf("round-trip lost the cache entry")
	}
}
