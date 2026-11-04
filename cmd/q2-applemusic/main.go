// q2-applemusic is a Q2 Pod card app (contrib/shanlingq2/README) that does Apple Music as gamdl
// does: the Q2 browses the signed-in account's library and Apple Music's catalogue on its own page,
// over Q2SPOT/1, and what it picks is downloaded as AAC 256 kbps, decrypted (the web player's
// Widevine key) and tagged into "Apple Music/" on the card, in Artist/Year - Album folders unless
// Organize Files is off; a playlist's songs are kept as an album of their own, in Playlists/. Every
// song then plays from the card, as local music. In Stream mode (stream.go) a press plays the song
// instead, fetched into a hidden folder that keeps none. An Apple Music subscription is needed. It
// runs from the card's .applemusic folder (copied to /tmp first) and is signed in from a phone with
// the account's media-user-token cookie.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/apple"
	"github.com/group-a-study/q2-applemusic/internal/q2"
	"github.com/group-a-study/q2-applemusic/internal/q2spot"
	"github.com/group-a-study/q2-applemusic/internal/tlsroots"
	"github.com/group-a-study/q2-applemusic/internal/widevine"
)

const version = "q2-v1.5"

// transport carries every request to Apple: TLS checked against the add-on's own roots, as the
// firmware has none. Tests answer for Apple instead.
var transport = func() http.RoundTripper { return tlsroots.Client(0).Transport }

// idleFor is how long it stays with nothing to download and its page unused, before it stops.
var idleFor = 15 * time.Minute

// The sign-in, as last found: what the Q2's page shows without asking Apple each time.
const (
	authUnknown = iota // not asked yet
	authNoToken        // no media-user-token on the card
	authOK             // signed in, with a subscription
	authFree           // signed in, without one: no downloads
	authExpired        // Apple refused the token
	authOffline        // Apple couldn't be reached
)

// tokenFile is the account's media-user-token in the add-on's folder.
const tokenFile = "media-user-token"

type daemon struct {
	dir     string
	state   *q2.State
	cache   *cache
	signin  *signin
	dl      *downloads
	st      *streamer
	browse  string
	control string
	dev     *widevine.Device

	mu   sync.Mutex
	am   *apple.Client
	tok  string
	auth int
	org  bool      // Organize Files: Artist/Year - Album folders
	strm bool      // Stream mode: a press plays a song, not downloads it
	used time.Time // the Q2's page, last
	quit chan struct{}
	once sync.Once

	cookies    map[string]cookieSeen // cookies files looked at (cookies.go)
	cookieNote string                // why the last one didn't sign in
	importing  atomic.Bool           // a look for cookies files under way
}

// signinPort is where the sign-in page listens (the next free one if taken).
var signinPort = 8100

func main() {
	dir := flag.String("dir", "/mnt/mmc/.applemusic", "the add-on's folder on the card")
	card := flag.String("card", "/mnt/mmc", "the card's mount point; downloads go to its Apple Music folder")
	statusFile := flag.String("status-file", "/tmp/q2-applemusic.state", "the status file Q2 Pod reads")
	browse := flag.String("browse-socket", "/tmp/q2-applemusic.browse.sock", "Q2SPOT/1 for the Q2's page")
	control := flag.String("control-socket", "/tmp/q2-applemusic.sock", "datagrams: q quits")
	flag.IntVar(&signinPort, "signin-port", signinPort, "the sign-in page's port, on the Wi-Fi")
	flag.Parse()
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(40 << 20)
	q2.Card = *card
	d := start(*dir, filepath.Join(*card, "Apple Music"), *statusFile, *browse, *control)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; d.shutdown() }()
	d.run()
	os.Exit(0)
}

func start(dir, folder, statusFile, browse, control string) *daemon {
	apple.UseTransport(transport())
	tlsroots.DateURL = "http://www.apple.com/"
	d := &daemon{dir: dir, state: &q2.State{Path: statusFile}, cache: newCache(), browse: browse, control: control,
		used: time.Now(), quit: make(chan struct{}), cookies: map[string]cookieSeen{}}
	// A device of the card's own (.applemusic/device.wvd), should Apple stop taking the built-in one.
	if b, err := os.ReadFile(filepath.Join(dir, "device.wvd")); err == nil {
		d.dev, _ = widevine.ParseWVD(b)
	}
	d.org = readOrganize(dir)
	d.strm = readMode(dir)
	d.dl = newDownloads(d, folder, filepath.Join(dir, "queue.json"), filepath.Join(dir, "bulk.json"))
	base := strings.TrimSuffix(statusFile, ".state") // Q2 Pod's names: /tmp/q2-applemusic.play, .playing
	d.st = newStreamer(d, filepath.Join(dir, "stream"), base+".play", base+".playing")
	d.startSignin(signinPort)
	d.client() // reads the token, if the card has one
	d.writeState()
	go func() { d.lookForCookies(); d.checkAuth() }()
	q2.Control(control, func(cmd []byte) {
		if len(cmd) > 0 && cmd[0] == 'q' {
			d.shutdown()
		}
	})
	go q2spot.Serve(browse, d.device, d.quit)
	go d.dl.firstScan() // after the socket: the card can take longer to read than Q2 Pod waits
	return d
}

// run downloads until asked to stop, writes the status file every second, asks Apple again while
// it couldn't be reached, and stops by itself once idle: nothing to download, its page unused.
func (d *daemon) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.dl.run(ctx); close(done) }()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-d.quit:
			cancel()
			<-done // the download under way ended, its .part gone; the queue saved
			d.st.stop()
			d.state.Remove()
			os.Remove(d.browse)
			os.Remove(d.control)
			return
		case <-t.C:
			d.writeState()
			if n%30 == 0 && d.authState() == authOffline {
				go d.checkAuth()
			}
			if n%cookieEvery == 0 {
				go d.lookForCookies()
			}
			d.mu.Lock()
			idle := time.Since(d.used)
			d.mu.Unlock()
			if !d.dl.pending() && !d.st.active() && idle > idleFor {
				d.shutdown()
			}
		}
	}
}

func (d *daemon) shutdown() { d.once.Do(func() { close(d.quit) }) }

func (d *daemon) touch() {
	d.mu.Lock()
	d.used = time.Now()
	d.mu.Unlock()
}

// client is the session for the card's token, made when the file appears or changes; nil without
// one. Apple isn't asked yet: session does that.
func (d *daemon) client() *apple.Client {
	b, _ := os.ReadFile(filepath.Join(d.dir, tokenFile))
	tok := strings.TrimSpace(string(b))
	d.mu.Lock()
	defer d.mu.Unlock()
	if tok != d.tok || (tok == "" && d.auth != authNoToken) {
		d.tok, d.am, d.auth = tok, nil, authUnknown
		if tok != "" {
			d.am = apple.New(tok, filepath.Join(d.dir, "devtoken"), d.dev)
		} else {
			d.auth = authNoToken
		}
		d.cache.clear()
	}
	return d.am
}

var errNoToken = errors.New("no media-user-token on the card")

// session is the card's session, its account known (asked once); one without a subscription
// signs in, but downloads refuse it.
func (d *daemon) session() (*apple.Client, error) {
	am := d.client()
	if am == nil {
		return nil, errNoToken
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := am.Account(ctx)
	switch {
	case errors.Is(err, apple.ErrSignedOut):
		d.setAuth(authExpired)
		return nil, err
	case apple.IsNoNetwork(err):
		d.setAuth(authOffline)
		return nil, err
	case err != nil:
		return nil, err
	case a.Active:
		d.setAuth(authOK)
	default:
		d.setAuth(authFree)
	}
	return am, nil
}

func (d *daemon) authState() int {
	d.client()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.auth
}

// setAuth notes the sign-in, and rewrites the status file when it changed (the sign-in page's code
// comes or goes); a sign-in that works again starts the queue.
func (d *daemon) setAuth(a int) {
	d.mu.Lock()
	changed := d.auth != a
	d.auth = a
	d.mu.Unlock()
	if changed {
		d.writeState()
		if a == authOK {
			d.dl.poke()
		}
	}
}

// checkAuth asks Apple whether the card's token works.
func (d *daemon) checkAuth() { d.session() }

// signedIn is a new token saved by the sign-in page.
func (d *daemon) signedIn() {
	d.client()
	d.checkAuth()
}

// writeState is what the Q2 reads: whether to hold standby off (a song downloading), and, while
// not signed in with a subscription, the sign-in page's address, PIN and code.
func (d *daemon) writeState() {
	hold := "0"
	if d.dl.busy() || d.st.busy() {
		hold = "1"
	}
	kv := map[string]string{"hold": hold, "version": version}
	if d.authState() != authOK && d.signin != nil {
		d.signin.fields(kv)
	}
	d.state.Write(kv)
}

// Organize Files, .applemusic/organize: "on" (the default, and anything but "off") files each new
// download as Apple Music/<album artist>/<year> - <album>/07 - Title [id].m4a; "off" every one in
// Apple Music/ as "Title - Artist [id].m4a". A playlist's songs go in Playlists/<name>/ either way.
func readOrganize(dir string) bool {
	b, _ := os.ReadFile(filepath.Join(dir, "organize"))
	return strings.TrimSpace(string(b)) != "off"
}

func (d *daemon) organized() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.org
}

func (d *daemon) setOrganize(on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	if err := q2.WriteFile(filepath.Join(d.dir, "organize"), []byte(v+"\n")); err != nil {
		return err
	}
	d.mu.Lock()
	d.org = on
	d.mu.Unlock()
	return nil
}

// Stream mode, .applemusic/mode: "stream" plays a pressed song (stream.go); "download" (the
// default, and anything else) downloads it.
func readMode(dir string) bool {
	b, _ := os.ReadFile(filepath.Join(dir, "mode"))
	return strings.TrimSpace(string(b)) == "stream"
}

func (d *daemon) streaming() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.strm
}

func (d *daemon) setMode(stream bool) error {
	v := "download"
	if stream {
		v = "stream"
	}
	if err := q2.WriteFile(filepath.Join(d.dir, "mode"), []byte(v+"\n")); err != nil {
		return err
	}
	d.mu.Lock()
	d.strm = stream
	d.mu.Unlock()
	return nil
}

// signOut removes the token from the card; the downloads stay, and the queue waits for a sign-in.
func (d *daemon) signOut() error {
	err := os.Remove(filepath.Join(d.dir, tokenFile))
	if os.IsNotExist(err) {
		err = nil
	}
	d.client()
	d.writeState()
	return err
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// lookForCookies signs in from a cookies file on the card (cookies.go), one look at a time.
func (d *daemon) lookForCookies() {
	if d.importing.CompareAndSwap(false, true) {
		defer d.importing.Store(false)
		d.importCookies()
	}
}
