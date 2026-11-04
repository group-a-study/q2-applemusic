package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/apple"
	"github.com/group-a-study/q2-applemusic/internal/q2spot"
)

// The Q2's Apple Music page (Q2 Pod's card app page, q2-pod's patch/addon.c): the account's
// library and Apple Music's catalogue, each list opened by a row (kind list: its uri the op, its
// context the argument), a song downloaded by a press (kind action: download <list>#<id>), or in
// Stream mode played from it (stream <list>#<id>, stream.go), a whole list by its Download All row
// or Play/Pause held on its row (the row's position: the op held sends). A song on the card is a
// local row: a press plays it from the card, as local music (its uri a file; a folder's ends "/",
// which lists its songs; context "album", in track order, as an album's folder holds them). Lists
// whose rows change as downloads finish say "refresh", asked again every 2 s while on top.
//
// Lists of songs ("contexts") are named songs (the library's), chart, album:<id>, playlist:<id>,
// artisttop:<id> and search:<term>; fetched whole and kept two minutes, so a download takes the
// songs as the page showed them. Cursors are offsets.
//
// A big list (a library of thousands of songs is tens of Apple's pages) can take longer than the
// Q2 waits (15 s, q2-pod's patch/addon.c), so a request waits at most answerIn for its fetch,
// which carries on: the page says Loading and asks again ("refresh") until it's there. Once kept,
// a list past keepFor is still shown while it is fetched again.

const keepFor = 2 * time.Minute

var answerIn = 8 * time.Second // tests shorten it

// errLoading is a list still being fetched after answerIn.
var errLoading = errors.New("still loading")

type cache struct {
	mu    sync.Mutex
	m     map[string]*entry
	slots chan struct{} // the fetches under way, at most its capacity: the Q2's memory is small
}

// entry is a key's listing (fetched at at; at zero before the first), the fetch under way (busy,
// closed as it ends), and the last fetch's failure while there is no listing to show.
type entry struct {
	l    listing
	at   time.Time
	err  error
	busy chan struct{}
}

type listing struct {
	rows   []q2spot.Row
	tracks []apple.Track
	search *apple.SearchResults
	lists  []bulkList // an Albums or Playlists page's, for Download All
}

func newCache() *cache { return &cache{m: map[string]*entry{}, slots: make(chan struct{}, 2)} }

func (c *cache) clear() {
	c.mu.Lock()
	c.m = map[string]*entry{}
	c.mu.Unlock()
}

// get is key's listing: a kept one younger than keepFor; else fetch's, waited for at most answerIn
// (errLoading after, the fetch carrying on); else, while fetch runs again, the one kept.
func (c *cache) get(key string, fetch func() (listing, error)) (listing, error) {
	return c.getting(key, fetch, false)
}

// fresh is get, but an old kept listing waits for fetch as a first one does (Download All takes
// the library as it is now: albums added on a phone since the page was opened included). A fetch
// that fails leaves the kept one.
func (c *cache) fresh(key string, fetch func() (listing, error)) (listing, error) {
	return c.getting(key, fetch, true)
}

func (c *cache) getting(key string, fetch func() (listing, error), fresh bool) (listing, error) {
	c.mu.Lock()
	e := c.m[key]
	if e == nil {
		if len(c.m) >= 32 {
			for k, old := range c.m {
				if old.busy == nil {
					delete(c.m, k)
				}
			}
		}
		e = &entry{}
		c.m[key] = e
	}
	if !e.at.IsZero() && time.Since(e.at) < keepFor {
		defer c.mu.Unlock()
		return e.l, nil
	}
	if e.busy == nil {
		if err := e.err; err != nil { // it failed after its request stopped waiting: this one says so, the next tries again
			delete(c.m, key)
			c.mu.Unlock()
			return listing{}, err
		}
		e.busy = make(chan struct{})
		go c.run(e, fetch)
	}
	busy, kept, l := e.busy, !e.at.IsZero(), e.l
	c.mu.Unlock()
	if kept && !fresh {
		return l, nil
	}
	select {
	case <-busy:
	case <-time.After(answerIn):
		return listing{}, errLoading
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.at.IsZero() {
		if c.m[key] == e {
			delete(c.m, key) // told: the next request tries again
		}
		return listing{}, e.err
	}
	return e.l, nil
}

// run is e's fetch, when a slot is free; a failure keeps a listing e already has. A fetch never
// gets from the cache itself, which could leave every slot waiting on one more.
func (c *cache) run(e *entry, fetch func() (listing, error)) {
	c.slots <- struct{}{}
	l, err := fetch()
	<-c.slots
	c.mu.Lock()
	if err == nil {
		e.l, e.at, e.err = l, time.Now(), nil
	} else if e.at.IsZero() {
		e.err = err
	}
	close(e.busy)
	e.busy = nil
	c.mu.Unlock()
}

// fetchCtx is a list fetch's time limit: it carries on after the page stops waiting.
func fetchCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Minute)
}

// page is rows from cursor on, as many as fit, with the cursor for the rest; msg the reply's.
func page(rows []q2spot.Row, cursor, msg string) q2spot.Reply {
	from, _ := strconv.Atoi(cursor)
	if from < 0 || from > len(rows) {
		from = len(rows)
	}
	r := q2spot.Reply{Message: msg, Rows: rows[from:]}
	if _, kept := r.Encode("0"); from+kept < len(rows) {
		r.Rows = rows[from : from+kept]
		r.Cursor = strconv.Itoa(from + kept)
	}
	return r
}

func info(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }

// open is a row opening a list: its songs (op tracks), held Play/Pause downloading them.
func open(ctx, title, sub string) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: "tracks", Context: ctx, Title: title, Subtitle: sub, Position: "download"}
}

func joinNonEmpty(sep string, s ...string) string {
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, sep)
}

func albumRow(a apple.Album) q2spot.Row {
	return open("album:"+a.ID, a.Name, joinNonEmpty(" · ", a.Artist, a.Year))
}

func artistRow(a apple.Artist) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: "artist", Context: a.ID, Title: a.Name}
}

func playlistRow(p apple.Playlist) q2spot.Row {
	return open("playlist:"+p.ID, p.Name, p.Curator)
}

// trackRow is a song of the list ctx: on the card, a local row that plays it; else a press
// downloads it, or in Stream mode plays it (Play/Pause held still downloads it), its state beside
// its artist.
func (d *daemon) trackRow(t apple.Track, ctx string) q2spot.Row {
	artist := t.Artist
	op := "download"
	if d.streaming() {
		op = "stream"
		if d.st.loading(t.ID) {
			artist += " · loading"
		}
	}
	switch st, extra := d.dl.state(t); st {
	case "done":
		return q2spot.Row{Kind: "local", URI: extra, Context: d.dl.order(filepath.Dir(extra)), Title: t.Name,
			Subtitle: artist + " · on the card"}
	case "downloading":
		artist += " · downloading"
	case "waiting":
		artist += " · waiting"
	case "failed":
		artist += " · failed: press to try again"
	}
	return q2spot.Row{Kind: "action", URI: op, Context: ctx + "#" + t.ID, Title: t.Name, Subtitle: artist,
		Position: "download"}
}

// fail is an error as the page shows it; a list still loading, a row saying so while the page
// asks again.
func fail(err error) q2spot.Reply {
	switch {
	case errors.Is(err, errLoading):
		return q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{info("Loading…", "Apple Music is sending a long list")}}
	case errors.Is(err, errNoToken):
		return q2spot.Fail(q2spot.Login, "Sign in to Apple Music from your phone")
	case errors.Is(err, apple.ErrSignedOut):
		return q2spot.Fail(q2spot.Login, "Your Apple Music sign-in has expired: sign in again from your phone")
	case apple.IsNoNetwork(err):
		return q2spot.Fail(q2spot.Offline, "Apple Music can't be reached")
	case errors.Is(err, apple.ErrNotPlayable):
		return q2spot.Fail(q2spot.Unavailable, "None of it plays in your Apple Music country")
	}
	return q2spot.Fail(q2spot.Error, "Apple Music didn't answer")
}

// device answers the Q2's page; a slow answer (the Q2 waits 15 s) is logged.
func (d *daemon) device(req q2spot.Request) q2spot.Reply {
	t := time.Now()
	r := d.answer(req)
	if el := time.Since(t); el > 3*time.Second {
		logf("page: %s %q took %.1f s (%s %s)", req.Op, req.Args, el.Seconds(), r.Status, r.Message)
	}
	return r
}

func (d *daemon) answer(req q2spot.Request) q2spot.Reply {
	d.touch()
	ok := func(msg string) q2spot.Reply { return q2spot.Reply{Message: msg} }
	switch req.Op {
	case "caps":
		var rows []q2spot.Row
		for _, op := range []string{"caps", "menu", "recent", "playlists", "albums", "artists", "artist", "tracks",
			"search", "found", "downloads", "account", "download", "bulk", "bulkstop", "cancel", "cancelall", "retry",
			"clearfailed", "organize", "mode", "stream", "cardartists", "cardalbums", "signout", "stop"} {
			rows = append(rows, q2spot.Row{Kind: "op", URI: op})
		}
		return q2spot.Reply{Message: version, Rows: rows}
	case "menu":
		return d.menu()
	case "downloads":
		return d.downloadsPage(cursor(req, 0))
	case "cancel":
		if !d.dl.remove(req.Arg(0)) {
			return q2spot.Fail(q2spot.Unavailable, "It isn't waiting any more")
		}
		return ok("Removed")
	case "cancelall":
		return ok(plural(d.dl.clear(), "song") + " removed")
	case "bulkstop":
		return ok(plural(d.dl.bulkStop(), "list") + " left out; the songs queued still download")
	case "retry":
		if d.dl.retry(req.Arg(0)) == 0 {
			return ok("Nothing to try again")
		}
		return ok("Trying again")
	case "clearfailed":
		d.dl.clearFailed()
		return ok("Cleared")
	case "organize":
		on := !d.organized()
		if err := d.setOrganize(on); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if on {
			return ok("New downloads go in Artist/Year - Album folders")
		}
		return ok("New downloads go straight in Apple Music/")
	case "mode":
		stream := !d.streaming()
		if err := d.setMode(stream); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if stream {
			return ok("A press plays the song; nothing is kept")
		}
		return ok("A press downloads the song")
	case "cardartists": // Apple Music/ or its 48khz/
		if req.Arg(0) == hiresDir {
			return d.cardArtists(hiresDir, cursor(req, 1))
		}
		return d.cardArtists("", cursor(req, 0))
	case "cardalbums":
		return d.cardAlbums(req.Arg(0), cursor(req, 1))
	case "signout":
		if err := d.signOut(); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't remove the sign-in from the card")
		}
		return ok("Signed out; your downloads stay")
	case "stop":
		logf("Stop Apple Music pressed: stopping")
		go func() { time.Sleep(300 * time.Millisecond); d.shutdown() }() // the reply first
		return ok("Apple Music stopped")
	}
	am, err := d.session()
	if err != nil {
		return fail(err)
	}
	list := func(key, cursor, msg string, fetch func(ctx context.Context) (listing, error)) q2spot.Reply {
		l, err := d.cache.get(key, func() (listing, error) {
			ctx, cancel := fetchCtx()
			defer cancel()
			return fetch(ctx)
		})
		if err != nil {
			return fail(err)
		}
		return page(l.rows, cursor, msg)
	}
	switch req.Op {
	case "account":
		a := am.Known()
		sub := "Active"
		if a == nil || !a.Active {
			sub = "None: downloads need one"
		}
		country := ""
		if a != nil {
			country = strings.ToUpper(a.Storefront)
		}
		return q2spot.Reply{Rows: []q2spot.Row{info("Apple Music subscription", sub),
			info("Country", country+": the catalogue and licences are this country's"),
			{Kind: "action", URI: "signout", Title: "Sign Out", Subtitle: "Removes your sign-in; your downloads stay"}}}
	case "recent":
		return list("recent", cursor(req, 0), "", func(ctx context.Context) (listing, error) {
			es, err := am.RecentlyAdded(ctx)
			var l listing
			for _, e := range es {
				if e.Album != nil {
					l.rows = append(l.rows, albumRow(*e.Album))
				} else {
					l.rows = append(l.rows, playlistRow(*e.Playlist))
				}
			}
			if len(l.rows) == 0 && err == nil {
				l.rows = []q2spot.Row{info("Nothing added lately", "")}
			}
			return l, err
		})
	case "playlists":
		return list("playlists", cursor(req, 0), "", func(ctx context.Context) (listing, error) { return d.playlistsListing(ctx, am) })
	case "albums":
		return list("albums", cursor(req, 0), "", func(ctx context.Context) (listing, error) { return d.albumsListing(ctx, am) })
	case "artists":
		return list("artists", cursor(req, 0), "", func(ctx context.Context) (listing, error) {
			as, err := am.LibraryArtists(ctx)
			var l listing
			for _, a := range as {
				l.rows = append(l.rows, artistRow(a))
			}
			if len(l.rows) == 0 && err == nil {
				l.rows = []q2spot.Row{info("No artists in your library", "")}
			}
			return l, err
		})
	case "artist":
		id := req.Arg(0)
		if id == "" {
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		return list("artist:"+id, cursor(req, 1), "", func(ctx context.Context) (listing, error) {
			var l listing
			if strings.HasPrefix(id, "r.") { // the library's: its albums in the library
				as, err := am.LibraryArtistAlbums(ctx, id)
				for _, a := range as {
					l.rows = append(l.rows, albumRow(a))
				}
				return l, err
			}
			name, err := am.ArtistName(ctx, id)
			if err != nil {
				return l, err
			}
			l.rows = append(l.rows, open("artisttop:"+id, "Top Songs", name+"'s most played"))
			as, err := am.ArtistAlbums(ctx, id)
			for _, a := range as {
				l.rows = append(l.rows, albumRow(a))
			}
			return l, err
		})
	case "tracks":
		ctx := req.Arg(0)
		if !listCtx(ctx) {
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		return d.tracksPage(am, ctx, cursor(req, 1))
	case "search":
		term := strings.TrimSpace(req.Arg(0))
		if term == "" {
			return q2spot.Fail(q2spot.Bad, "Search for what?")
		}
		sr, err := d.search(am, term)
		if err != nil {
			return fail(err)
		}
		return q2spot.Reply{Rows: []q2spot.Row{
			{Kind: "list", URI: "found", Context: "tracks:" + term, Title: "Songs", Subtitle: plural(len(sr.Tracks), "song")},
			{Kind: "list", URI: "found", Context: "albums:" + term, Title: "Albums", Subtitle: plural(len(sr.Albums), "album")},
			{Kind: "list", URI: "found", Context: "artists:" + term, Title: "Artists", Subtitle: plural(len(sr.Artists), "artist")},
			{Kind: "list", URI: "found", Context: "playlists:" + term, Title: "Playlists",
				Subtitle: plural(len(sr.Playlists), "playlist")},
		}}
	case "found":
		kind, term, _ := strings.Cut(req.Arg(0), ":")
		if kind == "tracks" {
			return d.tracksPage(am, "search:"+term, cursor(req, 1))
		}
		sr, err := d.search(am, term)
		if err != nil {
			return fail(err)
		}
		var rows []q2spot.Row
		switch kind {
		case "albums":
			for _, a := range sr.Albums {
				rows = append(rows, albumRow(a))
			}
		case "artists":
			for _, a := range sr.Artists {
				rows = append(rows, artistRow(a))
			}
		case "playlists":
			for _, p := range sr.Playlists {
				rows = append(rows, playlistRow(p))
			}
		default:
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		if len(rows) == 0 {
			rows = append(rows, info("Nothing found", ""))
		}
		return page(rows, cursor(req, 1), "")
	case "download":
		return d.download(am, req.Arg(0))
	case "bulk":
		return d.bulk(am, req.Arg(0))
	case "stream":
		return d.stream(am, req.Arg(0))
	}
	return q2spot.Fail(q2spot.Unsupported, "Update Apple Music on the card")
}

// cursor is a list request's cursor, its argument after the n that name the list (Q2SPOT/1: the
// op's last argument).
func cursor(req q2spot.Request, n int) string {
	if len(req.Args) <= n {
		return ""
	}
	return req.Args[len(req.Args)-1]
}

// listCtx is a list of songs the page opens.
func listCtx(ctx string) bool {
	switch ctx {
	case "songs", "chart":
		return true
	}
	kind, rest, ok := strings.Cut(ctx, ":")
	switch kind {
	case "playlist", "album", "artisttop":
		return ok && rest != ""
	case "search":
		return ok && strings.TrimSpace(rest) != ""
	}
	return false
}

// search is a term's results, the four kinds at once, kept two minutes.
func (d *daemon) search(am *apple.Client, term string) (*apple.SearchResults, error) {
	l, err := d.cache.get("search\t"+term, func() (listing, error) {
		ctx, cancel := fetchCtx()
		defer cancel()
		sr, err := am.Search(ctx, term)
		return listing{search: sr}, err
	})
	if err != nil {
		return nil, err
	}
	return l.search, nil
}

// fetchTracks is a list's songs, asked of Apple: a playlist's each with its place in it, for its
// album (downloads.go).
func (d *daemon) fetchTracks(ctx context.Context, am *apple.Client, list string) ([]apple.Track, error) {
	kind, id, _ := strings.Cut(list, ":")
	switch kind {
	case "songs":
		return am.LibrarySongs(ctx)
	case "chart":
		return am.Charts(ctx)
	case "album":
		ts, err := am.AlbumTracks(ctx, id)
		if err != nil {
			return ts, err
		}
		if ref, err := am.AlbumContext(ctx, id); err == nil { // else filed by each song's own tags
			ref.Count = len(ts)
			for i := range ts {
				r := ref
				ts[i].InAlbum = &r
			}
		}
		return ts, nil
	case "artisttop":
		return am.ArtistTop(ctx, id)
	case "playlist":
		p, err := am.PlaylistInfo(ctx, id)
		if err != nil {
			return nil, err
		}
		ts, err := am.PlaylistTracks(ctx, id)
		if err != nil {
			return nil, err
		}
		for i := range ts {
			ts[i].List = &apple.ListRef{ID: id, Name: p.Name, Cover: p.Artwork, Pos: i + 1, Count: len(ts)}
		}
		return ts, nil
	}
	return nil, errors.New("unknown list " + list)
}

// tracks is a list's songs, as the page shows them and a download takes them.
func (d *daemon) tracks(am *apple.Client, ctx string) ([]apple.Track, error) {
	if term, ok := strings.CutPrefix(ctx, "search:"); ok { // the search's own, as its page counted them
		sr, err := d.search(am, term)
		if err != nil {
			return nil, err
		}
		return sr.Tracks, nil
	}
	l, err := d.cache.get(ctx, func() (listing, error) {
		fctx, cancel := fetchCtx()
		defer cancel()
		ts, err := d.fetchTracks(fctx, am, ctx)
		return listing{tracks: ts}, err
	})
	return l.tracks, err
}

// tracksPage is a list's songs: Download All first (on its first page), then each song.
func (d *daemon) tracksPage(am *apple.Client, ctx, cursor string) q2spot.Reply {
	ts, err := d.tracks(am, ctx)
	if err != nil {
		return fail(err)
	}
	if len(ts) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing here", "")}}
	}
	d.dl.fresh()
	on := 0
	rows := make([]q2spot.Row, 0, len(ts)+1)
	rows = append(rows, q2spot.Row{})
	for _, t := range ts {
		r := d.trackRow(t, ctx)
		if r.Kind == "local" {
			on++
		}
		rows = append(rows, r)
	}
	all := q2spot.Row{Kind: "action", URI: "download", Context: ctx, Title: "Download All",
		Subtitle: plural(len(ts), "song")}
	if strings.HasPrefix(ctx, "playlist:") {
		all.Subtitle += ", kept as an album"
	}
	if on > 0 {
		all.Subtitle += fmt.Sprintf(" · %d on the card", on)
	}
	if on == len(ts) {
		all = d.allOnCard(rows[1:])
	} else if d.streaming() {
		rows = append([]q2spot.Row{{Kind: "action", URI: "stream", Context: ctx, Title: "Play All",
			Subtitle: "Stream it, keeping none"}}, rows...)
		rows[1] = all
		return page(rows, cursor, "refresh")
	}
	rows[0] = all
	return page(rows, cursor, "refresh")
}

// pick is a list's songs, or one of them (arg <list>#<id>), as a press asked for them.
func (d *daemon) pick(am *apple.Client, arg string) ([]apple.Track, string, *q2spot.Reply) {
	ctx, one := arg, ""
	if i := strings.LastIndexByte(arg, '#'); i >= 0 {
		ctx, one = arg[:i], arg[i+1:]
	}
	if !listCtx(ctx) {
		r := q2spot.Fail(q2spot.Bad, "Bad request")
		return nil, "", &r
	}
	ts, err := d.tracks(am, ctx)
	if errors.Is(err, errLoading) { // a toast: the list's page isn't open to say Loading
		r := q2spot.Fail(q2spot.Unavailable, "Still loading: try again in a moment")
		return nil, "", &r
	}
	if err != nil {
		r := fail(err)
		return nil, "", &r
	}
	if one == "" {
		return ts, "", nil
	}
	for i, t := range ts {
		if t.ID == one {
			return ts[i:], one, nil
		}
	}
	r := q2spot.Fail(q2spot.Unavailable, "The list changed: open it again")
	return nil, "", &r
}

func subscribed(am *apple.Client) bool {
	a := am.Known()
	return a != nil && a.Active
}

// stream plays a list's songs in Stream mode (stream.go), from one of them (<list>#<id>) or its first.
func (d *daemon) stream(am *apple.Client, arg string) q2spot.Reply {
	if !subscribed(am) {
		return q2spot.Fail(q2spot.Premium, "Streaming needs an Apple Music subscription")
	}
	ts, _, bad := d.pick(am, arg)
	if bad != nil {
		return *bad
	}
	if len(ts) == 0 {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play")
	}
	d.dl.fresh()
	d.st.start(append([]apple.Track(nil), ts...))
	return q2spot.Reply{Message: "Playing in a moment"}
}

// download queues a list's songs, or one of them (<list>#<id>).
func (d *daemon) download(am *apple.Client, arg string) q2spot.Reply {
	if !subscribed(am) {
		return q2spot.Fail(q2spot.Premium, "Downloads need an Apple Music subscription")
	}
	ts, one, bad := d.pick(am, arg)
	if bad != nil {
		return *bad
	}
	d.dl.fresh()
	if one != "" {
		t := ts[0]
		ts = ts[:1]
		if st, _ := d.dl.state(t); st == "failed" {
			d.dl.retry(key(t))
			return q2spot.Reply{Message: "Trying again"}
		}
	}
	n := d.dl.add(ts)
	switch {
	case n == 0 && one != "":
		if st, _ := d.dl.state(ts[0]); st == "done" {
			return q2spot.Reply{Message: "Already on the card"}
		}
		return q2spot.Reply{Message: "Already in Downloads"}
	case n == 0:
		return q2spot.Reply{Message: "Already on the card or in Downloads"}
	case one != "":
		return q2spot.Reply{Message: "Downloading"}
	}
	return q2spot.Reply{Message: "Downloading " + plural(n, "song")}
}

// playlistsListing is the Playlists page: Top Charts, Download All, then the library's playlists.
func (d *daemon) playlistsListing(ctx context.Context, am *apple.Client) (listing, error) {
	ps, err := am.LibraryPlaylists(ctx)
	l := listing{rows: []q2spot.Row{open("chart", "Top Charts", "Apple Music's most played")}}
	for _, p := range ps {
		l.lists = append(l.lists, bulkList{Ctx: "playlist:" + p.ID, Name: p.Name, Changed: p.Changed})
	}
	if len(ps) > 0 {
		l.rows = append(l.rows, q2spot.Row{Kind: "action", URI: "bulk", Context: "playlists",
			Title: "Download All Playlists", Subtitle: "All " + plural(len(ps), "playlist") + " in your library, each as an album"})
	}
	for _, p := range ps {
		l.rows = append(l.rows, playlistRow(p))
	}
	return l, err
}

// albumsListing is the Albums page: Download All, then the library's albums.
func (d *daemon) albumsListing(ctx context.Context, am *apple.Client) (listing, error) {
	as, err := am.LibraryAlbums(ctx)
	var l listing
	for _, a := range as {
		l.lists = append(l.lists, bulkList{Ctx: "album:" + a.ID, Name: a.Name, Artist: a.Artist, Songs: a.Songs})
	}
	if len(as) > 0 {
		l.rows = append(l.rows, q2spot.Row{Kind: "action", URI: "bulk", Context: "albums",
			Title: "Download All Albums", Subtitle: "All " + plural(len(as), "album") + " in your library"})
	} else if err == nil {
		l.rows = append(l.rows, info("No albums in your library", "Add some in the Music app"))
	}
	for _, a := range as {
		l.rows = append(l.rows, albumRow(a))
	}
	return l, err
}

// bulk is Download All for the library's albums or playlists: each queued in turn (downloads.go
// expand), the next once the last one's songs are on the card; those finished before, and not
// changed since, left out (record.go).
func (d *daemon) bulk(am *apple.Client, kind string) q2spot.Reply {
	if !subscribed(am) {
		return q2spot.Fail(q2spot.Premium, "Downloads need an Apple Music subscription")
	}
	fetch := d.albumsListing
	switch kind {
	case "albums":
	case "playlists":
		fetch = d.playlistsListing
	default:
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	l, err := d.cache.fresh(kind, func() (listing, error) {
		ctx, cancel := fetchCtx()
		defer cancel()
		return fetch(ctx, am)
	})
	if errors.Is(err, errLoading) {
		return q2spot.Fail(q2spot.Unavailable, "Still loading: try again in a moment")
	}
	if err != nil {
		return fail(err)
	}
	if len(l.lists) == 0 {
		return q2spot.Reply{Message: "Nothing to download"}
	}
	one := strings.TrimSuffix(kind, "s")
	n, before := d.dl.bulkAdd(l.lists)
	switch {
	case n == 0 && before == 1 && len(l.lists) == 1:
		return q2spot.Reply{Message: "Nothing new: the " + one + " was downloaded before"}
	case n == 0 && before == len(l.lists):
		return q2spot.Reply{Message: "Nothing new: all " + plural(before, one) + " were downloaded before"}
	case n == 0:
		return q2spot.Reply{Message: "Already downloading them all"}
	case before > 0:
		were := " were"
		if before == 1 {
			were = " was"
		}
		return q2spot.Reply{Message: "Downloading " + plural(n, one) + "; " + itoa(before) + were + " downloaded before"}
	case n == 1:
		return q2spot.Reply{Message: "Downloading 1 " + one}
	}
	return q2spot.Reply{Message: "Downloading all " + plural(n, one)}
}

// allOnCard is a list's first row when all its songs (rows, local each) are on the card: their
// folder, played from the card (an album's in track order), or, spread over several, Downloaded
// Songs' artists.
func (d *daemon) allOnCard(rows []q2spot.Row) q2spot.Row {
	dirs := map[string]bool{}
	for _, r := range rows {
		dirs[filepath.Dir(r.URI)] = true
	}
	if len(dirs) > 1 {
		return q2spot.Row{Kind: "list", URI: "cardartists", Title: "All on the card", Subtitle: "Browse your Apple Music downloads"}
	}
	dir := filepath.Dir(rows[0].URI)
	return q2spot.Row{Kind: "local", URI: dir + "/", Context: d.dl.order(dir), Title: "All on the card",
		Subtitle: "Play your Apple Music downloads"}
}

// songsIn is how many downloaded songs dir holds, and its folders but hidden ones.
func songsIn(dir string) (int, []string) {
	des, _ := os.ReadDir(dir)
	n, dirs := 0, []string(nil)
	for _, de := range des {
		switch {
		case strings.HasPrefix(de.Name(), "."):
		case de.IsDir():
			dirs = append(dirs, de.Name())
		case named.MatchString(de.Name()):
			n++
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	return n, dirs
}

// cardArtists is Downloaded Songs with Artist/Album folders on the card (in base: "" for Apple
// Music/ itself, or 48khz): the songs in Apple Music/ itself (downloaded with Organize Files off),
// newest first, then each artist's folder (Playlists' among them), then 48 kHz and Higher, which
// lists its own the same way.
func (d *daemon) cardArtists(base, cursor string) q2spot.Reply {
	root := filepath.Join(d.dl.folder, base)
	loose, artists := songsIn(root)
	var rows []q2spot.Row
	if loose > 0 && base == "" {
		rows = append(rows, q2spot.Row{Kind: "local", URI: root + "/", Title: "Unsorted Songs",
			Subtitle: plural(loose, "song") + " in Apple Music/, newest first"})
	}
	hires := false
	for _, a := range artists {
		if base == "" && a == hiresDir {
			hires = true
			continue
		}
		_, albums := songsIn(filepath.Join(root, a))
		what := plural(len(albums), "album")
		if a == playlistsDir {
			what = plural(len(albums), "playlist") + ", each as an album"
		}
		rows = append(rows, q2spot.Row{Kind: "list", URI: "cardalbums", Context: filepath.Join(base, a), Title: a, Subtitle: what})
	}
	if n := d.deduped.Load(); n > 0 && base == "" {
		rows = append(rows, info(plural(int(n), "Deezer album")+" kept out of the library",
			"Apple Music/ has them: run Library → Update Local Music"))
	}
	if hires {
		_, as := songsIn(filepath.Join(root, hiresDir))
		rows = append(rows, q2spot.Row{Kind: "list", URI: "cardartists", Context: hiresDir, Title: "48 kHz and Higher",
			Subtitle: plural(len(as), "artist") + ", kept apart for iPods"})
	}
	if len(rows) == 0 {
		rows = append(rows, info("Nothing on the card yet", "Downloaded songs show here"))
	}
	return page(rows, cursor, "")
}

// albumYear is an album folder's title and year: "2005 - Tender Buttons" is Tender Buttons and
// 2005; without a year, the name and 9999, so those come after the dated ones.
func albumYear(folder string) (string, int) {
	m := yearPrefix.FindStringSubmatch(folder)
	if m == nil {
		return folder, 9999
	}
	y, _ := strconv.Atoi(m[1])
	return m[2], y
}

var yearPrefix = regexp.MustCompile(`^([0-9]{4}) - (.+)$`)

// cardAlbums is an artist's folder (in Apple Music/, or 48khz/<artist>): each album's, oldest first
// (then by name), its songs in track order.
func (d *daemon) cardAlbums(artist, cursor string) q2spot.Reply {
	name := strings.TrimPrefix(artist, hiresDir+"/") // an artist in 48khz/
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	dir := filepath.Join(d.dl.folder, artist)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return q2spot.Fail(q2spot.Unavailable, "It isn't on the card any more")
	}
	loose, albums := songsIn(dir)
	sort.SliceStable(albums, func(i, j int) bool {
		_, a := albumYear(albums[i])
		_, b := albumYear(albums[j])
		return a < b
	})
	var rows []q2spot.Row
	for _, a := range albums {
		n, _ := songsIn(filepath.Join(dir, a))
		title, year := albumYear(a)
		sub := plural(n, "song")
		if year != 9999 {
			sub = strconv.Itoa(year) + " · " + sub
		}
		rows = append(rows, q2spot.Row{Kind: "local", URI: filepath.Join(dir, a) + "/", Context: "album", Title: title,
			Subtitle: sub})
	}
	if loose > 0 {
		rows = append(rows, q2spot.Row{Kind: "local", URI: dir + "/", Title: "Other Songs", Subtitle: plural(loose, "song")})
	}
	if len(rows) == 0 {
		rows = append(rows, info("Nothing here", ""))
	}
	return page(rows, cursor, "")
}

// menu is the page's first level, from the sign-in as last found (never waiting on Apple).
func (d *daemon) menu() q2spot.Reply {
	d.dl.fresh()
	have := d.dl.count()
	songs := q2spot.Row{Kind: "local", URI: d.dl.folder + "/", Title: "Downloaded Songs", Subtitle: plural(have, "song") + " on the card"}
	if !d.dl.scannedYet() {
		songs.Subtitle = "Reading the card…"
	}
	if _, dirs := songsIn(d.dl.folder); len(dirs) > 0 { // Artist/Album folders: browsed by them
		songs.Kind, songs.URI = "list", "cardartists"
	}
	org := q2spot.Row{Kind: "action", URI: "organize", Title: "Organize Files: On", Subtitle: "Artist / Year - Album / 01 - Song"}
	if !d.organized() {
		org.Title, org.Subtitle = "Organize Files: Off", "Every song in one folder, Apple Music/"
	}
	mode := q2spot.Row{Kind: "action", URI: "mode", Title: "Mode: Download", Subtitle: "A press saves the song in Apple Music/"}
	if d.streaming() {
		mode.Title, mode.Subtitle = "Mode: Stream", "A press plays the song; nothing is kept"
	}
	stop := q2spot.Row{Kind: "action", URI: "stop", Title: "Stop Apple Music", Subtitle: "Downloads carry on next time"}
	queue := d.queueRow()
	var rows []q2spot.Row
	cookies := info("Or put cookies.txt on the card", "music.apple.com's, exported: it signs in by itself")
	d.mu.Lock()
	if d.cookieNote != "" {
		cookies = info("The cookies file didn't sign in", d.cookieNote)
	}
	d.mu.Unlock()
	switch d.authState() {
	case authNoToken:
		rows = []q2spot.Row{{Kind: "remote", Title: "Sign In from Your Phone", Subtitle: "Press to show the code"}, cookies,
			info("An Apple Music subscription is needed", "Songs download to the card, then play from it")}
	case authExpired:
		rows = []q2spot.Row{{Kind: "remote", Title: "Sign In Again from Your Phone", Subtitle: "Your sign-in has expired"}, cookies}
	case authFree:
		rows = []q2spot.Row{info("This account has no subscription", "Apple Music downloads need one"),
			{Kind: "remote", Title: "Sign In with Another Account", Subtitle: "Press to show the code"},
			{Kind: "action", URI: "signout", Title: "Sign Out", Subtitle: ""}}
	case authOffline:
		rows = []q2spot.Row{info("Apple Music can't be reached", "Connect the Q2 to Wi-Fi")}
	case authUnknown:
		rows = []q2spot.Row{info("Signing in…", "")}
	default:
		rows = []q2spot.Row{
			{Kind: "list", URI: "recent", Title: "Recently Added"},
			open("songs", "Songs", "Your library's"),
			{Kind: "list", URI: "playlists", Title: "Playlists", Subtitle: "Top Charts and yours"},
			{Kind: "list", URI: "albums", Title: "Albums"},
			{Kind: "list", URI: "artists", Title: "Artists"},
			{Kind: "search", Title: "Search Apple Music"},
			queue,
			mode,
			org,
			{Kind: "list", URI: "account", Title: "Account"},
		}
		return q2spot.Reply{Message: "refresh", Rows: append(append([]q2spot.Row{songs}, rows...), stop)}
	}
	if have > 0 {
		rows = append(rows, songs)
	}
	if d.dl.pending() {
		rows = append(rows, queue)
	}
	return q2spot.Reply{Message: "refresh", Rows: append(rows, stop)}
}

// queueRow is the menu's Downloads row: what is under way.
func (d *daemon) queueRow() q2spot.Row {
	cur, waiting, failed, done, stalled := d.dl.snapshot()
	left, _, _ := d.dl.bulkState()
	sub := "Nothing waiting"
	switch {
	case stalled != "" && (len(waiting) > 0 || left > 0):
		sub = fmt.Sprintf("%d waiting · %s", len(waiting), stalled)
	case cur != nil:
		sub = fmt.Sprintf("Downloading %d of %d", len(done)+1, len(done)+1+len(waiting))
	case left > 0:
		sub = "Download All: " + plural(left, "list") + " to go"
	case len(failed) > 0:
		sub = plural(len(failed), "song") + " failed"
	case len(done) > 0:
		sub = plural(len(done), "song") + " downloaded"
	}
	if cur != nil && left > 0 {
		sub += " · " + plural(left, "list") + " after"
	}
	return q2spot.Row{Kind: "list", URI: "downloads", Title: "Downloads", Subtitle: sub}
}

// downloadsPage is the queue: what downloads now, what waits (a press removes it), Download All's
// lists to go, what failed (a press tries it again), and what came (a press plays it).
func (d *daemon) downloadsPage(cursor string) q2spot.Reply {
	cur, waiting, failed, done, stalled := d.dl.snapshot()
	left, total, now := d.dl.bulkState()
	var rows []q2spot.Row
	if stalled != "" && (len(waiting) > 0 || left > 0) {
		rows = append(rows, info("Waiting: "+stalled, "It tries again by itself"))
	}
	if cur != nil {
		sub := cur.Track.Artist + " · " + plural(len(waiting), "song") + " after it"
		if cur.Attempt > 1 {
			sub = fmt.Sprintf("%s · try %d of %d", cur.Track.Artist, cur.Attempt, songRetries+1)
		}
		rows = append(rows, info("Downloading "+cur.Track.Name, sub))
	}
	if left > 0 {
		sub := fmt.Sprintf("%d of %d lists to go", left, total)
		if now != "" {
			sub = "Fetching " + now + " · " + sub
		}
		rows = append(rows, info("Download All", sub),
			q2spot.Row{Kind: "action", URI: "bulkstop", Title: "Stop Download All", Subtitle: "The songs queued still download"})
	}
	if len(waiting) > 1 {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "cancelall", Title: "Remove All Waiting",
			Subtitle: plural(len(waiting), "song")})
	}
	if len(failed) > 0 {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "retry", Title: "Try the Failed Again",
			Subtitle: plural(len(failed), "song")},
			q2spot.Row{Kind: "action", URI: "clearfailed", Title: "Clear the Failed", Subtitle: "And Apple Music/" + notDownloaded})
	}
	for _, it := range waiting {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "cancel", Context: key(it.Track), Title: it.Track.Name,
			Subtitle: it.Track.Artist + " · waiting: press to remove"})
	}
	for _, it := range failed {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "retry", Context: key(it.Track), Title: it.Track.Name,
			Subtitle: "Failed (" + it.Err + "): press to try again"})
	}
	for _, it := range done {
		rows = append(rows, q2spot.Row{Kind: "local", URI: it.Path, Context: d.dl.order(filepath.Dir(it.Path)), Title: it.Track.Name,
			Subtitle: it.Track.Artist + " · downloaded"})
	}
	if len(rows) == 0 {
		rows = append(rows, info("Nothing to download", "Press a song, or hold Play/Pause on a list"))
	}
	return page(rows, cursor, "refresh")
}
