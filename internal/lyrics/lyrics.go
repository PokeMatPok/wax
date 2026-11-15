// Package lyrics fetches (time-synced) lyrics from lrclib.net — a free,
// keyless, crowd-sourced lyrics database.
package lyrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Line is one lyric line with its start offset from track start.
type Line struct {
	Start time.Duration
	Text  string
}

// Lyrics is the result of a lookup.
type Lyrics struct {
	Plain  string // whole lyrics as text (always filled)
	Synced []Line // nil when no synced variant was found
	Track  string
	Artist string
	Album  string
}

type lrclibRecord struct {
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	Plain        string  `json:"plainLyrics"`
	Synced       string  `json:"syncedLyrics"`
}

type Client struct {
	HTTP *http.Client
	ua   string
}

func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 12 * time.Second},
		ua:   "ytm-client/0.1 (desktop; personal)",
	}
}

// Get returns lyrics for a track. duration==0 skips the duration-matched
// /api/get call and goes straight to search.
func (c *Client) Get(ctx context.Context, artist, title string, duration time.Duration) (*Lyrics, error) {
	var rec *lrclibRecord
	if duration > 0 && artist != "" {
		if r, err := c.apiGet(ctx, artist, title, int(duration.Seconds()+0.5)); err == nil && r != nil {
			rec = r
		}
	}
	if rec == nil {
		if r, err := c.apiSearchBest(ctx, artist, title, duration); err == nil && r != nil {
			rec = r
		}
	}
	if rec == nil {
		return nil, fmt.Errorf("no lyrics found for %s - %s", artist, title)
	}
	if rec.Instrumental {
		return &Lyrics{Plain: "(instrumental)", Track: rec.TrackName, Artist: rec.ArtistName}, nil
	}

	l := &Lyrics{
		Plain:  rec.Plain,
		Track:  rec.TrackName,
		Artist: rec.ArtistName,
		Album:  rec.AlbumName,
	}
	if rec.Synced != "" {
		l.Synced = parseLRC(rec.Synced)
	}
	return l, nil
}

func (c *Client) apiGet(ctx context.Context, artist, title string, durSec int) (*lrclibRecord, error) {
	q := url.Values{}
	q.Set("artist_name", artist)
	q.Set("track_name", title)
	if durSec > 0 {
		q.Set("duration", strconv.Itoa(durSec))
	}
	return c.doJSON(ctx, "https://lrclib.net/api/get?"+q.Encode())
}

// apiSearchBest takes the first search hits and keeps the one with the
// closest duration (when duration is known), preferring records with lyrics.
func (c *Client) apiSearchBest(ctx context.Context, artist, title string, duration time.Duration) (*lrclibRecord, error) {
	q := url.Values{}
	q.Set("q", strings.TrimSpace(artist+" "+title))
	list, err := c.doList(ctx, "https://lrclib.net/api/search?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var best *lrclibRecord
	bestScore := -1e18
	for i := range list {
		r := list[i]
		if r.Plain == "" && r.Synced == "" {
			continue
		}
		score := 0.0
		if r.Synced != "" {
			score += 1e6 // synced strongly preferred
		}
		if i == 0 {
			score += 1e5 // ordering matters to lrclib
		}
		if duration > 0 && r.Duration > 0 {
			d := r.Duration - duration.Seconds()
			score -= d * d // exponential-ish penalty for duration mismatch
		}
		if score > bestScore {
			bestScore = score
			best = &list[i]
		}
	}
	return best, nil
}

func (c *Client) doJSON(ctx context.Context, getURL string) (*lrclibRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lrclib HTTP %d", resp.StatusCode)
	}
	var rec lrclibRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, err
	}
	if rec.Plain == "" && rec.Synced == "" {
		return nil, fmt.Errorf("no lyrics")
	}
	return &rec, nil
}

func (c *Client) doList(ctx context.Context, getURL string) ([]lrclibRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lrclib HTTP %d", resp.StatusCode)
	}
	var list []lrclibRecord
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list, nil
}

var lrcStamp = regexp.MustCompile(`^\[(\d+):([0-5]?\d(?:\.\d+)?)\]`)

// parseLRC parses "[mm:ss.xx] text" LRC lines; several stamps may precede one
// text, producing one Line per stamp.
func parseLRC(s string) []Line {
	var out []Line
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		stamps := []time.Duration{}
		for {
			m := lrcStamp.FindStringSubmatch(line)
			if m == nil {
				break
			}
			sec, _ := strconv.ParseFloat(m[2], 64)
			min, _ := strconv.Atoi(m[1])
			stamps = append(stamps, time.Duration(min)*time.Minute+time.Duration(sec*float64(time.Second)))
			line = line[lrcStamp.FindStringIndex(line)[1]:]
		}
		text := strings.TrimSpace(line)
		if text == "" && len(stamps) == 0 {
			continue
		}
		if len(stamps) == 0 {
			continue // metadata line like [ar:...]
		}
		for _, st := range stamps {
			out = append(out, Line{Start: st, Text: text})
		}
	}
	// keep order by start
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start < out[j-1].Start; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
