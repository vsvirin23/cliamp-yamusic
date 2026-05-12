package yamusic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const cacheFileName = "yamusic_likes_cache.json"
const cacheTTL = 30 * time.Minute

// cachedLikedTracks holds the cached liked tracks data.
type cachedLikedTracks struct {
	mu       sync.RWMutex
	loaded   bool
	cachedAt time.Time
	tracks   []cachedTrack
}

// cachedTrack is a serializable representation of a liked track for caching.
type cachedTrack struct {
	Path         string `json:"path"`
	Title        string `json:"title"`
	Artist       string `json:"artist"`
	Album        string `json:"album"`
	DurationSecs int    `json:"durationSecs"`
	YandexID     string `json:"yandexId"`
}

var likesCache cachedLikedTracks

// cachePath returns the path to the cache file.
func cachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "cliamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, cacheFileName), nil
}

// loadLikedTracksFromCache loads cached liked tracks from disk.
// Returns tracks and true if a valid cache was found, nil otherwise.
func loadLikedTracksFromCache() ([]cachedTrack, bool) {
	likesCache.mu.RLock()
	if likesCache.loaded {
		tracks := make([]cachedTrack, len(likesCache.tracks))
		copy(tracks, likesCache.tracks)
		likesCache.mu.RUnlock()
		return tracks, time.Since(likesCache.cachedAt) < cacheTTL
	}
	likesCache.mu.RUnlock()

	path, err := cachePath()
	if err != nil {
		return nil, false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false
		}
		return nil, false
	}

	var ct cachedLikedTracks
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if err := json.Unmarshal(data, &ct.tracks); err != nil {
		return nil, false
	}
	ct.cachedAt = time.Now()
	ct.loaded = true

	likesCache.mu.Lock()
	likesCache.tracks = ct.tracks
	likesCache.cachedAt = ct.cachedAt
	likesCache.loaded = true
	likesCache.mu.Unlock()

	tracks := make([]cachedTrack, len(ct.tracks))
	copy(tracks, ct.tracks)
	return tracks, true
}

// saveLikedTracksToCache saves liked tracks to disk.
func saveLikedTracksToCache(tracks []cachedTrack) error {
	path, err := cachePath()
	if err != nil {
		return err
	}

	data, err := json.Marshal(tracks)
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}

	likesCache.mu.Lock()
	likesCache.tracks = tracks
	likesCache.cachedAt = time.Now()
	likesCache.loaded = true
	likesCache.mu.Unlock()

	return nil
}
