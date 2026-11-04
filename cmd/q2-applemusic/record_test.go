package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/q2"
)

func libraryAlbum(id, name string, songs int) map[string]any {
	return res("library-albums", id, map[string]any{"name": name, "artistName": "Broadcast", "trackCount": songs})
}

// recordRows is the record's rows after its header, each without its When.
func recordRows(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(q2.Card, "Apple Music", recordName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 5 || lines[4] != "Kind\tName\tArtist\tSongs\tChanged\tNot Downloaded\tLink\tWhen" {
		t.Fatalf("record:\n%s", b)
	}
	var rows []string
	for _, l := range lines[5:] {
		f := strings.Split(l, "\t")
		if len(f) != 8 || len(f[7]) != len("2026-11-04 14:03") {
			t.Fatalf("record row %q", l)
		}
		rows = append(rows, strings.Join(f[:7], "|"))
	}
	return rows
}

// askedSince is what the fake was asked from the n-th request on.
func askedSince(f *fakeApple, n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.asked[n:], "\n")
}

func askedCount(f *fakeApple) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// TestDownloadAllRecord: Download All notes each album it finishes, and pressed again asks Apple
// only for the albums added or changed since; a song that failed isn't tried again.
func TestDownloadAllRecord(t *testing.T) {
	f := newFakeApple(t)
	tb := libraryAlbum("l.a1", "Tender Buttons", 2)
	f.albums = []any{tb}
	f.fail["102"] = 99 // Valerie fails each time
	d := rig(t, f)
	if r := ask(d, "bulk", "albums"); r.Message != "Downloading 1 album" {
		t.Fatalf("bulk: %+v", r)
	}
	until(t, "Tender Buttons", func() bool { return !d.dl.pending() && d.dl.count() == 1 })
	tbRow := "Album|Tender Buttons|Broadcast|2||1|https://music.apple.com/library/albums/l.a1"
	if rows := recordRows(t); strings.Join(rows, "\n") != tbRow {
		t.Fatalf("record %q", rows)
	}
	if _, err := os.Stat(d.dl.openFile); !os.IsNotExist(err) {
		t.Error("lists.json left behind")
	}

	// An album added to the library: only it is fetched, and Valerie isn't tried again.
	f.mu.Lock()
	f.albums = []any{tb, libraryAlbum("l.a2", "Berberian Sound Studio", 1)}
	valerie := f.plays["102"]
	f.mu.Unlock()
	d.cache.clear()
	n := askedCount(f)
	if r := ask(d, "bulk", "albums"); r.Message != "Downloading 1 album; 1 was downloaded before" {
		t.Fatalf("bulk again: %+v", r)
	}
	until(t, "Berberian Sound Studio", func() bool { return !d.dl.pending() && d.dl.count() == 2 })
	if asked := askedSince(f, n); strings.Contains(asked, "l.a1/tracks") || !strings.Contains(asked, "l.a2/tracks") {
		t.Errorf("asked:\n%s", asked)
	}
	f.mu.Lock()
	again := f.plays["102"]
	f.mu.Unlock()
	if again != valerie {
		t.Errorf("Valerie tried %d more times", again-valerie)
	}
	bssRow := "Album|Berberian Sound Studio|Broadcast|1||0|https://music.apple.com/library/albums/l.a2"
	if rows := recordRows(t); strings.Join(rows, "\n") != tbRow+"\n"+bssRow {
		t.Fatalf("record %q", rows)
	}

	// Nothing new: Apple is asked for the listing alone.
	d.cache.clear()
	n = askedCount(f)
	if r := ask(d, "bulk", "albums"); r.Message != "Nothing new: all 2 albums were downloaded before" {
		t.Fatalf("bulk with nothing new: %+v", r)
	}
	if asked := askedSince(f, n); asked != "GET /v1/me/library/albums" {
		t.Errorf("asked:\n%s", asked)
	}
	if d.dl.pending() {
		t.Error("Download All under way with nothing new")
	}

	// Another of Tender Buttons' songs added to the library: it is looked at again, its line updated.
	f.mu.Lock()
	f.albums = []any{libraryAlbum("l.a1", "Tender Buttons", 3), libraryAlbum("l.a2", "Berberian Sound Studio", 1)}
	f.mu.Unlock()
	d.cache.clear()
	n = askedCount(f)
	if r := ask(d, "bulk", "albums"); r.Message != "Downloading 1 album; 1 was downloaded before" {
		t.Fatalf("bulk, an album changed: %+v", r)
	}
	until(t, "the changed album looked at", func() bool { return strings.Contains(askedSince(f, n), "l.a1/tracks") && !d.dl.pending() })
	tb3 := strings.Replace(tbRow, "|2|", "|3|", 1)
	until(t, "its line", func() bool { return strings.Join(recordRows(t), "\n") == tb3+"\n"+bssRow })

	// A line deleted by hand: that album is looked at again.
	path := filepath.Join(q2.Card, "Apple Music", recordName)
	b, _ := os.ReadFile(path)
	var kept []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, "l.a2") {
			kept = append(kept, l)
		}
	}
	os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644)
	d.cache.clear()
	n = askedCount(f)
	if r := ask(d, "bulk", "albums"); r.Message != "Downloading 1 album; 1 was downloaded before" {
		t.Fatalf("bulk after a line deleted: %+v", r)
	}
	until(t, "its line back", func() bool { return strings.Join(recordRows(t), "\n") == tb3+"\n"+bssRow && !d.dl.pending() })
	if asked := askedSince(f, n); !strings.Contains(asked, "l.a2/tracks") || strings.Contains(asked, "POST /webPlayback") {
		t.Errorf("asked:\n%s", asked)
	}

	// Try the Failed Again still tries Valerie.
	f.mu.Lock()
	f.fail["102"] = 0
	f.mu.Unlock()
	if r := ask(d, "retry", "al.a1:102"); r.Message != "Trying again" {
		t.Fatalf("retry %+v", r)
	}
	until(t, "Valerie", func() bool { return !d.dl.pending() && d.dl.count() == 3 })
}

// TestDownloadAllFromCard: with no record, the lists already on the card (downloaded before it)
// count as finished without asking Apple for their songs: an album whose folder has its song
// count, a playlist whose folder was started after its last change.
func TestDownloadAllFromCard(t *testing.T) {
	f := newFakeApple(t)
	f.albums = []any{libraryAlbum("l.a1", "Tender Buttons", 2), libraryAlbum("l.a2", "Berberian Sound Studio", 1)}
	f.roadAt = "2025-04-03T12:21:29Z"
	d := rigWith(t, f, func(card string) {
		album := filepath.Join(card, "Apple Music", "Broadcast", "2005 - Tender Buttons")
		road := filepath.Join(card, "Apple Music", "Playlists", "Road Trip")
		os.MkdirAll(album, 0o755)
		os.MkdirAll(road, 0o755)
		for _, p := range []string{filepath.Join(album, "01 - Tears in the Typing Pool [101].m4a"),
			filepath.Join(album, "02 - Valerie [102].m4a"), filepath.Join(road, "01 - Pendulum [104].m4a"),
			filepath.Join(road, "02 - Valerie [102].m4a")} {
			os.WriteFile(p, []byte("m4a"), 0o644)
		}
		os.WriteFile(filepath.Join(road, marker), []byte("p.road\nRoad Trip\n"), 0o644)
	})
	until(t, "the card read", d.dl.scannedYet)
	n := askedCount(f)
	if r := ask(d, "bulk", "albums"); r.Message != "Downloading 1 album; 1 was downloaded before" {
		t.Fatalf("bulk albums: %+v", r)
	}
	until(t, "Berberian Sound Studio", func() bool { return !d.dl.pending() && d.dl.count() == 5 })
	if asked := askedSince(f, n); strings.Contains(asked, "l.a1/tracks") || !strings.Contains(asked, "l.a2/tracks") {
		t.Errorf("asked:\n%s", asked)
	}
	n = askedCount(f)
	if r := ask(d, "bulk", "playlists"); r.Message != "Nothing new: the playlist was downloaded before" {
		t.Fatalf("bulk playlists: %+v", r)
	}
	if asked := askedSince(f, n); asked != "GET /v1/me/library/playlists" {
		t.Errorf("asked:\n%s", asked)
	}
	if rows := recordRows(t); strings.Join(rows, "\n") != strings.Join([]string{
		"Album|Tender Buttons|Broadcast|2||0|https://music.apple.com/library/albums/l.a1",
		"Album|Berberian Sound Studio|Broadcast|1||0|https://music.apple.com/library/albums/l.a2",
		"Playlist|Road Trip||2|2025-04-03T12:21:29Z|0|https://music.apple.com/library/playlist/p.road"}, "\n") {
		t.Fatalf("record %q", rows)
	}

	// Road Trip changed since its folder was started: it is looked at again (its songs on the card).
	f.mu.Lock()
	f.roadAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	f.mu.Unlock()
	d.cache.clear()
	n = askedCount(f)
	if r := ask(d, "bulk", "playlists"); r.Message != "Downloading 1 playlist" {
		t.Fatalf("bulk, the playlist changed: %+v", r)
	}
	until(t, "Road Trip looked at", func() bool { return strings.Contains(askedSince(f, n), "p.road/tracks") && !d.dl.pending() })
	if asked := askedSince(f, n); strings.Contains(asked, "POST /webPlayback") {
		t.Errorf("a song on the card downloaded again:\n%s", asked)
	}
}
