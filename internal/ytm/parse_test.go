package ytm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// col builds a flex/fixed column with the given text runs, mirroring the
// musicResponsiveListItemFlexColumnRenderer shape.
func col(texts ...string) map[string]any {
	runs := make([]any, len(texts))
	for i, t := range texts {
		runs[i] = map[string]any{"text": t}
	}
	return map[string]any{
		"musicResponsiveListItemFlexColumnRenderer": map[string]any{
			"text": map[string]any{"runs": runs},
		},
	}
}

func cols(list ...map[string]any) []any {
	out := make([]any, len(list))
	for i, c := range list {
		out[i] = c
	}
	return out
}

// trackItem assembles a musicResponsiveListItemRenderer-shaped map.
func trackItem(videoID string, flex, fixed []any, playLabel string) map[string]any {
	item := map[string]any{
		"flexColumns":  flex,
		"fixedColumns": fixed,
		"navigationEndpoint": map[string]any{
			"watchEndpoint": map[string]any{"videoId": videoID},
		},
	}
	if playLabel != "" {
		item["overlay"] = map[string]any{
			"musicItemThumbnailOverlayRenderer": map[string]any{
				"content": map[string]any{
					"musicPlayButtonRenderer": map[string]any{
						"accessibilityPlayData": map[string]any{
							"accessibilityData": map[string]any{"label": playLabel},
						},
					},
				},
			},
		}
	}
	return item
}

func TestParseTrackSongRow(t *testing.T) {
	item := trackItem("v1",
		cols(col("Get Lucky"), col("Daft Punk", " • ", "Random Access Memories")),
		cols(col("5:38")),
		"Play Get Lucky - Daft Punk")
	tr := parseTrack(item)
	if tr.Title != "Get Lucky" || tr.Artist != "Daft Punk" || tr.Album != "Random Access Memories" {
		t.Fatalf("song row misparsed: %+v", tr)
	}
	if tr.Duration != "5:38" {
		t.Fatalf("duration = %q, want 5:38", tr.Duration)
	}
	if tr.VideoID != "v1" {
		t.Fatalf("videoID = %q", tr.VideoID)
	}
}

func TestParseTrackVideoRowAgeDoesNotLeak(t *testing.T) {
	// "Artist • Video • 3:45 • 10 years ago" — the age stamp must not
	// become the Album (regression: it used to).
	item := trackItem("v2",
		cols(col("Some Video"), col("Some Artist", " • ", "Video", " • ", "3:45", " • ", "10 years ago")),
		cols(col("3:45")),
		"Play Some Video - Some Artist")
	tr := parseTrack(item)
	if tr.Artist != "Some Artist" {
		t.Fatalf("artist = %q, want Some Artist", tr.Artist)
	}
	if tr.Album != "" {
		t.Fatalf("album = %q, want empty (age stamp leaked)", tr.Album)
	}
	if tr.Duration != "3:45" {
		t.Fatalf("duration = %q", tr.Duration)
	}
}

func TestParseTrackLiveRow(t *testing.T) {
	item := trackItem("v3",
		cols(col("Live Concert"), col("Some Artist", " • ", "LIVE")),
		cols(col("LIVE")),
		"Play Live Concert - Some Artist")
	tr := parseTrack(item)
	if tr.Duration != "LIVE" {
		t.Fatalf("duration = %q, want LIVE badge", tr.Duration)
	}
	if tr.Album != "" {
		t.Fatalf("album = %q, want empty (LIVE leaked into metadata)", tr.Album)
	}
	if tr.Artist != "Some Artist" {
		t.Fatalf("artist = %q", tr.Artist)
	}
}

func TestParseTrackPremiereRow(t *testing.T) {
	item := trackItem("v4",
		cols(col("Premiere Night"), col("Some Artist", " • ", "PREMIERE")),
		cols(col("PREMIERE")),
		"Play Premiere Night - Some Artist")
	tr := parseTrack(item)
	if tr.Duration != "PREMIERE" {
		t.Fatalf("duration = %q, want PREMIERE badge", tr.Duration)
	}
	if tr.Album != "" {
		t.Fatalf("album = %q, want empty", tr.Album)
	}
}

func TestParseTrackPlaysFiltered(t *testing.T) {
	item := trackItem("v5",
		cols(col("Harder Better Faster Stronger"), col("Daft Punk", " • ", "Song", " • ", "5:38", " • ", "1.2B plays")),
		cols(col("5:38")),
		"Play Harder Better Faster Stronger - Daft Punk")
	tr := parseTrack(item)
	if tr.Artist != "Daft Punk" || tr.Album != "" {
		t.Fatalf("plays count leaked: %+v", tr)
	}
}

func TestArtistFallbackWithDashInArtist(t *testing.T) {
	// The play label is "Play <title> - <artist>"; an artist containing
	// " - " must not be truncated (regression: LastIndex split inside it).
	item := trackItem("v6",
		cols(col("Highway to Hell")),
		nil,
		"Play Highway to Hell - AC - DC")
	tr := parseTrack(item)
	if tr.Artist != "AC - DC" {
		t.Fatalf("artist = %q, want 'AC - DC'", tr.Artist)
	}
}

func TestArtistFallbackTitleWithDash(t *testing.T) {
	item := trackItem("v7",
		cols(col("Intro - Reprise")),
		nil,
		"Play Intro - Reprise - Daft Punk")
	tr := parseTrack(item)
	if tr.Artist != "Daft Punk" {
		t.Fatalf("artist = %q, want Daft Punk", tr.Artist)
	}
}

// TestParseCapturedSearch runs the parser against a real captured InnerTube
// response (testdata/search.json), fully offline. Regenerate the fixture
// with WAX_DUMP_FIXTURE=1 go test ./internal/ytm/ while online.
func TestParseCapturedSearch(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "search.json"))
	if err != nil {
		t.Skipf("no captured fixture: %v", err)
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	items := findObjects(root, "musicResponsiveListItemRenderer")
	if len(items) < 5 {
		t.Fatalf("fixture has only %d list items; capture a richer response", len(items))
	}
	parsed := 0
	for _, it := range items {
		tr := parseTrack(it)
		if tr.VideoID == "" || tr.Title == "" {
			continue
		}
		parsed++
		for _, bad := range []string{"ago", "plays", "views", "LIVE", "PREMIERE"} {
			if strings.Contains(tr.Album, bad) {
				t.Errorf("album %q leaked non-metadata %q in %+v", tr.Album, bad, tr)
			}
			if strings.Contains(tr.Artist, bad) {
				t.Errorf("artist %q leaked non-metadata %q in %+v", tr.Artist, bad, tr)
			}
		}
	}
	if parsed < 5 {
		t.Errorf("only %d of %d items parsed", parsed, len(items))
	}
}
