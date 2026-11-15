package ytm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// No API key required.
const (
	innertubeNextURL   = "https://music.youtube.com/youtubei/v1/next?prettyPrint=false"
	innertubeBrowseURL = "https://music.youtube.com/youtubei/v1/browse?prettyPrint=false"
)

// Autoplay returns the radio tracks YouTube Music computes to follow
// videoID (its autoplay/ripple order). The seed track itself is excluded.
//
// The anonymous next endpoint does not fill the playback queue, but it
// hands out the session browse id of the track's radio ("Related") shelf,
// which the browse endpoint serves without sign-in.
func Autoplay(videoID string) ([]Track, error) {
	relatedID, err := relatedShelfID(videoID)
	if err != nil {
		return nil, err
	}
	return fetchShelf(relatedID, videoID)
}

// relatedShelfID asks next for the seed track and digs the radio shelf
// browse id (MUSIC_PAGE_TYPE_TRACK_RELATED) out of the watch-next tabs.
func relatedShelfID(videoID string) (string, error) {
	payload := map[string]any{
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "WEB_REMIX",
				"clientVersion": "1.20240403.01.00",
				"hl":            "en",
				"gl":            "US",
			},
		},
		"videoId":    videoID,
		"playlistId": "RDAMVM" + videoID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, innertubeNextURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	req.Header.Set("Cookie", "SOCS=CAI; CONSENT=YES+cb")

	resp, err := ytClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("autoplay request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("youtube returned HTTP %d: %s", resp.StatusCode, string(data))
	}

	var root any
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return "", fmt.Errorf("decoding youtube response: %w", err)
	}
	for _, tab := range findObjects(root, "tabRenderer") {
		ep, ok := tab["endpoint"].(map[string]any)
		if !ok {
			continue
		}
		be, ok := ep["browseEndpoint"].(map[string]any)
		if !ok {
			continue
		}
		cfgs, ok := be["browseEndpointContextSupportedConfigs"].(map[string]any)
		if !ok {
			continue
		}
		cfg, ok := cfgs["browseEndpointContextMusicConfig"].(map[string]any)
		if !ok {
			continue
		}
		if pageType, _ := cfg["pageType"].(string); pageType == "MUSIC_PAGE_TYPE_TRACK_RELATED" {
			id, _ := be["browseId"].(string)
			if id != "" {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("autoplay: no radio shelf offered for %s", videoID)
}

// fetchShelf browses a Related/radio shelf and parses its track rows.
func fetchShelf(browseID, seedID string) ([]Track, error) {
	payload := map[string]any{
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "WEB_REMIX",
				"clientVersion": "1.20240403.01.00",
				"hl":            "en",
				"gl":            "US",
			},
		},
		"browseId": browseID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, innertubeBrowseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	req.Header.Set("Cookie", "SOCS=CAI; CONSENT=YES+cb")

	resp, err := ytClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("autoplay browse failed: %w", err)
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
	for _, item := range items {
		t := parseTrack(item)
		if t.VideoID == "" || t.VideoID == seedID || t.Title == "" {
			continue
		}
		tracks = append(tracks, t)
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("autoplay: empty radio shelf for %s (%d rows)", seedID, len(items))
	}
	return tracks, nil
}

// runsText extracts m[key] as plain text from "{ runs | simpleText }".
func runsText(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	mm, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := mm["simpleText"].(string); ok {
		return s
	}
	runs, ok := mm["runs"].([]any)
	if !ok || len(runs) == 0 {
		return ""
	}
	r0, ok := runs[0].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := r0["text"].(string)
	return s
}
