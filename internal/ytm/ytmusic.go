package ytm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// InnerTube (the private web API YouTube Music's own web player uses)
// client context. Same family of clients that NewPipe/yt-dlp use.
// No API key required.
const innertubeSearchURL = "https://music.youtube.com/youtubei/v1/search?prettyPrint=false"

type Track struct {
	Title     string
	Artist    string
	Album     string
	Duration  string
	VideoID   string
	Thumbnail string // URL of the largest result thumbnail, empty if none
}

var ytClient = &http.Client{Timeout: 20 * time.Second}

// SearchYTMusic runs a YouTube Music search and returns the track results.
func SearchYTMusic(query string) ([]Track, error) {
	payload := map[string]any{
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "WEB_REMIX",
				"clientVersion": "1.20240403.01.00",
				"hl":            "en",
				"gl":            "US",
			},
		},
		"query": query,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, innertubeSearchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := ytClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("youtube returned HTTP %d: %s", resp.StatusCode, string(data))
	}

	var root any
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, fmt.Errorf("decoding youtube response: %w", err)
	}

	items := findObjects(root, "musicResponsiveListItemRenderer")
	var tracks []Track
	for _, node := range items {
		if t := parseTrack(node); t.VideoID != "" && t.Title != "" {
			tracks = append(tracks, t)
		}
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("no results for %q: %d item nodes, none parseable (response layout changed?)", query, len(items))
	}
	return tracks, nil
}

// findObjects walks decoded JSON and collects every value stored under key.
func findObjects(node any, key string) []map[string]any {
	var out []map[string]any
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == key {
				switch it := v.(type) {
				case map[string]any:
					out = append(out, it)
				case []any:
					for _, el := range it {
						if m, ok := el.(map[string]any); ok {
							out = append(out, m)
						}
					}
				}
			}
			out = append(out, findObjects(v, key)...)
		}
	case []any:
		for _, it := range n {
			out = append(out, findObjects(it, key)...)
		}
	}
	return out
}

func parseTrack(item map[string]any) Track {
	var t Track

	if pid, ok := item["playlistItemData"].(map[string]any); ok {
		t.VideoID, _ = pid["videoId"].(string)
	}
	if t.VideoID == "" {
		if nav, ok := item["navigationEndpoint"].(map[string]any); ok {
			if w, ok := nav["watchEndpoint"].(map[string]any); ok {
				t.VideoID, _ = w["videoId"].(string)
			}
		}
	}

	var texts []string
	if cols, ok := item["flexColumns"].([]any); ok {
		for i, c := range cols {
			if s := flexColumnText(c); s != "" {
				if i > 0 || t.Title == "" {
					texts = append(texts, s)
				}
			}
		}
	}
	if cols, ok := item["fixedColumns"].([]any); ok {
		for _, c := range cols {
			if s := flexColumnText(c); s != "" {
				texts = append(texts, s)
			}
		}
	}

	// The subtitle is columns joined with bullets, e.g.
	// "Daft Punk • Random Access Memories", "Song • 5:38 • 1.2B plays".
	// Inside a column the bullet is a separate run; between columns it is not
	// present at all, so join column texts with the bullet before splitting.
	title := texts[0]
	var rest []string
	if len(texts) > 1 {
		rest = splitParts(strings.Join(texts[1:], " • "), " • ")
	}
	for _, p := range rest {
		if isDuration(p) {
			t.Duration = p
			break
		}
	}
	for _, p := range rest {
		if _, ok := nonMetadata(p); !ok {
			continue
		}
		if t.Artist == "" {
			t.Artist = p
		} else if t.Album == "" {
			t.Album = p
		}
	}
	t.Title = title
	if t.Artist == "" {
		t.Artist = artistFromAccessibility(item)
	}
	if th, ok := item["thumbnail"].(map[string]any); ok {
		t.Thumbnail = largestThumbnail(th)
	}
	return t
}

// largestThumbnail digs thumbnails out of a musicThumbnailRenderer wrapper
// and returns the widest URL.
func largestThumbnail(wrapper map[string]any) string {
	mtr, ok := wrapper["musicThumbnailRenderer"].(map[string]any)
	if !ok {
		return ""
	}
	th, ok := mtr["thumbnail"].(map[string]any)
	if !ok {
		return ""
	}
	list, ok := th["thumbnails"].([]any)
	if !ok {
		return ""
	}
	best := ""
	bestW := 0
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		u, _ := m["url"].(string)
		w := 0
		if fv, ok := m["width"].(float64); ok {
			w = int(fv)
		}
		if u != "" && w >= bestW {
			best, bestW = u, w
		}
	}
	return best
}

// artistFromAccessibility extracts the fallback artist from the play button's
// accessibility label, which looks like "Play <title> - <artist>".
func artistFromAccessibility(item map[string]any) string {
	overlay, ok := item["overlay"].(map[string]any)
	if !ok {
		return ""
	}
	o, ok := overlay["musicItemThumbnailOverlayRenderer"].(map[string]any)
	if !ok {
		return ""
	}
	c, ok := o["content"].(map[string]any)
	if !ok {
		return ""
	}
	p, ok := c["musicPlayButtonRenderer"].(map[string]any)
	if !ok {
		return ""
	}
	apd, ok := p["accessibilityPlayData"].(map[string]any)
	if !ok {
		return ""
	}
	ad, ok := apd["accessibilityData"].(map[string]any)
	if !ok {
		return ""
	}
	label, _ := ad["label"].(string)
	label = strings.TrimSpace(strings.TrimPrefix(label, "Play "))
	if i := strings.LastIndex(label, " - "); i >= 0 {
		return strings.TrimSpace(label[i+3:])
	}
	return ""
}

// nonMetadata returns the part unchanged if it is real metadata (artist,
// album, ...), and the empty string if it is duration, a type label, or
// view/play counts.
func nonMetadata(p string) (string, bool) {
	if isDuration(p) || isTypeLabel(p) {
		return "", false
	}
	if strings.HasSuffix(p, " plays") || strings.HasSuffix(p, " views") {
		return "", false
	}
	return p, true
}

func splitParts(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isDuration(s string) bool {
	// m:ss or h:mm:ss
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for _, c := range s {
		if c != ':' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func isTypeLabel(s string) bool {
	switch s {
	case "Song", "Video", "Single", "EP", "Album", "Playlist", "Artist", "Live", "Radio", "Top result", "Top Result":
		return true
	}
	return false
}

// flexColumnText returns the concatenated runs of the text in a
// musicResponsiveListItemFlexColumnRenderer.
func flexColumnText(col any) string {
	m, ok := col.(map[string]any)
	if !ok {
		return ""
	}
	r, ok := m["musicResponsiveListItemFlexColumnRenderer"].(map[string]any)
	if !ok {
		return ""
	}
	txt, ok := r["text"].(map[string]any)
	if !ok {
		return ""
	}
	runs, ok := txt["runs"].([]any)
	if !ok {
		return ""
	}
	var s string
	for _, run := range runs {
		if rm, ok := run.(map[string]any); ok {
			if text, ok := rm["text"].(string); ok {
				s += text
			}
		}
	}
	return s
}
