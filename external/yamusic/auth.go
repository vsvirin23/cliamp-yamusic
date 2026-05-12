// Device OAuth authentication for Yandex Music.
//
// Uses the Yandex OAuth device-code flow:
//  1. Request confirmation codes from Yandex
//  2. Display a URL + code for the user to visit
//  3. Poll for the token until authorization completes or expires
//  4. Return the token
//
// The well-known application ID for the Yandex Music Windows client is used:
//
//	client_id: 23cabbbdc6cd418abb4b39c32c41195d
//	client_secret: 53bc75238f0c4d08a118e51fe9203300
//
// These same credentials are used by goym and the Python yandex-music-api library.
package yamusic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	yauth "github.com/oklookat/yandexauth/v3"
)

const (
	// Well-known client credentials for the Yandex Music Windows application.
	defaultClientID     = "23cabbbdc6cd418abb4b39c32c41195d"
	defaultClientSecret = "53bc75238f0c4d08a118e51fe9203300"

	deviceID       = "cliamp-001"
	deviceName     = "cliamp"
	authTimeout    = 5 * time.Minute
	yamusicSection = "yandexmusic"
)

// ErrAuthTimeout is returned when the user doesn't complete authorization in time.
var ErrAuthTimeout = errors.New("yamusic: authorization timed out after 5 minutes")

// RunAuthFlow runs the interactive Yandex OAuth device-code flow.
// It prints instructions to stderr, waits for the user to authorize via browser,
// and returns the access token on success.
//
// After getting the token, the caller should save it to config via SaveToken.
func RunAuthFlow() (accessToken string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	defer cancel()

	token, err := yauth.New(
		ctx,
		http.DefaultClient,
		defaultClientID,
		defaultClientSecret,
		deviceID,
		deviceName,
		func(url, code string) {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "━━━ Yandex Music Authorization ━━━")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "  Visit this URL in your browser:\n    %s\n\n", url)
			fmt.Fprintf(os.Stderr, "  Sign in and enter this code:\n    %s\n\n", code)
			fmt.Fprintln(os.Stderr, "  The CLI will continue automatically once you authorize.")
			fmt.Fprintln(os.Stderr, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			fmt.Fprintln(os.Stderr)
		},
	)
	if err != nil {
		return "", fmt.Errorf("yamusic: auth: %w", err)
	}
	if token == nil {
		return "", ErrAuthTimeout
	}

	tokenStr := token.AccessToken
	if tokenStr == "" {
		return "", errors.New("yamusic: auth returned empty access token")
	}

	fmt.Fprintln(os.Stderr, "✓ Authorization successful!")
	return tokenStr, nil
}
