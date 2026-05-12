// Package yamusic implements a playlist.Provider for Yandex Music.
//
// Yandex Music is opt-in: it only registers when [yandexmusic] enabled = true
// is set in config. Authentication requires either an OAuth access token
// (obtained via browser extension or API) or a browser name for yt-dlp cookie
// extraction (--cookies-from-browser).
//
// Track streaming uses Yandex's MD5-signed direct download URLs. The stream
// URLs are plain HTTP (no DRM) and cliamp plays them natively via ffmpeg or
// its built-in decoders.
package yamusic

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cliamp/playlist"
	"cliamp/provider"
	"cliamp/resolve"
)

// Compile-time interface checks.
var (
	_ playlist.Provider = (*Provider)(nil)
	_ provider.Searcher = (*Provider)(nil)
)

const (
	apiBase        = "https://api.music.yandex.net"
	apiTimeout     = 15 * time.Second
	searchLimit    = 20
	maxTrackRetry  = 3
)

// ErrNotAuthenticated is returned when no valid token or cookie session is available.
var ErrNotAuthenticated = errors.New("yamusic: not authenticated - configure token or cookies_from")

// Config holds settings for the Yandex Music provider.
type Config struct {
	Enabled     bool   // true only when user explicitly sets enabled = true
	Token       string // Yandex Music OAuth access token
	CookiesFrom string // browser name for yt-dlp --cookies-from-browser (e.g. "chrome")
}

// IsSet reports whether the provider should be exposed.
func (c Config) IsSet() bool { return c.Enabled && (c.Token != "" || c.CookiesFrom != "") }

// apiResponse is the generic Yandex Music API response wrapper.
type apiResponse[T any] struct {
	Result   T      `json:"result"`
	InvocationInfo json.RawMessage `json:"invocationInfo"`
	Error    *struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error"`
}

// Artist represents a Yandex Music artist in API responses.
type artist struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Album represents a Yandex Music album in API responses.
type album struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// track represents a Yandex Music track in API responses.
type track struct {
	ID         int      `json:"id"`
	RealID     int      `json:"realId"`
	Title      string   `json:"title"`
	DurationMs int      `json:"durationMs"`
	FileSize   int      `json:"fileSize"`
	Artists    []artist `json:"artists"`
	Albums     []album  `json:"albums"`
	Available  bool     `json:"available"`
}

// playlistItem represents a Yandex Music playlist in API responses.
type playlistItem struct {
	Kind       int      `json:"kind"`
	UID        int      `json:"uid"`
	Title      string   `json:"title"`
	TrackCount int      `json:"trackCount"`
	Owner      struct {
		UID int `json:"uid"`
	} `json:"owner"`
}

// trackDownloadInfo holds variant info for track download.
type trackDownloadInfo struct {
	Codec          string `json:"codec"`
	BitrateInKbps  int    `json:"bitrateInKbps"`
	DownloadInfoUrl string `json:"downloadInfoUrl"`
	Direct         bool   `json:"direct"`
}

// fullDownloadInfo holds the signed download URL parts.
type fullDownloadInfo struct {
	Host string `json:"host"`
	Path string `json:"path"`
	S    string `json:"s"`
	Ts   string `json:"ts"`
}

// searchResult holds Yandex Music search results.
type searchResult struct {
	Tracks struct {
		Results []track `json:"results"`
	} `json:"tracks"`
	Albums struct {
		Results []album `json:"results"`
	} `json:"albums"`
	Artists struct {
		Results []artist `json:"results"`
	} `json:"artists"`
}

// Provider implements playlist.Provider and provider.Searcher for Yandex Music.
type Provider struct {
	token       string
	cookiesFrom string
	httpClient  *http.Client
	userID      int // discovered lazily from AccountStatus

	mu        sync.Mutex
	playlists []playlist.PlaylistInfo
}

// NewFromConfig returns a provider, or nil when Yandex Music is not enabled.
func NewFromConfig(cfg Config) *Provider {
	if !cfg.Enabled {
		return nil
	}
	if cfg.CookiesFrom != "" {
		resolve.SetYTDLCookiesFrom(cfg.CookiesFrom)
	}
	return &Provider{
		token:       strings.TrimSpace(cfg.Token),
		cookiesFrom: strings.TrimSpace(cfg.CookiesFrom),
		httpClient:  &http.Client{Timeout: apiTimeout},
	}
}

func (p *Provider) Name() string { return "Yandex Music" }

// Refresh clears cached playlist state.
func (p *Provider) Refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playlists = nil
	p.userID = 0
}

// ensureAuth returns the auth header value or extracts cookies.
// The token takes precedence; if neither is available, returns an error.
func (p *Provider) ensureAuth(ctx context.Context) (string, error) {
	if p.token != "" {
		return p.token, nil
	}
	if p.cookiesFrom != "" {
		// Cookie extraction via yt-dlp for the browser session.
		// The token path is preferred; cookies are a fallback.
		return "", fmt.Errorf("yamusic: cookie auth not yet implemented, use token instead")
	}
	return "", ErrNotAuthenticated
}

// ensureUserID discovers the user ID from the account status endpoint.
func (p *Provider) ensureUserID(ctx context.Context) (int, error) {
	p.mu.Lock()
	if p.userID != 0 {
		uid := p.userID
		p.mu.Unlock()
		return uid, nil
	}
	p.mu.Unlock()

	token, err := p.ensureAuth(ctx)
	if err != nil {
		return 0, err
	}

	var resp apiResponse[struct {
		Account struct {
			UID int `json:"uid"`
		} `json:"account"`
	}]
	if err := p.apiGet(ctx, "/account/status", nil, token, &resp); err != nil {
		return 0, err
	}
	uid := resp.Result.Account.UID
	if uid == 0 {
		return 0, ErrNotAuthenticated
	}

	p.mu.Lock()
	p.userID = uid
	p.mu.Unlock()
	return uid, nil
}

// Playlists returns the user's playlists from their Yandex Music library.
// Includes user-created playlists, liked tracks, and a "Liked" section.
func (p *Provider) Playlists() ([]playlist.PlaylistInfo, error) {
	p.mu.Lock()
	if p.playlists != nil {
		out := append([]playlist.PlaylistInfo(nil), p.playlists...)
		p.mu.Unlock()
		return out, nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()

	uid, err := p.ensureUserID(ctx)
	if err != nil {
		return nil, err
	}

	token, err := p.ensureAuth(ctx)
	if err != nil {
		return nil, err
	}

	var infos []playlist.PlaylistInfo

	// Get user playlists.
	var playlistsResp apiResponse[[]playlistItem]
	if err := p.apiGet(ctx, fmt.Sprintf("/users/%d/playlists/list", uid), nil, token, &playlistsResp); err == nil {
		for _, pl := range playlistsResp.Result {
			name := strings.TrimSpace(pl.Title)
			if name == "" {
				name = fmt.Sprintf("Playlist #%d", pl.Kind)
			}
			infos = append(infos, playlist.PlaylistInfo{
				ID:         fmt.Sprintf("yamusic:%d:%d", pl.UID, pl.Kind),
				Name:       name,
				TrackCount: pl.TrackCount,
				Section:    "My Playlists",
			})
		}
	}

	// Add liked tracks as a virtual playlist.
	infos = append(infos, playlist.PlaylistInfo{
		ID:      "yamusic:likes",
		Name:    "Liked Tracks",
		Section: "Library",
	})

	p.mu.Lock()
	p.playlists = append([]playlist.PlaylistInfo(nil), infos...)
	p.mu.Unlock()
	return infos, nil
}

// Tracks returns tracks for the given playlist. Supports:
// - user playlists: "yamusic:{uid}:{kind}"
// - liked tracks:  "yamusic:likes"
// - search-based:  "yamusic:search:{query}"
func (p *Provider) Tracks(playlistID string) ([]playlist.Track, error) {
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()

	token, err := p.ensureAuth(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case strings.HasPrefix(playlistID, "yamusic:likes"):
		return p.likedTracks(ctx, token)
	case strings.HasPrefix(playlistID, "yamusic:search:"):
		query := strings.TrimPrefix(playlistID, "yamusic:search:")
		return p.searchTracks(ctx, token, query, searchLimit)
	default:
		// Format: yamusic:{uid}:{kind}
		parts := strings.Split(playlistID, ":")
		if len(parts) < 3 {
			return nil, fmt.Errorf("yamusic: invalid playlist id %q", playlistID)
		}
		kind, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("yamusic: invalid kind in playlist id %q", playlistID)
		}
		return p.playlistTracks(ctx, token, kind)
	}
}

// likedTracks returns the user's liked tracks.
func (p *Provider) likedTracks(ctx context.Context, token string) ([]playlist.Track, error) {
	uid, err := p.ensureUserID(ctx)
	if err != nil {
		return nil, err
	}

	var resp apiResponse[struct {
		Library struct {
			Tracks []track `json:"tracks"`
		} `json:"library"`
	}]
	if err := p.apiGet(ctx, fmt.Sprintf("/users/%d/likes/tracks", uid), nil, token, &resp); err != nil {
		return nil, err
	}

	return p.resolveTrackStreams(ctx, token, resp.Result.Library.Tracks)
}

// playlistTracks returns the tracks in a specific playlist.
func (p *Provider) playlistTracks(ctx context.Context, token string, kind int) ([]playlist.Track, error) {
	uid, err := p.ensureUserID(ctx)
	if err != nil {
		return nil, err
	}

	// Get playlist with track IDs.
	type trackRef struct {
		ID      int `json:"id"`
		AlbumID int `json:"albumId"`
	}
	var rawResp struct {
		Result struct {
			Tracks []trackRef `json:"tracks"`
		} `json:"result"`
	}
	if err := p.apiGetRaw(ctx, fmt.Sprintf("/users/%d/playlists/%d", uid, kind), nil, token, &rawResp); err != nil {
		return nil, err
	}

	if len(rawResp.Result.Tracks) == 0 {
		return nil, nil
	}

	// Fetch track metadata by IDs.
	ids := make([]string, len(rawResp.Result.Tracks))
	for i, t := range rawResp.Result.Tracks {
		ids[i] = strconv.Itoa(t.ID)
	}

	var tracksResp apiResponse[[]track]
	if err := p.apiGet(ctx, "/tracks", url.Values{"track-ids": {strings.Join(ids, ",")}}, token, &tracksResp); err != nil {
		return nil, err
	}

	return p.resolveTrackStreams(ctx, token, tracksResp.Result)
}

// searchTracks searches Yandex Music for tracks matching the query.
func (p *Provider) searchTracks(ctx context.Context, token, query string, limit int) ([]playlist.Track, error) {
	var searchResp apiResponse[searchResult]
	params := url.Values{
		"text":     {query},
		"type":     {"track"},
		"page":     {"0"},
	}
	if limit > 0 {
		params.Set("page-size", strconv.Itoa(limit))
	}
	if err := p.apiGet(ctx, "/search", params, token, &searchResp); err != nil {
		return nil, err
	}

	return p.resolveTrackStreams(ctx, token, searchResp.Result.Tracks.Results)
}

// SearchTracks searches Yandex Music catalog. Implements provider.Searcher.
func (p *Provider) SearchTracks(_ context.Context, query string, limit int) ([]playlist.Track, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()

	token, err := p.ensureAuth(ctx)
	if err != nil {
		return nil, err
	}

	return p.searchTracks(ctx, token, q, limit)
}

// resolveTrackStreams fetches download info for each track and converts to
// cliamp tracks with signed streaming URLs as Path.
func (p *Provider) resolveTrackStreams(ctx context.Context, token string, tracks []track) ([]playlist.Track, error) {
	var result []playlist.Track

	for _, t := range tracks {
		if t.ID == 0 || !t.Available {
			continue
		}

		// Get track download info.
		streamURL, err := p.getTrackStreamURL(ctx, token, t.ID)
		if err != nil {
			// Skip tracks we can't stream.
			continue
		}

		artist := ""
		if len(t.Artists) > 0 {
			names := make([]string, len(t.Artists))
			for i, a := range t.Artists {
				names[i] = a.Name
			}
			artist = strings.Join(names, ", ")
		}
		album := ""
		if len(t.Albums) > 0 {
			album = t.Albums[0].Title
		}

		result = append(result, playlist.Track{
			Path:         streamURL,
			Title:        t.Title,
			Artist:       artist,
			Album:        album,
			Stream:       true,
			DurationSecs: (t.DurationMs + 999) / 1000,
			ProviderMeta: map[string]string{provider.MetaYandexMusicID: strconv.Itoa(t.ID)},
		})
	}

	return result, nil
}

// getTrackStreamURL fetches download info for a track and constructs a signed
// direct streaming URL using the MD5 hash signing method.
func (p *Provider) getTrackStreamURL(ctx context.Context, token string, trackID int) (string, error) {
	var lastErr error
	for i := 0; i < maxTrackRetry; i++ {
		// Step 1: Get download info variants.
		var dowInfos []trackDownloadInfo
		dowResp, err := p.apiGetRawResp(ctx, fmt.Sprintf("/tracks/%d/download-info", trackID), nil, token)
		if err != nil {
			lastErr = err
			continue
		}

		if err := json.Unmarshal(dowResp, &dowInfos); err != nil {
			lastErr = err
			continue
		}

		if len(dowInfos) == 0 {
			lastErr = fmt.Errorf("no download info for track %d", trackID)
			continue
		}

		// Step 2: Pick the best bitrate variant (prefer AAC/MP3 with highest bitrate).
		var bestInfo trackDownloadInfo
		bestBitrate := 0
		for _, info := range dowInfos {
			br := info.BitrateInKbps
			// Prefer higher bitrate. For same bitrate, prefer AAC over MP3.
			if br > bestBitrate || (br == bestBitrate && info.Codec == "aac") {
				bestBitrate = br
				bestInfo = info
			}
		}

		// Step 3: Fetch full download info (signed URL parts).
		fullInfoURL := bestInfo.DownloadInfoUrl + "&format=json"
		fullInfoBody, err := p.httpBody(ctx, fullInfoURL, token)
		if err != nil {
			lastErr = err
			continue
		}

		var info fullDownloadInfo
		if err := json.Unmarshal(fullInfoBody, &info); err != nil {
			lastErr = err
			continue
		}

		// Step 4: Build signed URL (same algorithm as yamusic-tui).
		codec := bestInfo.Codec
		signedURL := createTrackURL(&info, codec)
		return signedURL, nil
	}
	return "", fmt.Errorf("yamusic: failed to get stream URL for track %d: %w", trackID, lastErr)
}

// createTrackURL builds the MD5-signed direct download URL.
// Algorithm (reverse-engineered from Yandex Music API):
//
//	trackUrl = "XGRlBW9FXlekgbPrRHuSiA" + path[1:] + s
//	hash = MD5(trackUrl)
//	url = "https://" + host + "/get-" + codec + "/" + hash + "/" + ts + path
func createTrackURL(info *fullDownloadInfo, codec string) string {
	// The path always starts with / so we skip it.
	trackURL := "XGRlBW9FXlekgbPrRHuSiA" + info.Path[1:] + info.S
	hashSum := md5.Sum([]byte(trackURL))
	hashHex := hex.EncodeToString(hashSum[:])
	return "https://" + info.Host + "/get-" + codec + "/" + hashHex + "/" + info.Ts + info.Path
}

// --- HTTP helpers ---

// apiGet does an authenticated GET to the Yandex Music API and decodes the response.
func (p *Provider) apiGet(ctx context.Context, path string, params url.Values, token string, out any) error {
	return p.apiGetRaw(ctx, path, params, token, out)
}

// apiGetRaw does an authenticated GET and decodes into an arbitrary struct (no apiResponse wrapper).
func (p *Provider) apiGetRaw(ctx context.Context, path string, params url.Values, token string, out any) error {
	endpoint := apiBase + path
	if params != nil {
		endpoint = endpoint + "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "OAuth "+token)
	req.Header.Set("User-Agent", "cliamp/1.0 (yamusic provider)")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("yamusic: http %s", resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("yamusic: decode response: %w", err)
	}
	return nil
}

// apiGetRawResp does an authenticated GET and returns the raw JSON body,
// parsing only the outer apiResponse wrapper to check for errors.
func (p *Provider) apiGetRawResp(ctx context.Context, path string, params url.Values, token string) (json.RawMessage, error) {
	endpoint := apiBase + path
	if params != nil {
		endpoint = endpoint + "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+token)
	req.Header.Set("User-Agent", "cliamp/1.0 (yamusic provider)")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("yamusic: http %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("yamusic: read body: %w", err)
	}

	// Parse the outer wrapper to check for errors.
	var wrapper struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("yamusic: decode response: %w", err)
	}
	return wrapper.Result, nil
}

// httpBody makes an HTTP GET (without the api wrapper) and returns the raw body.
func (p *Provider) httpBody(ctx context.Context, urlStr, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+token)
	req.Header.Set("User-Agent", "cliamp/1.0 (yamusic provider)")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("yamusic: http %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	return body, err
}


