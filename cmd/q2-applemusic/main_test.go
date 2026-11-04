package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/apple"
	"github.com/group-a-study/q2-applemusic/internal/mp4"
	"github.com/group-a-study/q2-applemusic/internal/mp4/mp4test"
	"github.com/group-a-study/q2-applemusic/internal/q2"
	"github.com/group-a-study/q2-applemusic/internal/q2spot"
	"github.com/group-a-study/q2-applemusic/internal/widevine"
)

// fakeApple answers for music.apple.com, the AMP API and the playback service: a library of two
// albums (three songs) and a playlist, each song's stream encrypted as Apple's web AAC is, its key
// from a real Widevine license.
type fakeApple struct {
	*httptest.Server
	t      *testing.T
	mu     sync.Mutex
	asked  []string // the AMP and playback paths asked, in order
	active bool
	token  string         // the media-user-token it takes
	fail   map[string]int // songs the playback service refuses, how many more times
	plays  map[string]int // the playback service's answers, by song
}

const devToken = "eyJhbGciOiJFUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.c2ln"

var songKey = []byte("the-song-key-016")

// sample is song id's i-th sample, its bytes in the clear.
func sample(id string, i int) []byte { return bytes.Repeat([]byte(id[len(id)-1:]), 100+i) }

func plain(id string) []byte {
	var b []byte
	for i := 0; i < 50; i++ {
		b = append(b, sample(id, i)...)
	}
	return b
}

func stream(id string) []byte {
	var ss [][]byte
	for i := 0; i < 50; i++ {
		ss = append(ss, sample(id, i))
	}
	rate := uint16(44100)
	if id == "103" { // Berberian Sound Studio: a 48 kHz master, kept in 48khz/
		rate = 48000
	}
	return mp4test.FragmentedAt(rate, ss, [][]byte{prefetch, songKey},
		[][]byte{[]byte("\x00\x00\x00\x00\x00\x00\x00\x00s1/e1   "), bytes.Repeat([]byte{0xab}, 16)}, 20, 0, mp4test.ESDS)
}

var prefetch = []byte{0x32, 0xb8, 0xad, 0xe1, 0x76, 0x9e, 0x26, 0xb1, 0xff, 0xb8, 0x98, 0x63, 0x52, 0x79, 0x3f, 0xc6}

type song struct {
	id, name, artist, album string
	track, tracks           int
}

var songs = map[string]song{
	"101": {"101", "Tears in the Typing Pool", "Broadcast", "Tender Buttons", 1, 2},
	"102": {"102", "Valerie", "Broadcast", "Tender Buttons", 2, 2},
	"103": {"103", "Spell Blue", "Broadcast", "Berberian Sound Studio", 1, 1},
	"104": {"104", "Pendulum", "Broadcast", "Ha Ha Sound", 4, 12},
}

func res(typ, id string, attrs map[string]any) map[string]any {
	return map[string]any{"id": id, "type": typ, "attributes": attrs}
}

// artBase is where the fake's covers are.
var artBase string

func songRes(s song, library bool) map[string]any {
	return songIn(s, library, s.album, s.track)
}

// songIn is a song as an album lists it: a library compilation names itself and the song's place in it.
func songIn(s song, library bool, album string, track int) map[string]any {
	pp := map[string]any{"id": s.id, "kind": "song"}
	typ, id := "songs", s.id
	if library {
		pp = map[string]any{"id": "i." + s.id, "kind": "song", "isLibrary": true, "catalogId": s.id}
		typ, id = "library-songs", "i."+s.id
	}
	return res(typ, id, map[string]any{"name": s.name, "artistName": s.artist, "albumName": album,
		"trackNumber": track, "releaseDate": "2005-09-19", "playParams": pp,
		"artwork": map[string]any{"url": artBase + "/ART/song" + s.id + "/{w}x{h}bb.jpg"}})
}

// appleToken is a made-up media-user-token shaped as Apple's: "0.", then base64 with + / and =.
func appleToken(fill string) string {
	return "0.Ag" + strings.Repeat(fill, 200) + "+nA/UCN1vZ6y9rwRryGL7ooUJlZaUGPQed1qTfPLIyCg2k=="
}

func newFakeApple(t *testing.T) *fakeApple {
	f := &fakeApple{t: t, active: true, token: appleToken("x"), fail: map[string]int{}, plays: map[string]int{}}
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(mux)
	artBase = f.URL
	data := func(w http.ResponseWriter, items ...map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{"data": items})
	}
	signed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.asked = append(f.asked, r.Method+" "+r.URL.Path)
			tok := f.token
			f.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+devToken {
				w.WriteHeader(401)
				return
			}
			if r.Header.Get("Media-User-Token") != tok {
				w.WriteHeader(403)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/us/browse", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><script type="module" src="/assets/index~a1b2c3.js"></script></html>`)
	})
	mux.HandleFunc("/assets/index~a1b2c3.js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `var x="%s";`, devToken)
	})
	mux.HandleFunc("/v1/me/account", signed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		active := f.active
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"subscription": map[string]any{"active": active, "storefront": "au"}}})
	}))
	mux.HandleFunc("/v1/me/library/albums", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, res("library-albums", "l.a1", map[string]any{"name": "Tender Buttons", "artistName": "Broadcast"}),
			res("library-albums", "l.a2", map[string]any{"name": "Berberian Sound Studio", "artistName": "Broadcast"}))
	}))
	// Library albums with their catalogue album (include=catalog): Tender Buttons, and a compilation
	// whose songs Apple matched to their own albums (the playback service names those).
	withCatalog := func(id, name, artist, catName, date, art string) map[string]any {
		a := res("library-albums", id, map[string]any{"name": name, "artistName": artist})
		a["relationships"] = map[string]any{"catalog": map[string]any{"data": []any{res("albums", "9"+id, map[string]any{
			"name": catName, "artistName": artist, "releaseDate": date,
			"artwork": map[string]any{"url": f.URL + "/ART/" + art + "/{w}x{h}bb.jpg"}})}}}
		return a
	}
	mux.HandleFunc("/v1/me/library/albums/l.a1", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, withCatalog("l.a1", "Tender Buttons", "Broadcast", "Tender Buttons", "2005-09-19", "tb"))
	}))
	mux.HandleFunc("/v1/me/library/albums/l.fnl", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, withCatalog("l.fnl", "Friday Night Lights", "Various Artists",
			"Friday Night Lights (Original Television Soundtrack)", "2008-10-21", "fnl"))
	}))
	mux.HandleFunc("/v1/me/library/albums/l.fnl/tracks", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, songIn(songs["104"], true, "Friday Night Lights", 1), songIn(songs["102"], true, "Friday Night Lights", 2))
	}))
	mux.HandleFunc("/v1/me/library/albums/l.a1/tracks", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, songRes(songs["101"], true), songRes(songs["102"], true))
	}))
	mux.HandleFunc("/v1/me/library/albums/l.a2/tracks", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, songRes(songs["103"], true))
	}))
	mux.HandleFunc("/v1/me/library/playlists", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, res("library-playlists", "p.road", map[string]any{"name": "Road Trip"}))
	}))
	mux.HandleFunc("/v1/me/library/playlists/p.road", signed(func(w http.ResponseWriter, r *http.Request) {
		data(w, res("library-playlists", "p.road", map[string]any{"name": "Road Trip",
			"artwork": map[string]any{"url": f.URL + "/ART/road/{w}x{h}bb.jpg"}}))
	}))
	mux.HandleFunc("/v1/me/library/playlists/p.road/tracks", signed(func(w http.ResponseWriter, r *http.Request) {
		// One page of one song, then the next: next pages are followed.
		if r.URL.Query().Get("offset") == "" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{songRes(songs["104"], false)},
				"next": "/v1/me/library/playlists/p.road/tracks?offset=1"})
			return
		}
		data(w, songRes(songs["102"], true))
	}))
	mux.HandleFunc("/v1/catalog/au/songs/", signed(func(w http.ResponseWriter, r *http.Request) {
		ttml := `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p begin="1.5" end="3">First line</p>` +
			`<p begin="00:03.250" end="5">Second <span>line</span></p></div></body></tt>`
		data(w, res("lyrics", "1", map[string]any{"ttml": ttml}))
	}))
	mux.HandleFunc("/webPlayback", signed(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ SalableAdamId string }
		json.NewDecoder(r.Body).Decode(&body)
		s := songs[body.SalableAdamId]
		f.mu.Lock()
		f.plays[s.id]++
		refuse := f.fail[s.id] > 0
		if refuse {
			f.fail[s.id]--
		}
		f.mu.Unlock()
		if refuse {
			json.NewEncoder(w).Encode(map[string]any{"failureType": "9999", "customerMessage": "Something went wrong"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"songList": []any{map[string]any{"assets": []any{
			map[string]any{"flavor": "32:ctrp64", "URL": f.URL + "/hls/nope.m3u8"},
			map[string]any{"flavor": "28:ctrp256", "URL": f.URL + "/hls/" + s.id + ".m3u8", "metadata": map[string]any{
				"itemName": s.name, "artistName": s.artist, "playlistName": s.album, "playlistArtistName": s.artist,
				"trackNumber": s.track, "trackCount": s.tracks, "discNumber": 1, "discCount": 1,
				"releaseDate": "2005-09-19T07:00:00Z", "genre": "Electronic", "explicit": 0}}}}}})
	}))
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(r.URL.Path), ".m3u8"), ".mp4")
		if strings.HasSuffix(r.URL.Path, ".mp4") {
			http.ServeContent(w, r, "s.mp4", time.Time{}, bytes.NewReader(stream(id)))
			return
		}
		b := stream(id)
		kid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 16))
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"%s.mp4\",BYTERANGE=\"%d@0\"\n", id, 600)
		fmt.Fprintf(w, "#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,KEYFORMAT=\"urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed\",KEYFORMATVERSIONS=\"1\",URI=\"%s\"\n",
			"data:text/plain;base64,AAAAOHBzc2gAAAAA7e+LqXnWSs6jyCfc1R0h7QAAABgSEAAAAAAAAAAAczEvZTEgICBI88aJmwY=")
		fmt.Fprintf(w, "#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,KEYFORMAT=\"urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed\",KEYFORMATVERSIONS=\"1\",URI=\"data:text/plain;base64,%s\"\n", kid)
		fmt.Fprintf(w, "#EXTINF:5,\n#EXT-X-BYTERANGE:%d@600\n%s.mp4\n#EXT-X-ENDLIST\n", len(b)-600, id)
	})
	mux.HandleFunc("/acquireWebPlaybackLicense", signed(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		ch, _ := base64.StdEncoding.DecodeString(body["challenge"].(string))
		lic, err := widevine.License(widevine.Builtin(), ch, []widevine.Key{{ID: bytes.Repeat([]byte{0xab}, 16), Key: songKey}})
		if err != nil {
			t.Errorf("license: %v", err)
			json.NewEncoder(w).Encode(map[string]any{"status": -1})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": 0, "license": base64.StdEncoding.EncodeToString(lic)})
	}))
	mux.HandleFunc("/ART/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "\xff\xd8\xff"+r.URL.Path)
	})
	t.Cleanup(f.Close)
	return f
}

// rig is a daemon on a card in a temp folder, signed in, talking to f.
func rig(t *testing.T, f *fakeApple) *daemon {
	t.Helper()
	return rigWith(t, f, nil)
}

// rigWith is rig with the card readied by before (given its mount point) first.
func rigWith(t *testing.T, f *fakeApple, before func(card string)) *daemon {
	t.Helper()
	apple.HomeURL, apple.AMPURL, apple.PlayURL = f.URL, f.URL, f.URL
	transport = func() http.RoundTripper { return http.DefaultTransport }
	cardReady = func() bool { return true }
	gap, retryIn, answerIn, retryGap = 0, 50*time.Millisecond, 10*time.Second, 0
	signinPort = 0
	card := t.TempDir()
	q2.Card = card
	dir := filepath.Join(card, ".applemusic")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, tokenFile), []byte(f.token+"\n"), 0o600)
	if before != nil {
		before(card)
	}
	tmp := t.TempDir()
	d := start(dir, filepath.Join(card, "Apple Music"), filepath.Join(tmp, "q2-applemusic.state"),
		filepath.Join(tmp, "browse.sock"), filepath.Join(tmp, "ctl.sock"))
	done := make(chan struct{})
	go func() { d.run(); close(done) }()
	t.Cleanup(func() {
		d.shutdown()
		<-done
		for d.deduping.Load() { // its look for Deezer duplicates, which reads cardReady, done too
			time.Sleep(5 * time.Millisecond)
		}
	})
	until(t, "the sign-in", func() bool { return d.authState() == authOK })
	return d
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func ask(d *daemon, op string, args ...string) q2spot.Reply {
	return d.device(q2spot.Request{ID: "1", Op: op, Args: args})
}

func titles(r q2spot.Reply) []string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, row.Title)
	}
	return out
}

// tags is the M4A's tags (mp4.ReadTags) and its samples, checked to be song id's in the clear.
func song4a(t *testing.T, path, id string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(b, plain(id)) {
		t.Errorf("%s: the samples aren't %s's in the clear", filepath.Base(path), id)
	}
	return mp4.ReadTags(b)
}

func TestDownloadAllAlbums(t *testing.T) {
	f := newFakeApple(t)
	d := rig(t, f)
	r := ask(d, "albums")
	if got := titles(r); len(got) != 3 || got[0] != "Download All Albums" || r.Rows[0].Subtitle != "All 2 albums in your library" {
		t.Fatalf("albums: %q %q", got, r.Rows[0].Subtitle)
	}
	if r := ask(d, "bulk", "albums"); r.Message != "Downloading all 2 albums" {
		t.Fatalf("bulk: %+v", r)
	}
	root := filepath.Join(q2.Card, "Apple Music", "Broadcast")
	a := filepath.Join(root, "2005 - Tender Buttons", "02 - Valerie [102].m4a")
	b := filepath.Join(q2.Card, "Apple Music", "48khz", "Broadcast", "2005 - Berberian Sound Studio", "01 - Spell Blue [103].m4a")
	until(t, "both albums", func() bool {
		_, e1 := os.Stat(a)
		_, e2 := os.Stat(b)
		return e1 == nil && e2 == nil && !d.dl.pending()
	})
	tg := song4a(t, a, "102")
	for k, v := range map[string]string{"\xa9nam": "Valerie", "\xa9alb": "Tender Buttons", "aART": "Broadcast",
		"trkn": "2/2", "\xa9day": "2005-09-19", "\xa9lyr": "[00:01.50]First line\n[00:03.25]Second line", "cnID": "102"} {
		if tg[k] != v {
			t.Errorf("tag %q = %q, want %q", k, tg[k], v)
		}
	}
	if want := len("\xff\xd8\xff/ART/song102/600x600bb.jpg"); tg["covr"] != fmt.Sprint(want, " bytes") {
		t.Errorf("cover %q", tg["covr"])
	}
	// One album's songs were asked for only once the one before was done.
	f.mu.Lock()
	asked := strings.Join(f.asked, "\n")
	f.mu.Unlock()
	if i, j := strings.Index(asked, "l.a2/tracks"), strings.LastIndex(asked, "POST /webPlayback"); i < 0 ||
		strings.Count(asked[:i], "POST /webPlayback") != 2 || j < i {
		t.Errorf("albums not one at a time:\n%s", asked)
	}
	if _, err := os.Stat(filepath.Join(d.dir, "bulk.json")); !os.IsNotExist(err) {
		t.Error("bulk.json left behind")
	}
	if r := ask(d, "tracks", "album:l.a1"); r.Rows[0].Title != "All on the card" || r.Rows[1].Kind != "local" {
		t.Errorf("album page: %+v", r.Rows)
	}
	// The 48 kHz album is kept apart, and only there; Downloaded Songs browses it the same way.
	if _, err := os.Stat(filepath.Join(root, "2005 - Berberian Sound Studio")); !os.IsNotExist(err) {
		t.Error("the 48 kHz album's folder was also made in the usual place")
	}
	if got := titles(ask(d, "cardartists")); strings.Join(got, "|") != "Broadcast|48 kHz and Higher" {
		t.Errorf("Downloaded Songs %q", got)
	}
	if r := ask(d, "cardartists", "48khz"); len(r.Rows) != 1 || r.Rows[0].Title != "Broadcast" || r.Rows[0].Context != "48khz/Broadcast" {
		t.Errorf("48 kHz and Higher %+v", r.Rows)
	}
	if r := ask(d, "cardalbums", "48khz/Broadcast"); len(r.Rows) != 1 || r.Rows[0].Title != "Berberian Sound Studio" ||
		r.Rows[0].URI != filepath.Dir(b)+"/" {
		t.Errorf("48 kHz artist %+v", r.Rows)
	}
	if r := ask(d, "cardalbums", "48khz/../x"); r.Status != q2spot.Bad {
		t.Errorf("a path out of 48khz/ taken: %+v", r)
	}
	if tg := song4a(t, b, "103"); tg["\xa9alb"] != "Berberian Sound Studio" || tg["aART"] != "Broadcast" {
		t.Errorf("48 kHz song tags %q", tg)
	}
}

func TestPlaylistAsAlbum(t *testing.T) {
	f := newFakeApple(t)
	d := rig(t, f)
	r := ask(d, "playlists")
	if got := titles(r); len(got) != 3 || got[1] != "Download All Playlists" {
		t.Fatalf("playlists: %q", got)
	}
	r = ask(d, "tracks", "playlist:p.road")
	if got := titles(r); len(got) != 3 || got[0] != "Download All" || got[1] != "Pendulum" || got[2] != "Valerie" ||
		!strings.Contains(r.Rows[0].Subtitle, "kept as an album") {
		t.Fatalf("playlist page: %q %q", got, r.Rows[0].Subtitle)
	}
	// Valerie on its album already: the playlist's album still gets its own copy.
	if r := ask(d, "download", "album:l.a1#102"); r.Message != "Downloading" {
		t.Fatalf("download: %+v", r)
	}
	until(t, "the album's song", func() bool { return !d.dl.pending() && d.dl.count() == 1 })
	if r := ask(d, "bulk", "playlists"); r.Message != "Downloading 1 playlist" {
		t.Fatalf("bulk: %+v", r)
	}
	dir := filepath.Join(q2.Card, "Apple Music", "Playlists", "Road Trip")
	until(t, "the playlist", func() bool { return !d.dl.pending() && d.dl.count() == 3 })
	one := song4a(t, filepath.Join(dir, "01 - Pendulum [104].m4a"), "104")
	two := song4a(t, filepath.Join(dir, "02 - Valerie [102].m4a"), "102")
	for _, c := range []struct {
		tags      map[string]string
		trkn, art string
	}{{one, "1/2", "song104"}, {two, "2/2", "song102"}} {
		if c.tags["\xa9alb"] != "Road Trip" || c.tags["aART"] != "Playlists" || c.tags["trkn"] != c.trkn || c.tags["cpil"] != "1" {
			t.Errorf("playlist song tags %q", c.tags)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cover.jpg")); string(b) != "\xff\xd8\xff/ART/road/600x600bb.jpg" {
		t.Errorf("cover.jpg %q", b)
	}
	// Each song keeps its own cover.
	b, _ := os.ReadFile(filepath.Join(dir, "01 - Pendulum [104].m4a"))
	if !bytes.Contains(b, []byte("/ART/song104/600x600bb.jpg")) {
		t.Error("the song's own cover isn't embedded")
	}
	if id, name := readMarker(dir); id != "p.road" || name != "Road Trip" {
		t.Errorf("marker %q %q", id, name)
	}
	// The playlist's page and a fresh scan see its songs on the card, apart from the album's.
	d.dl.rescan()
	if r := ask(d, "tracks", "playlist:p.road"); r.Rows[0].Title != "All on the card" || r.Rows[0].URI != dir+"/" {
		t.Errorf("playlist page after: %+v", r.Rows[0])
	}
	if r := ask(d, "cardalbums", "Playlists"); len(r.Rows) != 1 || r.Rows[0].Title != "Road Trip" || r.Rows[0].Context != "album" {
		t.Errorf("Downloaded Songs → Playlists: %+v", r.Rows)
	}
}

func TestBulkStop(t *testing.T) {
	f := newFakeApple(t)
	d := rig(t, f)
	ask(d, "albums")
	until(t, "the start's look at the card", func() bool { return d.dl.scannedYet() && !d.deduping.Load() })
	cardReady = func() bool { return false } // the card away: Download All waits, its lists kept
	ask(d, "bulk", "albums")
	until(t, "the queue to wait", func() bool { _, _, _, _, why := d.dl.snapshot(); return why == "The card is away" })
	if left, total, _ := d.dl.bulkState(); left != 2 || total != 2 {
		t.Fatalf("bulk state %d of %d", left, total)
	}
	if r := ask(d, "downloads"); !strings.Contains(strings.Join(titles(r), "|"), "Stop Download All") {
		t.Fatalf("downloads page %q", titles(r))
	}
	if r := ask(d, "bulkstop"); !strings.HasPrefix(r.Message, "2 lists left out") {
		t.Fatalf("bulkstop %+v", r)
	}
	if left, _, _ := d.dl.bulkState(); left != 0 || d.dl.pending() {
		t.Fatal("still pending after Stop Download All")
	}
}

func TestSignIn(t *testing.T) {
	f := newFakeApple(t)
	d := rig(t, f)
	if got := titles(d.menu()); len(got) < 3 || got[1] != "Recently Added" {
		t.Fatalf("menu %q", got)
	}
	// A token Apple refuses: the menu asks for a new sign-in, and the queue waits.
	f.mu.Lock()
	f.token = "another"
	f.mu.Unlock()
	os.WriteFile(filepath.Join(d.dir, tokenFile), []byte(appleToken("y")), 0o600)
	d.checkAuth()
	if d.authState() != authExpired {
		t.Fatalf("auth %d", d.authState())
	}
	if got := titles(d.menu()); got[0] != "Sign In Again from Your Phone" {
		t.Fatalf("menu %q", got)
	}
	if r := ask(d, "albums"); r.Status != q2spot.Login {
		t.Fatalf("albums while signed out: %+v", r)
	}
	kv := map[string]string{}
	d.signin.fields(kv)
	if kv["pin"] == "" || kv["note"] == "" {
		t.Fatalf("QR page fields %q", kv)
	}
	tok := appleToken("z")
	for _, pasted := range []string{tok, "  " + tok + "\n",
		"# Netscape\n.music.apple.com\tTRUE\t/\tTRUE\t0\tmedia-user-token\t" + tok + "\n",
		"foo=1; media-user-token=" + tok + "; bar=2"} {
		if got := pastedToken(pasted); got != tok || !validToken(got) {
			t.Fatalf("token from %q: %q", pasted, got)
		}
	}
	if validToken("0.short") || validToken(tok+" more") {
		t.Fatal("a malformed token taken")
	}
}

func TestNames(t *testing.T) {
	if n := songName("1", `AC/DC: "Live"?`, 7, 12, 2, 2); n != "2-07 - AC_DC_ _Live__ [1]" {
		t.Errorf("songName %q", n)
	}
	if n := albumFolderName("Tender Buttons.", "2005", "Unknown"); n != "2005 - Tender Buttons" {
		t.Errorf("albumFolderName %q", n)
	}
	if n := flatName("9", "Song", "Artist"); n != "Song - Artist [9]" {
		t.Errorf("flatName %q", n)
	}
	if !named.MatchString("01 - Song [123].m4a") || named.MatchString("cover.jpg") {
		t.Error("named")
	}
	if !leftover.MatchString("01 - Song [1].m4a.enc.part") || !leftover.MatchString("x.m4a.part") {
		t.Error("leftover")
	}
}

func TestRetries(t *testing.T) {
	f := newFakeApple(t)
	d := rig(t, f)
	f.mu.Lock()
	f.fail["101"], f.fail["103"] = 2, 99
	f.mu.Unlock()
	ask(d, "tracks", "album:l.a1")
	ask(d, "tracks", "album:l.a2")
	ask(d, "download", "album:l.a1")
	ask(d, "download", "album:l.a2")
	until(t, "the queue", func() bool { return !d.dl.pending() })
	f.mu.Lock()
	plays := fmt.Sprint(f.plays)
	f.mu.Unlock()
	// Tears in the Typing Pool on its third try; Spell Blue tried four times, then left; Valerie between.
	if plays != "map[101:3 102:1 103:4]" {
		t.Fatalf("playback asked %s", plays)
	}
	if d.dl.count() != 2 {
		t.Fatalf("%d songs on the card", d.dl.count())
	}
	list := filepath.Join(q2.Card, "Apple Music", notDownloaded)
	b, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	row := strings.Split(lines[len(lines)-1], "\t")
	if lines[3] != "Artist\tAlbum\tTrack\tPlaylist\tReason\tLink\tWhen" || len(row) != 7 ||
		strings.Join(row[:6], "|") != "Broadcast|Berberian Sound Studio|Spell Blue||Apple: Something went wrong|https://music.apple.com/au/song/103" ||
		len(row[6]) != len("2026-11-04 14:03") {
		t.Fatalf("Not Downloaded.txt:\n%s", b)
	}
	if r := ask(d, "downloads"); !strings.Contains(strings.Join(titles(r), "|"), "Spell Blue") {
		t.Fatalf("downloads %q", titles(r))
	}
	// Kept across a restart.
	if again := newDownloads(d, d.dl.folder, d.dl.file, d.dl.bulkFile); len(again.failed) != 1 || again.failed[0].Track.ID != "103" {
		t.Fatalf("failed after a restart: %+v", again.failed)
	}
	// Tried again with Apple answering: it downloads, and leaves the list (the file goes with the last).
	f.mu.Lock()
	f.fail["103"] = 0
	f.mu.Unlock()
	if r := ask(d, "retry", "103"); r.Message != "Trying again" {
		t.Fatalf("retry %+v", r)
	}
	until(t, "the retried song", func() bool { return !d.dl.pending() && d.dl.count() == 3 })
	if _, err := os.Stat(list); !os.IsNotExist(err) {
		t.Error("Not Downloaded.txt still there")
	}
	if _, err := os.Stat(d.dl.failedFile); !os.IsNotExist(err) {
		t.Error("failed.json still there")
	}
}

// TestSignInPage signs in through the phone's page: the PIN, then a token pasted as Apple's look.
func TestSignInPage(t *testing.T) {
	f := newFakeApple(t)
	d := rig(t, f)
	os.Remove(filepath.Join(d.dir, tokenFile))
	d.signOut()
	page := "http://127.0.0.1:" + fmt.Sprint(d.signin.port)
	jar := &cookieJar{}
	c := &http.Client{Jar: jar}
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, page+path, strings.NewReader(body))
		req.Header.Set("X-Q2", "1")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, fmt.Sprint(out["error"])
	}
	if code, _ := post("/api/login", `{"pin":"`+d.signin.pin+`"}`); code != 200 {
		t.Fatalf("PIN refused: %d", code)
	}
	if code, msg := post("/api/token", `{"token":"0.short"}`); code != 400 {
		t.Fatalf("a short token: %d %s", code, msg)
	}
	if code, msg := post("/api/token", `{"token":"`+f.token+`"}`); code != 200 {
		t.Fatalf("Apple's token refused: %d %s", code, msg)
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, tokenFile)); strings.TrimSpace(string(b)) != f.token {
		t.Fatal("the token isn't on the card")
	}
	until(t, "the sign-in", func() bool { return d.authState() == authOK })
}

// cookieJar keeps the sign-in page's cookie.
type cookieJar struct {
	mu sync.Mutex
	cs []*http.Cookie
}

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	j.cs = append(j.cs, cs...)
	j.mu.Unlock()
}

func (j *cookieJar) Cookies(*url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cs
}

// TestCookiesFile signs in from a cookies file put on the card.
func TestCookiesFile(t *testing.T) {
	f := newFakeApple(t)
	d := rig(t, f)
	d.signOut()
	until(t, "signed out", func() bool { return d.authState() == authNoToken })
	if got := titles(d.menu()); got[1] != "Or put cookies.txt on the card" {
		t.Fatalf("menu %q", got)
	}
	line := func(tok string) string {
		return "# Netscape HTTP Cookie File\n.apple.com\tTRUE\t/\tTRUE\t0\tmyacinfo\tsecret\n" +
			"#HttpOnly_.music.apple.com\tTRUE\t/\tTRUE\t1800000000\tmedia-user-token\t" + tok + "\n"
	}
	accounts := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Count(strings.Join(f.asked, "\n"), "/v1/me/account")
	}
	// One Apple refuses: it stays, the menu says so, and it isn't asked about again.
	bad := filepath.Join(q2.Card, "cookies.txt")
	os.WriteFile(bad, []byte(line(appleToken("q"))), 0o644)
	d.lookForCookies()
	if got := titles(d.menu()); got[1] != "The cookies file didn't sign in" || d.authState() != authNoToken {
		t.Fatalf("menu %q, auth %d", got, d.authState())
	}
	n := accounts()
	d.lookForCookies()
	if accounts() != n {
		t.Fatal("a refused cookies file was asked about again")
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatal("the refused cookies file was deleted")
	}
	// A good one, in Apple Music/: signed in, the token saved, the file gone.
	os.MkdirAll(d.dl.folder, 0o755)
	good := filepath.Join(d.dl.folder, "cookies-music-apple-com.txt")
	os.WriteFile(good, []byte(line(f.token)), 0o644)
	d.lookForCookies()
	until(t, "the sign-in", func() bool { return d.authState() == authOK })
	if d.token() != f.token {
		t.Fatal("the token isn't saved")
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatal("the cookies file is still on the card")
	}
	if cookieToken([]byte("media-user-token="+f.token)) != f.token || cookieToken([]byte(line("0.short"))) != "" {
		t.Fatal("cookieToken")
	}
}

// TestStartsBeforeTheCardIsRead: the page answers while the card is still being read (a card of
// thousands of songs takes longer than the ten seconds Q2 Pod gives a starting app), and nothing
// downloads until it is read, so a song already there isn't fetched again.
func TestStartsBeforeTheCardIsRead(t *testing.T) {
	f := newFakeApple(t)
	hold := make(chan struct{})
	scanHook = func() { <-hold }
	defer func() { scanHook = func() {} }()
	d := rigWith(t, f, func(card string) {
		dir := filepath.Join(card, "Apple Music", "Broadcast", "2005 - Tender Buttons")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "02 - Valerie [102].m4a"), []byte("on the card"), 0o644)
		os.WriteFile(filepath.Join(dir, "01 - Tears in the Typing Pool [101].m4a.part"), []byte("left"), 0o644)
	})
	if got := ask(d, "menu"); got.Rows[0].Subtitle != "Reading the card…" {
		t.Fatalf("menu while reading: %+v", got.Rows[0])
	}
	ask(d, "tracks", "album:l.a1")
	if r := ask(d, "download", "album:l.a1"); r.Message != "Downloading 2 songs" {
		t.Fatalf("download: %+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	f.mu.Lock()
	early := len(f.plays)
	f.mu.Unlock()
	if early != 0 {
		t.Fatal("downloads started before the card was read")
	}
	close(hold)
	until(t, "the queue", func() bool { return d.dl.scannedYet() && !d.dl.pending() })
	f.mu.Lock()
	plays := fmt.Sprint(f.plays)
	f.mu.Unlock()
	if plays != "map[101:1]" {
		t.Fatalf("playback asked %s: Valerie was on the card", plays)
	}
	if _, err := os.Stat(filepath.Join(d.dl.folder, "Broadcast", "2005 - Tender Buttons", "01 - Tears in the Typing Pool [101].m4a.part")); !os.IsNotExist(err) {
		t.Fatal("the leftover .part wasn't swept")
	}
}

// TestCompilation: a library compilation whose songs Apple matched to their own releases is one album,
// Various Artists', not each song's own; an earlier copy alone in its own album's folder goes, one in a
// fuller album stays, and an album whose songs are its own is filed as before.
func TestCompilation(t *testing.T) {
	f := newFakeApple(t)
	d := rigWith(t, f, func(card string) {
		dir := filepath.Join(card, "Apple Music", "Broadcast", "2009 - Ha Ha Sound") // as v1.6 filed it
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "04 - Pendulum [104].m4a"), []byte("stray"), 0o644)
	})
	until(t, "the card read", d.dl.scannedYet)
	ask(d, "tracks", "album:l.a1")
	ask(d, "download", "album:l.a1")
	until(t, "Tender Buttons", func() bool { return !d.dl.pending() && d.dl.count() == 3 })
	tb := filepath.Join(d.dl.folder, "Broadcast", "2005 - Tender Buttons")
	if _, err := os.Stat(filepath.Join(tb, "02 - Valerie [102].m4a")); err != nil {
		t.Fatal("an album of its own songs isn't filed as before")
	}
	r := ask(d, "tracks", "album:l.fnl")
	if got := titles(r); len(got) != 3 || got[1] != "Pendulum" || !strings.Contains(r.Rows[2].Subtitle, "") {
		t.Fatalf("compilation page %q", got)
	}
	if r.Rows[1].Kind == "local" || r.Rows[2].Kind == "local" {
		t.Fatalf("the compilation's songs count as on the card from other albums: %+v", r.Rows)
	}
	ask(d, "download", "album:l.fnl")
	until(t, "the compilation", func() bool { return !d.dl.pending() && d.dl.count() >= 4 })
	fnl := filepath.Join(d.dl.folder, "Various Artists", "2008 - Friday Night Lights")
	for _, c := range []struct{ file, id, trkn string }{{"01 - Pendulum [104].m4a", "104", "1/2"}, {"02 - Valerie [102].m4a", "102", "2/2"}} {
		tg := song4a(t, filepath.Join(fnl, c.file), c.id)
		if tg["\xa9alb"] != "Friday Night Lights" || tg["aART"] != "Various Artists" || tg["trkn"] != c.trkn ||
			tg["cpil"] != "1" || tg["\xa9day"] != "2008" || tg["\xa9ART"] != "Broadcast" {
			t.Errorf("%s tags %q", c.file, tg)
		}
	}
	b, _ := os.ReadFile(filepath.Join(fnl, "01 - Pendulum [104].m4a"))
	if !bytes.Contains(b, []byte("/ART/fnl/600x600bb.jpg")) {
		t.Error("the compilation's cover isn't embedded")
	}
	if _, err := os.Stat(filepath.Join(d.dl.folder, "Broadcast", "2009 - Ha Ha Sound")); !os.IsNotExist(err) {
		t.Error("the stray copy's folder is still there")
	}
	if _, err := os.Stat(filepath.Join(tb, "02 - Valerie [102].m4a")); err != nil {
		t.Error("a copy in a fuller album was removed")
	}
	if r := ask(d, "tracks", "album:l.fnl"); r.Rows[0].Title != "All on the card" || r.Rows[0].URI != fnl+"/" {
		t.Errorf("compilation page after %+v", r.Rows[0])
	}
	if r := ask(d, "tracks", "album:l.a1"); r.Rows[0].Title != "All on the card" || r.Rows[0].URI != tb+"/" {
		t.Errorf("Tender Buttons page after %+v", r.Rows[0])
	}
}

// TestDedupe: Deezer's copy of an album Apple Music/ has whole is kept out of the library (.nomedia);
// one only Deezer has, or that Apple Music/ has fewer songs of, stays; a mark the app wrote that no
// longer holds goes, and one put there by hand is left.
func TestDedupe(t *testing.T) {
	f := newFakeApple(t)
	var dz string
	d := rigWith(t, f, func(card string) {
		dz = filepath.Join(card, "Deezer", "Broadcast")
		album := func(name string, songs int, mark string) {
			os.MkdirAll(filepath.Join(dz, name), 0o755)
			for i := 1; i <= songs; i++ {
				os.WriteFile(filepath.Join(dz, name, fmt.Sprintf("%02d - Song [%d].flac", i, 900+i)), []byte("flac"), 0o644)
			}
			if mark != "" {
				os.WriteFile(filepath.Join(dz, name, nomedia), []byte(mark), 0o644)
			}
		}
		album("2005 - Tender Buttons", 2, "")
		album("2009 - Ha Ha Sound", 1, "")
		album("2012 - Berberian Sound Studio", 3, "")
		album("Old Album (2001)", 1, ourMark)
		album("Kept By Hand", 1, "mine")
	})
	ask(d, "albums")
	ask(d, "bulk", "albums")
	until(t, "the downloads and the look for duplicates", func() bool {
		_, err := os.Stat(filepath.Join(dz, "2005 - Tender Buttons", nomedia))
		return !d.dl.pending() && err == nil && !d.deduping.Load()
	})
	mark := func(album string) string {
		b, err := os.ReadFile(filepath.Join(dz, album, nomedia))
		if err != nil {
			return "none"
		}
		return string(b)
	}
	for album, want := range map[string]string{"2005 - Tender Buttons": ourMark, "2009 - Ha Ha Sound": "none",
		"2012 - Berberian Sound Studio": "none", "Old Album (2001)": "none", "Kept By Hand": "mine"} {
		if got := mark(album); got != want {
			t.Errorf("%s: .nomedia %q, want %q", album, got, want)
		}
	}
	if d.deduped.Load() != 1 || !strings.Contains(strings.Join(titles(ask(d, "cardartists")), "|"), "1 Deezer album kept out of the library") {
		t.Errorf("deduped %d, Downloaded Songs %q", d.deduped.Load(), titles(ask(d, "cardartists")))
	}
	// Apple Music's copy gone: Deezer's is in the library again.
	os.RemoveAll(filepath.Join(d.dl.folder, "Broadcast", "2005 - Tender Buttons"))
	if n := d.dedupe(); n != 0 || mark("2005 - Tender Buttons") != "none" {
		t.Errorf("after Apple Music's copy went: %d kept out, mark %q", n, mark("2005 - Tender Buttons"))
	}
	if albumKey(albumTitle("2005 - Greatest Hits Vol. 2")) == albumKey(albumTitle("Greatest Hits (2001)")) {
		t.Error("two albums taken for one")
	}
}
