// Package cover fetches and caches cover art images for display in lists.
// Get returns a cached image immediately if available; otherwise it kicks
// off a background fetch and reports not-found until it lands.
package cover

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Cache struct {
	mu      sync.Mutex
	items   map[string]image.Image
	pending map[string]bool
	paths   map[string]string // videoID -> on-disk cover file (for SMTC thumbnails)
	client  *http.Client
	// OnLoad is called (async-safe) after each image lands, so the UI can
	// trigger a redraw.
	OnLoad func(videoID string)
}

func New() *Cache {
	return &Cache{
		items:   map[string]image.Image{},
		pending: map[string]bool{},
		paths:   map[string]string{},
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Get returns the image for videoID if cached; otherwise it starts a fetch
// for url in the background and returns (nil, false).
func (c *Cache) Get(videoID, url string) (image.Image, bool) {
	c.mu.Lock()
	if img, ok := c.items[videoID]; ok {
		c.mu.Unlock()
		return img, true
	}
	shouldStart := !c.pending[videoID]
	if shouldStart {
		c.pending[videoID] = true
	}
	c.mu.Unlock()

	if shouldStart && url != "" {
		go c.fetch(videoID, url)
	}
	return nil, false
}

// Path returns the on-disk cover file for videoID if one has been fetched
// (empty string otherwise). SMTC thumbnails need a file path.
func (c *Cache) Path(videoID string) string {
	if videoID == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[videoID]
}

func (c *Cache) fetch(videoID, url string) {
	img, data, err := decode(url)
	if err == nil && len(data) > 0 {
		if p, ok := writeCoverFile(videoID, data); ok {
			c.mu.Lock()
			c.paths[videoID] = p
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	delete(c.pending, videoID)
	if err == nil && img != nil {
		c.items[videoID] = img
	}
	cb := c.OnLoad
	c.mu.Unlock()
	if err == nil && cb != nil {
		cb(videoID)
	}
}

// writeCoverFile tees the raw cover bytes into the config dir so system
// integrations (SMTC thumbnails) can reference them by path.
func writeCoverFile(videoID string, data []byte) (string, bool) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", false
	}
	cdir := filepath.Join(dir, "ytm-client", "covers")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return "", false
	}
	p := filepath.Join(cdir, videoID+".jpg")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", false
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return "", false
	}
	return p, true
}

func decode(url string) (image.Image, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("cover fetch: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, data, err
}
