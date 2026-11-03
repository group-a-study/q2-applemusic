// Package apple is an Apple Music client, as gamdl (github.com/glomatico/gamdl, MIT) talks to
// Apple: the web player's developer token, read from music.apple.com's own script, and the
// account's media-user-token cookie sign each request to the AMP API (the library and the
// catalogue) and to the web player's playback service, which gives a song's AAC 256 stream and
// its Widevine key.
package apple

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/q2"
	"github.com/group-a-study/q2-applemusic/internal/widevine"
)

// Apple's endpoints; tests answer for them.
var (
	HomeURL = "https://music.apple.com"
	AMPURL  = "https://amp-api.music.apple.com"
	PlayURL = "https://play.itunes.apple.com/WebObjects/MZPlay.woa/wa"
)

// userAgent is a desktop browser's, as the web player's requests carry one.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15"

var transport http.RoundTripper = http.DefaultTransport

// UseTransport sets what every request goes through (the add-on's TLS roots; tests' servers).
func UseTransport(rt http.RoundTripper) { transport = rt }

var (
	// ErrSignedOut is a media-user-token Apple refuses (expired, or signed out on the web).
	ErrSignedOut = errors.New("Apple Music refused the sign-in")
	// ErrNoSubscription is an account without an active Apple Music subscription.
	ErrNoSubscription = errors.New("no Apple Music subscription")
	// ErrUnavailable is a song Apple won't play in the account's country.
	ErrUnavailable = errors.New("not available in your country")
	errNoToken     = errors.New("no developer token in music.apple.com's script")
)

// netError is a request that never got an answer.
type netError struct{ err error }

func (e netError) Error() string { return "Apple can't be reached: " + e.err.Error() }
func (e netError) Unwrap() error { return e.err }

// IsNoNetwork is whether err is Apple not answering at all (no Wi-Fi, DNS, a timeout).
func IsNoNetwork(err error) bool {
	var ne netError
	return errors.As(err, &ne)
}

// StatusError is an answer Apple gave with an error status.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("Apple answered %d: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("Apple answered %d", e.Code)
}

// Account is what Apple says of the signed-in account.
type Account struct {
	Active     bool   // an Apple Music subscription
	Storefront string // its country, as the catalogue is asked for: "au"
}

// Client is one account's session.
type Client struct {
	mut       string // the media-user-token
	tokenFile string // where the developer token is kept between runs ("" nowhere)
	Language  string
	dev       *widevine.Device

	mu      sync.Mutex
	token   string
	account *Account
	hc      *http.Client
}

// New is a session for a media-user-token. tokenFile keeps the developer token (it lasts months,
// and finding it costs music.apple.com's 4 MB script); dev is the Widevine device, nil for the
// built-in one.
func New(mediaUserToken, tokenFile string, dev *widevine.Device) *Client {
	if dev == nil {
		dev = widevine.Builtin()
	}
	return &Client{mut: mediaUserToken, tokenFile: tokenFile, Language: "en-US", dev: dev,
		hc: &http.Client{Transport: transport, Timeout: 60 * time.Second}}
}

// wrap marks a request's failure to get an answer as the network's.
func wrap(err error) error {
	var ue *url.Error
	var ne net.Error
	if errors.As(err, &ue) || errors.As(err, &ne) {
		return netError{err}
	}
	return err
}

// tokenShape is a JWT, as the web player's script holds its developer token.
var tokenShape = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// indexJS is the web player's main script, named in its home page.
var indexJS = regexp.MustCompile(`/(assets/index[~-][^/"]+\.js)`)

// devToken is the web player's developer token: kept, from the card, or read from the script.
func (c *Client) devToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	if c.tokenFile != "" {
		if b, err := os.ReadFile(c.tokenFile); err == nil {
			if t := strings.TrimSpace(string(b)); tokenShape.MatchString(t) && !expired(t) {
				c.mu.Lock()
				c.token = t
				c.mu.Unlock()
				return t, nil
			}
		}
	}
	page, err := c.fetch(ctx, HomeURL+"/us/browse", 2<<20)
	if err != nil {
		return "", err
	}
	m := indexJS.FindSubmatch(page)
	if m == nil {
		return "", errNoToken
	}
	js, err := c.fetch(ctx, HomeURL+"/"+string(m[1]), 16<<20)
	if err != nil {
		return "", err
	}
	t := string(tokenShape.Find(js))
	if t == "" {
		return "", errNoToken
	}
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
	if c.tokenFile != "" {
		q2.WriteFile(c.tokenFile, []byte(t+"\n"))
	}
	return t, nil
}

// expired is whether a JWT's exp has passed (or is within a day of it).
func expired(jwt string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return true
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return true
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &claims) != nil || claims.Exp == 0 {
		return false
	}
	return time.Unix(claims.Exp, 0).Before(time.Now().Add(24 * time.Hour))
}

// dropToken forgets a developer token Apple refused, so the next request reads a new one.
func (c *Client) dropToken(tok string) {
	c.mu.Lock()
	if c.token == tok {
		c.token = ""
	}
	c.mu.Unlock()
	if c.tokenFile != "" {
		os.Remove(c.tokenFile)
	}
}

// fetch GETs a page without signing it, at most max bytes.
func (c *Client) fetch(ctx context.Context, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, wrap(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return b, wrap(err)
}

// call is a signed request to Apple (the developer token and the account's cookie, as the web
// player sends them), its JSON answer decoded into out. A refused developer token is read again
// once; a refused sign-in is ErrSignedOut.
func (c *Client) call(ctx context.Context, method, u string, body any, out any) error {
	for try := 0; ; try++ {
		tok, err := c.devToken(ctx)
		if err != nil {
			return err
		}
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Origin", HomeURL)
		req.Header.Set("Referer", HomeURL+"/")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Media-User-Token", c.mut)
		req.Header.Set("X-Apple-Music-User-Token", c.mut)
		req.Header.Set("Cookie", "media-user-token="+c.mut)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return wrap(err)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return wrap(err)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && try == 0:
			c.dropToken(tok)
			continue
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return ErrSignedOut
		case resp.StatusCode == http.StatusNotFound:
			return &StatusError{Code: 404}
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if try < 3 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(2<<try) * time.Second):
				}
				continue
			}
			return &StatusError{Code: resp.StatusCode}
		case resp.StatusCode >= 300:
			return &StatusError{Code: resp.StatusCode, Msg: apiError(b)}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("Apple's answer: %w", err)
		}
		return nil
	}
}

// apiError is the first error's title an AMP answer gives.
func apiError(b []byte) string {
	var e struct {
		Errors []struct{ Title, Detail string } `json:"errors"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Errors) > 0 {
		if e.Errors[0].Detail != "" {
			return e.Errors[0].Detail
		}
		return e.Errors[0].Title
	}
	return ""
}

// Account asks Apple about the account (kept once known): its subscription and country.
func (c *Client) Account(ctx context.Context) (*Account, error) {
	c.mu.Lock()
	a := c.account
	c.mu.Unlock()
	if a != nil {
		return a, nil
	}
	var r struct {
		Meta struct {
			Subscription struct {
				Active     bool   `json:"active"`
				Storefront string `json:"storefront"`
			} `json:"subscription"`
		} `json:"meta"`
	}
	if err := c.call(ctx, http.MethodGet, AMPURL+"/v1/me/account?meta=subscription", nil, &r); err != nil {
		return nil, err
	}
	a = &Account{Active: r.Meta.Subscription.Active, Storefront: strings.ToLower(r.Meta.Subscription.Storefront)}
	if a.Storefront == "" {
		a.Storefront = "us"
	}
	c.mu.Lock()
	c.account = a
	c.mu.Unlock()
	return a, nil
}

// Known is the account as last asked for, nil before.
func (c *Client) Known() *Account {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.account
}

// storefront is the account's country for the catalogue.
func (c *Client) storefront(ctx context.Context) (string, error) {
	a, err := c.Account(ctx)
	if err != nil {
		return "", err
	}
	return a.Storefront, nil
}
