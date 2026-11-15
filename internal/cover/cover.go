// Package cover fetches and caches cover art images for display in lists.
// Get returns a cached image immediately if available; otherwise it kicks
// off a background fetch and reports not-found until it lands.
package cover

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"sync"
	"time"
)

type Cache struct {
	mu      sync.Mutex
	items   map[string]image.Image
	pending map[string]bool
	client  *http.Client
	// OnLoad is called (async-safe) after each image lands, so the UI can
	// trigger a redraw.
	OnLoad func(videoID string)
}

func New() *Cache {
	return &Cache{
		items:   map[string]image.Image{},
		pending: map[string]bool{},
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

func (c *Cache) fetch(videoID, url string) {
	img, err := decode(url)
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

func decode(url string) (image.Image, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}
