package apple

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Track is a song as a list shows it and the queue keeps it: its catalogue id (a library song's
// catalogId), what its row says, and its cover's URL template.
type Track struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Artist  string    `json:"artist,omitempty"`
	Album   string    `json:"album,omitempty"`
	Number  int       `json:"number,omitempty"`
	Disc    int       `json:"disc,omitempty"`
	Year    string    `json:"year,omitempty"`
	Artwork string    `json:"art,omitempty"`
	List    *ListRef  `json:"list,omitempty"`     // the playlist it is kept in as an album, if it is
	InAlbum *AlbumRef `json:"in_album,omitempty"` // the album it was listed in (Number and Disc are its place there)
}

// ListRef is a playlist a song downloads into, kept as an album: its id, name and cover, and the
// song's place in it.
// AlbumRef is the album a song was listed in: a compilation in the library can hold songs Apple
// matched to their own releases (a soundtrack not licensed in the country), which the song's tags
// from the playback service then name instead (downloads.go place).
type AlbumRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Artist string `json:"artist,omitempty"`
	Year   string `json:"year,omitempty"`
	Cover  string `json:"cover,omitempty"`
	Count  int    `json:"count,omitempty"`
}

type ListRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Cover string `json:"cover,omitempty"`
	Pos   int    `json:"pos"`
	Count int    `json:"count"`
}

type Album struct {
	ID, Name, Artist, Year, Artwork string
	Library                         bool // a library album (l.…), whose tracks are the library's
}

type Artist struct {
	ID, Name string
	Library  bool
}

type Playlist struct {
	ID, Name, Curator, Artwork string
	Library                    bool
}

// SearchResults are a term's songs, albums, artists and playlists.
type SearchResults struct {
	Tracks    []Track
	Albums    []Album
	Artists   []Artist
	Playlists []Playlist
}

// resource is an AMP resource as its lists give it.
type resource struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Name        string `json:"name"`
		ArtistName  string `json:"artistName"`
		AlbumName   string `json:"albumName"`
		CuratorName string `json:"curatorName"`
		TrackNumber int    `json:"trackNumber"`
		DiscNumber  int    `json:"discNumber"`
		ReleaseDate string `json:"releaseDate"`
		Artwork     *struct {
			URL string `json:"url"`
		} `json:"artwork"`
		PlayParams *struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			IsLibrary bool   `json:"isLibrary"`
			CatalogID string `json:"catalogId"`
		} `json:"playParams"`
	} `json:"attributes"`
	Relationships struct {
		Tracks  *page `json:"tracks"`
		Catalog *page `json:"catalog"`
	} `json:"relationships"`
}

type page struct {
	Data []resource `json:"data"`
	Next string     `json:"next"`
}

func (r resource) artwork() string {
	if r.Attributes.Artwork == nil {
		return ""
	}
	return r.Attributes.Artwork.URL
}

func year(date string) string {
	if len(date) >= 4 {
		if _, err := strconv.Atoi(date[:4]); err == nil {
			return date[:4]
		}
	}
	return ""
}

// track is a song resource as a Track; ok is false for one with no catalogue id (uploaded to the
// library, or not playable here).
func (r resource) track() (Track, bool) {
	a := r.Attributes
	id := ""
	if p := a.PlayParams; p != nil {
		switch {
		case p.CatalogID != "":
			id = p.CatalogID
		case !p.IsLibrary && p.Kind == "song":
			id = p.ID
		}
	}
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return Track{}, false
	}
	return Track{ID: id, Name: a.Name, Artist: a.ArtistName, Album: a.AlbumName, Number: a.TrackNumber,
		Disc: a.DiscNumber, Year: year(a.ReleaseDate), Artwork: r.artwork()}, true
}

// ErrNotPlayable is a list whose songs Apple lists, none of them playable in the account's country.
var ErrNotPlayable = errors.New("none of its songs plays in your country")

// playable is rs's songs, or ErrNotPlayable when it has songs and none plays.
func playable(rs []resource) ([]Track, error) {
	ts := tracks(rs)
	if len(ts) == 0 && len(rs) > 0 {
		return nil, ErrNotPlayable
	}
	return ts, nil
}

func tracks(rs []resource) []Track {
	out := make([]Track, 0, len(rs))
	for _, r := range rs {
		if t, ok := r.track(); ok {
			out = append(out, t)
		}
	}
	return out
}

func (r resource) album() Album {
	return Album{ID: r.ID, Name: r.Attributes.Name, Artist: r.Attributes.ArtistName, Year: year(r.Attributes.ReleaseDate),
		Artwork: r.artwork(), Library: strings.HasPrefix(r.Type, "library-")}
}

func (r resource) playlist() Playlist {
	return Playlist{ID: r.ID, Name: r.Attributes.Name, Curator: r.Attributes.CuratorName, Artwork: r.artwork(),
		Library: strings.HasPrefix(r.Type, "library-")}
}

// maxItems caps a list fetched whole (an account's library songs, a playlist).
const maxItems = 5000

// all is every item of a paged AMP list from u (limit 100 a page), following each page's next.
func (c *Client) all(ctx context.Context, u string) ([]resource, error) {
	var out []resource
	next := u
	for next != "" && len(out) < maxItems {
		var p page
		if err := c.call(ctx, http.MethodGet, next, nil, &p); err != nil {
			return out, err
		}
		out = append(out, p.Data...)
		next = ""
		if p.Next != "" {
			next = AMPURL + p.Next
			if q := keep(u, p.Next); q != "" {
				next += q
			}
		}
	}
	return out, nil
}

// keep is the query parameters of u that next leaves out (next names the offset, not the limit
// or the language), as gamdl's get_extended_api_data carries them.
func keep(u, next string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return ""
	}
	pn, err := url.Parse(next)
	if err != nil {
		return ""
	}
	have := pn.Query()
	add := url.Values{}
	for k, v := range pu.Query() {
		if _, ok := have[k]; !ok {
			add[k] = v
		}
	}
	if len(add) == 0 {
		return ""
	}
	if pn.RawQuery == "" {
		return "?" + add.Encode()
	}
	return "&" + add.Encode()
}

func (c *Client) catalog(ctx context.Context, path string, q url.Values) (string, error) {
	sf, err := c.storefront(ctx)
	if err != nil {
		return "", err
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("l", c.Language)
	return AMPURL + "/v1/catalog/" + sf + path + "?" + q.Encode(), nil
}

func library(path string, limit int) string {
	return AMPURL + "/v1/me/library" + path + "?limit=" + strconv.Itoa(limit)
}

// LibrarySongs is the library's songs that are in the catalogue.
func (c *Client) LibrarySongs(ctx context.Context) ([]Track, error) {
	rs, err := c.all(ctx, library("/songs", 100))
	return tracks(rs), err
}

// LibraryAlbums is the library's albums.
func (c *Client) LibraryAlbums(ctx context.Context) ([]Album, error) {
	rs, err := c.all(ctx, library("/albums", 100))
	out := make([]Album, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.album())
	}
	return out, err
}

// LibraryArtists is the library's artists.
func (c *Client) LibraryArtists(ctx context.Context) ([]Artist, error) {
	rs, err := c.all(ctx, library("/artists", 100))
	out := make([]Artist, 0, len(rs))
	for _, r := range rs {
		out = append(out, Artist{ID: r.ID, Name: r.Attributes.Name, Library: true})
	}
	return out, err
}

// LibraryArtistAlbums is a library artist's albums in the library.
func (c *Client) LibraryArtistAlbums(ctx context.Context, id string) ([]Album, error) {
	rs, err := c.all(ctx, library("/artists/"+url.PathEscape(id)+"/albums", 100))
	out := make([]Album, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.album())
	}
	return out, err
}

// LibraryPlaylists is the library's playlists.
func (c *Client) LibraryPlaylists(ctx context.Context) ([]Playlist, error) {
	rs, err := c.all(ctx, library("/playlists", 100))
	out := make([]Playlist, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.playlist())
	}
	return out, err
}

// Entry is an album or a playlist, in a list of both.
type Entry struct {
	Album    *Album
	Playlist *Playlist
}

// RecentlyAdded is the library's albums and playlists added last, newest first.
func (c *Client) RecentlyAdded(ctx context.Context) ([]Entry, error) {
	var p page
	err := c.call(ctx, http.MethodGet, library("/recently-added", 25), nil, &p)
	var out []Entry
	for _, r := range p.Data {
		switch r.Type {
		case "library-albums", "albums":
			a := r.album()
			out = append(out, Entry{Album: &a})
		case "library-playlists", "playlists":
			pl := r.playlist()
			out = append(out, Entry{Playlist: &pl})
		}
	}
	return out, err
}

// AlbumTracks is an album's songs: a library album's in the library, a catalogue album's all.
func (c *Client) AlbumTracks(ctx context.Context, id string) ([]Track, error) {
	var rs []resource
	var err error
	if strings.HasPrefix(id, "l.") {
		rs, err = c.all(ctx, library("/albums/"+url.PathEscape(id)+"/tracks", 100))
	} else {
		var u string
		if u, err = c.catalog(ctx, "/albums/"+url.PathEscape(id)+"/tracks", url.Values{"limit": {"100"}}); err != nil {
			return nil, err
		}
		rs, err = c.all(ctx, u)
	}
	if err != nil {
		return tracks(rs), err
	}
	return playable(rs)
}

// AlbumContext is an album as its songs are filed when they were matched to other releases: a library
// album's name and artist, with its catalogue album's year and cover when it has one.
func (c *Client) AlbumContext(ctx context.Context, id string) (AlbumRef, error) {
	var u string
	if strings.HasPrefix(id, "l.") {
		u = AMPURL + "/v1/me/library/albums/" + url.PathEscape(id) + "?include=catalog"
	} else {
		var err error
		if u, err = c.catalog(ctx, "/albums/"+url.PathEscape(id), nil); err != nil {
			return AlbumRef{}, err
		}
	}
	var p page
	if err := c.call(ctx, http.MethodGet, u, nil, &p); err != nil {
		return AlbumRef{}, err
	}
	if len(p.Data) == 0 {
		return AlbumRef{}, &StatusError{Code: 404}
	}
	r := p.Data[0]
	ref := AlbumRef{ID: id, Name: r.Attributes.Name, Artist: r.Attributes.ArtistName, Year: year(r.Attributes.ReleaseDate),
		Cover: r.artwork()}
	if cat := r.Relationships.Catalog; cat != nil && len(cat.Data) > 0 {
		ca := cat.Data[0]
		if y := year(ca.Attributes.ReleaseDate); y != "" {
			ref.Year = y
		}
		if a := ca.artwork(); a != "" {
			ref.Cover = a // the catalogue's template lasts; a library album's link expires in a day
		}
		if ref.Artist == "" {
			ref.Artist = ca.Attributes.ArtistName
		}
	}
	return ref, nil
}

// PlaylistTracks is a playlist's songs that are in the catalogue.
func (c *Client) PlaylistTracks(ctx context.Context, id string) ([]Track, error) {
	var rs []resource
	var err error
	if strings.HasPrefix(id, "p.") {
		rs, err = c.all(ctx, library("/playlists/"+url.PathEscape(id)+"/tracks", 100))
	} else {
		var u string
		if u, err = c.catalog(ctx, "/playlists/"+url.PathEscape(id)+"/tracks", url.Values{"limit": {"100"}}); err != nil {
			return nil, err
		}
		rs, err = c.all(ctx, u)
	}
	if err != nil {
		return tracks(rs), err
	}
	return playable(rs)
}

// PlaylistInfo is a playlist's name and cover.
func (c *Client) PlaylistInfo(ctx context.Context, id string) (Playlist, error) {
	var u string
	if strings.HasPrefix(id, "p.") {
		u = AMPURL + "/v1/me/library/playlists/" + url.PathEscape(id)
	} else {
		var err error
		if u, err = c.catalog(ctx, "/playlists/"+url.PathEscape(id), nil); err != nil {
			return Playlist{}, err
		}
	}
	var p page
	if err := c.call(ctx, http.MethodGet, u, nil, &p); err != nil {
		return Playlist{}, err
	}
	if len(p.Data) == 0 {
		return Playlist{}, &StatusError{Code: 404}
	}
	return p.Data[0].playlist(), nil
}

// AlbumInfo is a catalogue album's title, artist and year.
func (c *Client) AlbumInfo(ctx context.Context, id string) (Album, error) {
	var u string
	if strings.HasPrefix(id, "l.") {
		u = AMPURL + "/v1/me/library/albums/" + url.PathEscape(id)
	} else {
		var err error
		if u, err = c.catalog(ctx, "/albums/"+url.PathEscape(id), nil); err != nil {
			return Album{}, err
		}
	}
	var p page
	if err := c.call(ctx, http.MethodGet, u, nil, &p); err != nil {
		return Album{}, err
	}
	if len(p.Data) == 0 {
		return Album{}, &StatusError{Code: 404}
	}
	return p.Data[0].album(), nil
}

// ArtistTop is a catalogue artist's top songs.
func (c *Client) ArtistTop(ctx context.Context, id string) ([]Track, error) {
	u, err := c.catalog(ctx, "/artists/"+url.PathEscape(id)+"/view/top-songs", url.Values{"limit": {"20"}})
	if err != nil {
		return nil, err
	}
	var p page
	err = c.call(ctx, http.MethodGet, u, nil, &p)
	return tracks(p.Data), err
}

// ArtistAlbums is a catalogue artist's albums, then its singles and EPs.
func (c *Client) ArtistAlbums(ctx context.Context, id string) ([]Album, error) {
	var out []Album
	for _, view := range []string{"full-albums", "singles"} {
		u, err := c.catalog(ctx, "/artists/"+url.PathEscape(id)+"/view/"+view, url.Values{"limit": {"100"}})
		if err != nil {
			return out, err
		}
		rs, err := c.all(ctx, u)
		if err != nil {
			return out, err
		}
		for _, r := range rs {
			out = append(out, r.album())
		}
	}
	return out, nil
}

// ArtistName is a catalogue artist's name.
func (c *Client) ArtistName(ctx context.Context, id string) (string, error) {
	u, err := c.catalog(ctx, "/artists/"+url.PathEscape(id), nil)
	if err != nil {
		return "", err
	}
	var p page
	if err := c.call(ctx, http.MethodGet, u, nil, &p); err != nil {
		return "", err
	}
	if len(p.Data) == 0 {
		return "", &StatusError{Code: 404}
	}
	return p.Data[0].Attributes.Name, nil
}

// Search is a term's songs, albums, artists and playlists in the catalogue, 25 of each.
func (c *Client) Search(ctx context.Context, term string) (*SearchResults, error) {
	u, err := c.catalog(ctx, "/search", url.Values{"term": {term}, "types": {"songs,albums,artists,playlists"}, "limit": {"25"}})
	if err != nil {
		return nil, err
	}
	var r struct {
		Results struct {
			Songs, Albums, Artists, Playlists page
		} `json:"results"`
	}
	if err := c.call(ctx, http.MethodGet, u, nil, &r); err != nil {
		return nil, err
	}
	sr := &SearchResults{Tracks: tracks(r.Results.Songs.Data)}
	for _, a := range r.Results.Albums.Data {
		sr.Albums = append(sr.Albums, a.album())
	}
	for _, a := range r.Results.Artists.Data {
		sr.Artists = append(sr.Artists, Artist{ID: a.ID, Name: a.Attributes.Name})
	}
	for _, p := range r.Results.Playlists.Data {
		sr.Playlists = append(sr.Playlists, p.playlist())
	}
	return sr, nil
}

// Charts is the country's most played songs.
func (c *Client) Charts(ctx context.Context) ([]Track, error) {
	u, err := c.catalog(ctx, "/charts", url.Values{"types": {"songs"}, "limit": {"50"}})
	if err != nil {
		return nil, err
	}
	var r struct {
		Results struct {
			Songs []page `json:"songs"`
		} `json:"results"`
	}
	if err := c.call(ctx, http.MethodGet, u, nil, &r); err != nil {
		return nil, err
	}
	if len(r.Results.Songs) == 0 {
		return nil, nil
	}
	return tracks(r.Results.Songs[0].Data), nil
}

// CoverURL is an artwork template at size px square, as a JPEG.
func CoverURL(template string, size int) string {
	s := strconv.Itoa(size)
	return strings.NewReplacer("{w}", s, "{h}", s, "{f}", "jpg", "{c}", "bb").Replace(template)
}
