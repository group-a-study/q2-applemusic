package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/apple"
	"github.com/group-a-study/q2-applemusic/internal/q2"
)

// Sort, .applemusic/sort (the user's ask, 2026-11-04): the order of the Albums and Playlists pages,
// which Download All takes too. "recent" (the default, and anything unknown) puts the last added
// first; "name" is A to Z; "artist" lists albums by artist, then year (oldest first, as their card
// folders), then name; "released" lists albums by release date, the newest first. Playlists, which
// have neither artist nor release date, go A to Z for those two.
var sorts = []struct{ key, title, sub, done string }{
	{"recent", "Recently Added", "Albums and Playlists, the last added first", "the last added first"},
	{"name", "Name", "Albums and Playlists, A to Z", "A to Z"},
	{"artist", "Artist", "Albums by artist, then year; playlists A to Z", "albums by artist"},
	{"released", "Release Date", "Albums, the newest release first; playlists A to Z", "albums by release date"},
}

func sortIndex(key string) int {
	for i, s := range sorts {
		if s.key == key {
			return i
		}
	}
	return 0
}

func readSort(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "sort"))
	return sorts[sortIndex(strings.TrimSpace(string(b)))].key
}

func (d *daemon) sortBy() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sort
}

// nextSort saves the order after the current one; the Albums and Playlists pages are fetched again in it.
func (d *daemon) nextSort() (int, error) {
	i := (sortIndex(d.sortBy()) + 1) % len(sorts)
	if err := q2.WriteFile(filepath.Join(d.dir, "sort"), []byte(sorts[i].key+"\n")); err != nil {
		return 0, err
	}
	d.mu.Lock()
	d.sort = sorts[i].key
	d.mu.Unlock()
	d.cache.drop("albums", "playlists")
	return i, nil
}

// nameKey is a name as sorted: case aside, and a leading "The" left out.
func nameKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if rest, ok := strings.CutPrefix(s, "the "); ok && rest != "" {
		return rest
	}
	return s
}

func sortAlbums(as []apple.Album, by string) {
	sort.SliceStable(as, func(i, j int) bool {
		a, b := as[i], as[j]
		switch by {
		case "name":
		case "artist":
			if ka, kb := nameKey(a.Artist), nameKey(b.Artist); ka != kb {
				return ka < kb
			}
			if a.Year != b.Year {
				return a.Year != "" && (b.Year == "" || a.Year < b.Year) // undated last
			}
		case "released":
			if a.Year != b.Year {
				return a.Year > b.Year // "" (undated) last
			}
		default:
			return newer(a.Added, b.Added)
		}
		return nameKey(a.Name) < nameKey(b.Name)
	})
}

func sortPlaylists(ps []apple.Playlist, by string) {
	sort.SliceStable(ps, func(i, j int) bool {
		if by == "recent" {
			return newer(ps[i].Added, ps[j].Added)
		}
		return nameKey(ps[i].Name) < nameKey(ps[j].Name)
	})
}

// newer is whether date a (a listing's dateAdded) is later than b; an undated one is the oldest, and
// Apple's order (by name) stays among equals.
func newer(a, b string) bool {
	ta, ea := time.Parse(time.RFC3339, a)
	tb, eb := time.Parse(time.RFC3339, b)
	switch {
	case ea != nil:
		return false
	case eb != nil:
		return true
	}
	return ta.After(tb)
}
