package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Stream is a resolved, directly playable media URL plus metadata.
type Stream struct {
	VideoID  string
	URL      string
	Title    string
	Format   string        // human-readable, e.g. "opus 160k"
	Duration time.Duration // 0 if unknown
}

// Extractor turns a video ID into a playable Stream.
type Extractor interface {
	Resolve(ctx context.Context, videoID string) (*Stream, error)
}

// ytdlp resolves streams by shelling out to yt-dlp (https://yt-dlp.org).
type ytdlp struct {
	bin string
}

// NewYtDLP locates a yt-dlp binary: explicit path, then ./bin (and the
// directory of this executable's ./bin), then PATH.
func NewYtDLP(explicit string) (*ytdlp, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	for _, base := range binBases() {
		candidates = append(candidates, filepath.Join(base, "bin", "yt-dlp.exe"))
		candidates = append(candidates, filepath.Join(base, "bin", "yt-dlp"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return &ytdlp{bin: c}, nil
		}
	}
	if _, err := exec.LookPath("yt-dlp"); err == nil {
		return &ytdlp{bin: "yt-dlp"}, nil
	}
	return nil, fmt.Errorf("yt-dlp not found (expected in ./bin or PATH)")
}

// binBases returns the working directory and the executable's directory.
func binBases() []string {
	bases := []string{"."}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "." {
			bases = append(bases, dir)
		}
	}
	return bases
}

type ytdlpJSON struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
	URL      string  `json:"url"` // present when a single format was requested
	Formats  []struct {
		FormatID   string  `json:"format_id"`
		URL        string  `json:"url"`
		Ext        string  `json:"ext"`
		Acodec     string  `json:"acodec"`
		Vcodec     string  `json:"vcodec"`
		ABR        float64 `json:"abr"`
		AudioExt   string  `json:"audio_ext"`
		FormatNote string  `json:"format_note"`
	} `json:"formats"`
}

// Resolve runs yt-dlp -J on the video and picks the best audio-only format.
// Cancelling ctx kills the yt-dlp process.
func (y *ytdlp) Resolve(ctx context.Context, videoID string) (*Stream, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
	}

	url := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
	cmd := exec.CommandContext(ctx, y.bin, "-J", "--no-playlist", "--no-warnings",
		"-f", "bestaudio", url)
	hideChildConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if e, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("yt-dlp failed: %s", string(e.Stderr))
		}
		_ = ee
		return nil, fmt.Errorf("running yt-dlp: %w", err)
	}

	var data ytdlpJSON
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("parsing yt-dlp output: %w", err)
	}

	streamURL := data.URL
	format := "best"
	if streamURL == "" {
		best := -1.0
		for _, f := range data.Formats {
			if f.Vcodec != "none" || f.Acodec == "none" || f.Acodec == "" {
				continue
			}
			if f.ABR > best {
				best = f.ABR
				streamURL = f.URL
				format = fmt.Sprintf("%s %s %.0fk", f.Ext, f.Acodec, f.ABR)
			}
		}
	}
	if streamURL == "" {
		return nil, fmt.Errorf("yt-dlp returned no audio URL for %s", videoID)
	}

	s := &Stream{
		VideoID: videoID,
		URL:     streamURL,
		Title:   data.Title,
		Format:  format,
	}
	if data.Duration > 0 {
		s.Duration = time.Duration(data.Duration * float64(time.Second))
	}
	return s, nil
}
