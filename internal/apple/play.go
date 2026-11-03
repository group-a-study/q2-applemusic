package apple

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/mp4"
	"github.com/group-a-study/q2-applemusic/internal/widevine"
)

// flavor is the web player's AAC 256 kbps, 44.1 kHz stream, encrypted with CENC for Widevine
// (gamdl's aac-web).
const flavor = "28:ctrp256"

// prefetchKey decrypts the first seconds of every web stream (its first sample description), which
// Apple encrypts with this fixed key (gamdl's DEFAULT_SONG_DECRYPTION_KEY), so playback can start
// before the license comes.
var prefetchKey = []byte{0x32, 0xb8, 0xad, 0xe1, 0x76, 0x9e, 0x26, 0xb1, 0xff, 0xb8, 0x98, 0x63, 0x52, 0x79, 0x3f, 0xc6}

// defaultKeyURI is the Widevine key every playlist lists first, the prefetch key's: never the
// song's (gamdl's DRM_DEFAULT_KEY_MAPPING).
const defaultKeyURI = "data:text/plain;base64,AAAAOHBzc2gAAAAA7e+LqXnWSs6jyCfc1R0h7QAAABgSEAAAAAAAAAAAczEvZTEgICBI88aJmwY="

const widevineFormat = "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"

// Meta is a song's tags as the playback service gives them (its asset's metadata).
type Meta struct {
	Title, Artist, Album, AlbumArtist, Genre, Composer, Copyright, Date string
	Track, Tracks, Disc, Discs                                          int
	Compilation, Explicit, Clean                                        bool
}

// Song is a song ready to download: its tags and its stream's playlist.
type Song struct {
	ID     string
	Meta   Meta
	stream string
}

// Prepare asks the playback service for a song: its AAC stream and its tags.
func (c *Client) Prepare(ctx context.Context, id string) (*Song, error) {
	var r struct {
		SongList []struct {
			Assets []struct {
				Flavor   string         `json:"flavor"`
				URL      string         `json:"URL"`
				Metadata map[string]any `json:"metadata"`
			} `json:"assets"`
		} `json:"songList"`
		Dialog *struct {
			Message     string `json:"message"`
			Explanation string `json:"explanation"`
		} `json:"dialog"`
		FailureType     string `json:"failureType"`
		CustomerMessage string `json:"customerMessage"`
	}
	body := map[string]string{"salableAdamId": id, "language": c.Language}
	if err := c.call(ctx, http.MethodPost, PlayURL+"/webPlayback", body, &r); err != nil {
		return nil, err
	}
	switch {
	case r.Dialog != nil:
		return nil, fmt.Errorf("Apple: %s", firstOf(r.Dialog.Message, r.Dialog.Explanation, "it won't play this song"))
	case r.FailureType != "":
		if r.FailureType == "3076" || strings.Contains(strings.ToLower(r.CustomerMessage), "subscri") {
			return nil, ErrNoSubscription
		}
		return nil, fmt.Errorf("Apple: %s", firstOf(r.CustomerMessage, "it won't play this song ("+r.FailureType+")"))
	case len(r.SongList) == 0:
		return nil, ErrUnavailable
	}
	s := &Song{ID: id}
	for _, a := range r.SongList[0].Assets {
		if a.Flavor == flavor {
			s.stream = a.URL
			s.Meta = meta(a.Metadata)
		}
	}
	if s.stream == "" {
		return nil, ErrUnavailable
	}
	return s, nil
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// meta reads an asset's metadata: names, numbers (JSON numbers or strings), flags.
func meta(m map[string]any) Meta {
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	num := func(k string) int {
		n, _ := strconv.Atoi(str(k))
		return n
	}
	flag := func(k string) bool {
		switch v := m[k].(type) {
		case bool:
			return v
		case float64:
			return v != 0
		case string:
			return v == "true" || v == "1"
		}
		return false
	}
	date := str("releaseDate")
	if i := strings.IndexByte(date, 'T'); i > 0 {
		date = date[:i]
	}
	md := Meta{Title: str("itemName"), Artist: str("artistName"), Album: str("playlistName"),
		AlbumArtist: str("playlistArtistName"), Genre: str("genre"), Composer: str("composerName"),
		Copyright: str("copyright"), Date: date, Track: num("trackNumber"), Tracks: num("trackCount"),
		Disc: num("discNumber"), Discs: num("discCount"), Compilation: flag("compilation")}
	switch num("explicit") {
	case 1:
		md.Explicit = true
	case 2:
		md.Clean = true
	}
	if md.AlbumArtist == "" {
		md.AlbumArtist = md.Artist
	}
	return md
}

// Year is the song's release year, "" without one.
func (m Meta) Year() string { return year(m.Date) }

// hlsKey is a playlist's EXT-X-KEY.
type hlsKey struct{ format, uri string }

// piece is a range of a file a playlist names: from at, n bytes (n < 0: to its end).
type piece struct {
	uri   string
	at, n int64
}

// playlist reads an HLS media playlist: its keys, and its init section and segments in order.
func playlist(base string, text []byte) ([]hlsKey, []piece, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return nil, nil, err
	}
	abs := func(u string) string {
		r, err := bu.Parse(u)
		if err != nil {
			return u
		}
		return r.String()
	}
	var keys []hlsKey
	var ps []piece
	var next int64 = -1 // where a BYTERANGE without @ starts: after the last one
	rng := func(s string) (int64, int64) {
		n, at := s, ""
		if i := strings.IndexByte(s, '@'); i >= 0 {
			n, at = s[:i], s[i+1:]
		}
		ln, _ := strconv.ParseInt(n, 10, 64)
		off := next
		if at != "" {
			off, _ = strconv.ParseInt(at, 10, 64)
		}
		if off < 0 {
			off = 0
		}
		next = off + ln
		return off, ln
	}
	pending := piece{n: -1}
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			a := attrs(line[len("#EXT-X-KEY:"):])
			keys = append(keys, hlsKey{format: a["KEYFORMAT"], uri: a["URI"]})
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			a := attrs(line[len("#EXT-X-MAP:"):])
			p := piece{uri: abs(a["URI"]), n: -1}
			if br := a["BYTERANGE"]; br != "" {
				p.at, p.n = rng(br)
			}
			ps = append(ps, p)
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			pending.at, pending.n = rng(line[len("#EXT-X-BYTERANGE:"):])
		case strings.HasPrefix(line, "#"):
		default:
			pending.uri = abs(line)
			ps = append(ps, pending)
			pending = piece{n: -1}
		}
	}
	if len(ps) == 0 {
		return nil, nil, errors.New("Apple's stream playlist names no segments")
	}
	return keys, ps, nil
}

// attrs reads an HLS attribute list: KEY=value, KEY="quoted, value".
func attrs(s string) map[string]string {
	m := map[string]string{}
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		k := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var v string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				v, s = s[1:], ""
			} else {
				v, s = s[1:end+1], s[end+2:]
			}
		} else if c := strings.IndexByte(s, ','); c >= 0 {
			v, s = s[:c], s[c:]
		} else {
			v, s = s, ""
		}
		m[k] = v
		s = strings.TrimPrefix(strings.TrimSpace(s), ",")
	}
	return m
}

// songKey is the playlist's Widevine key for the song: not the default (prefetch) one; a key with
// no format, as some playlists list it, will do (gamdl's untyped fallback).
func songKey(keys []hlsKey) string {
	for _, k := range keys {
		if k.format == widevineFormat && k.uri != defaultKeyURI {
			return k.uri
		}
	}
	for _, k := range keys {
		if k.format == "" && k.uri != defaultKeyURI && strings.HasPrefix(k.uri, "data:") {
			return k.uri
		}
	}
	return ""
}

// merge joins pieces that follow on in one file, so a file in byte ranges is one request.
func merge(ps []piece) []piece {
	var out []piece
	for _, p := range ps {
		if n := len(out); n > 0 {
			l := &out[n-1]
			if l.uri == p.uri && l.n >= 0 && p.n >= 0 && l.at+l.n == p.at {
				l.n += p.n
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// keys is the license's content keys for the stream's sample descriptions: each by its key id; one
// that names none of them, the prefetch key for the first of two and the song's for the rest, as
// gamdl does.
func (c *Client) keys(ctx context.Context, id, keyURI string, kids [][]byte) (map[int][]byte, error) {
	init, err := widevine.InitData(keyURI)
	if err != nil {
		return nil, err
	}
	s := c.dev.NewSession()
	ch, err := s.Challenge(init)
	if err != nil {
		return nil, err
	}
	var r struct {
		Status  int    `json:"status"`
		License string `json:"license"`
	}
	body := map[string]any{"challenge": base64.StdEncoding.EncodeToString(ch), "key-system": "com.widevine.alpha",
		"uri": keyURI, "adamId": id, "isLibrary": false, "user-initiated": true}
	if err := c.call(ctx, http.MethodPost, PlayURL+"/acquireWebPlaybackLicense", body, &r); err != nil {
		return nil, err
	}
	if r.Status != 0 || r.License == "" {
		return nil, fmt.Errorf("Apple refused the key (status %d)", r.Status)
	}
	lic, err := base64.StdEncoding.DecodeString(r.License)
	if err != nil {
		return nil, err
	}
	got, err := s.Keys(lic)
	if err != nil {
		return nil, err
	}
	out := map[int][]byte{}
	for i, kid := range kids {
		if kid == nil {
			continue
		}
		for _, k := range got {
			if bytes.Equal(k.ID, kid) {
				out[i] = k.Key
			}
		}
		if out[i] == nil {
			if i == 0 && len(kids) > 1 {
				out[i] = prefetchKey
			} else {
				out[i] = got[0].Key
			}
		}
	}
	return out, nil
}

// download writes the stream's pieces, in order, to path: a range each, resumed after a cut.
func (c *Client) download(ctx context.Context, pieces []piece, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 64<<10)
	for _, p := range merge(pieces) {
		got := int64(0)
		for try := 0; ; try++ {
			n, err := c.get(ctx, p, got, w)
			got += n
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !IsNoNetwork(err) || try >= 4 {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(try+1) * 3 * time.Second):
			}
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// get copies a piece from skip on to w. A connection that sends nothing for a minute is dropped
// (and resumed by download).
func (c *Client) get(ctx context.Context, p piece, skip int64, w io.Writer) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(time.Minute, cancel)
	defer stall.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.uri, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	switch {
	case p.n >= 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", p.at+skip, p.at+p.n-1))
	case skip > 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", skip))
	}
	hc := &http.Client{Transport: transport} // no total timeout: a song is megabytes
	resp, err := hc.Do(req)
	if err != nil {
		return 0, wrap(err)
	}
	defer resp.Body.Close()
	body := &alive{r: resp.Body, t: stall}
	switch {
	case resp.StatusCode == http.StatusOK && (p.n >= 0 || skip > 0):
		// No range served: the piece is cut from the whole file.
		start := skip
		if p.n >= 0 {
			start += p.at
		}
		if _, err := io.CopyN(io.Discard, body, start); err != nil {
			return 0, wrap(err)
		}
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent:
	default:
		return 0, &StatusError{Code: resp.StatusCode}
	}
	var r io.Reader = body
	if p.n >= 0 {
		r = io.LimitReader(body, p.n-skip)
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return n, wrap(err)
	}
	if p.n >= 0 && n != p.n-skip {
		return n, netError{io.ErrUnexpectedEOF}
	}
	return n, nil
}

// alive puts its stall timer back each time bytes come.
type alive struct {
	r io.Reader
	t *time.Timer
}

func (a *alive) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.t.Reset(time.Minute)
	}
	return n, err
}

// Save downloads the song to path: its stream fetched to path.enc.part, its key from the license,
// its samples decrypted into path.part with tags (and cover and lyrics, if given), renamed into
// place. Nothing is left but path, or nothing on failure.
func (c *Client) Save(ctx context.Context, s *Song, path string, tags *mp4.Tags) error {
	text, err := c.fetch(ctx, s.stream, 4<<20)
	if err != nil {
		return err
	}
	keys, pieces, err := playlist(s.stream, text)
	if err != nil {
		return err
	}
	keyURI := songKey(keys)
	if keyURI == "" {
		return errors.New("Apple's stream has no Widevine key")
	}
	enc, part := path+".enc.part", path+".part"
	defer os.Remove(enc)
	if err := c.download(ctx, pieces, enc); err != nil {
		return err
	}
	f, err := os.Open(enc)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	st, err := mp4.Read(f, fi.Size())
	if err != nil {
		return err
	}
	// iPods play AAC up to 48 kHz and skip above it; the web stream is always 44.1 kHz.
	if r := st.SampleRate(); r > 48000 {
		return fmt.Errorf("the stream is %d Hz, more than an iPod plays", r)
	}
	ks, err := c.keys(ctx, s.ID, keyURI, st.KeyIDs())
	if err != nil {
		return err
	}
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	err = st.Write(out, f, ks, tags)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, path)
	}
	if err != nil {
		os.Remove(part)
	}
	return err
}

// Cover is an artwork template's picture, size px square, as a JPEG; nil without one.
func (c *Client) Cover(ctx context.Context, template string, size int) []byte {
	if template == "" {
		return nil
	}
	b, err := c.fetch(ctx, CoverURL(template, size), 8<<20)
	if err != nil || len(b) < 4 {
		return nil
	}
	return b
}
