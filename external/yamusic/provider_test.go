package yamusic

import (
	"strings"
	"testing"
)

func TestNewFromConfig_Disabled(t *testing.T) {
	p := NewFromConfig(Config{Enabled: false})
	if p != nil {
		t.Error("NewFromConfig with Enabled=false should return nil")
	}
}

func TestNewFromConfig_EnabledNoAuth(t *testing.T) {
	p := NewFromConfig(Config{Enabled: true})
	if p != nil {
		t.Error("NewFromConfig with Enabled=true but no token should return nil")
	}
}

func TestNewFromConfig_EnabledWithToken(t *testing.T) {
	p := NewFromConfig(Config{Enabled: true, Token: "test-token"})
	if p == nil {
		t.Fatal("NewFromConfig with token should return provider")
	}
	if p.Name() != "Yandex Music" {
		t.Errorf("Name() = %q, want %q", p.Name(), "Yandex Music")
	}
}

func TestNewFromConfig_EnabledWithCookies(t *testing.T) {
	p := NewFromConfig(Config{Enabled: true, CookiesFrom: "chrome"})
	if p == nil {
		t.Fatal("NewFromConfig with cookies_from should return provider")
	}
}

func TestCreateTrackURL(t *testing.T) {
	info := &fullDownloadInfo{
		Host: "s3.music.yandex.net",
		Path: "/mdata/00/1234/abcd.mp3",
		S:    "abc123",
		Ts:   "1234567890",
	}
	codec := "mp3"
	url := createTrackURL(info, codec)
	if url == "" {
		t.Fatal("createTrackURL returned empty string")
	}
	if !strings.HasPrefix(url, "https://s3.music.yandex.net/get-mp3/") {
		t.Errorf("unexpected URL prefix: %s", url)
	}
	// URL should contain the path
	if !strings.Contains(url, "/mdata/00/1234/abcd.mp3") {
		t.Errorf("URL missing track path: %s", url)
	}
	// URL should contain the timestamp
	if !strings.Contains(url, "1234567890") {
		t.Errorf("URL missing timestamp: %s", url)
	}
}

func TestConfigIsSet(t *testing.T) {
	tests := []struct {
		cfg  Config
		want bool
	}{
		{Config{Enabled: false}, false},
		{Config{Enabled: true, Token: ""}, false},
		{Config{Enabled: true, Token: "tok"}, true},
		{Config{Enabled: true, CookiesFrom: "chrome"}, true},
	}
	for _, tc := range tests {
		got := tc.cfg.IsSet()
		if got != tc.want {
			t.Errorf("Config{Enabled=%v, Token=%q}.IsSet() = %v, want %v", tc.cfg.Enabled, tc.cfg.Token, got, tc.want)
		}
	}
}
