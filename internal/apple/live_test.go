package apple

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/mp4"
)

// cookie is a Netscape cookies file's media-user-token for music.apple.com.
func cookie(t *testing.T, path string) string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fs := strings.Split(sc.Text(), "\t")
		if len(fs) >= 7 && fs[5] == "media-user-token" && strings.HasSuffix(fs[0], "music.apple.com") {
			return strings.TrimSpace(fs[6])
		}
	}
	t.Fatal("no media-user-token in the cookies file")
	return ""
}

// TestLive talks to Apple: AM_LIVE_COOKIES names a cookies file of a signed-in account. It
// searches, downloads one song and checks the M4A decodes.
func TestLive(t *testing.T) {
	path := os.Getenv("AM_LIVE_COOKIES")
	if path == "" {
		t.Skip("AM_LIVE_COOKIES not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := os.Getenv("AM_LIVE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	c := New(cookie(t, path), filepath.Join(dir, "token"), nil)
	a, err := c.Account(ctx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	t.Logf("account: subscription %v, storefront %s", a.Active, a.Storefront)
	term := os.Getenv("AM_LIVE_TERM")
	if term == "" {
		term = "Adele Rolling in the Deep"
	}
	sr, err := c.Search(ctx, term)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	t.Logf("search: %d songs, %d albums, %d artists, %d playlists", len(sr.Tracks), len(sr.Albums), len(sr.Artists), len(sr.Playlists))
	if len(sr.Tracks) == 0 {
		t.Fatal("no songs found")
	}
	tr := sr.Tracks[0]
	t.Logf("song: %s by %s (%s) %s", tr.Name, tr.Artist, tr.ID, tr.Artwork)
	s, err := c.Prepare(ctx, tr.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Logf("meta: %+v", s.Meta)
	lyr := c.Lyrics(ctx, tr.ID)
	t.Logf("lyrics: %d bytes, first: %.60q", len(lyr), lyr)
	tags := &mp4.Tags{Title: s.Meta.Title, Artist: s.Meta.Artist, Album: s.Meta.Album, AlbumArtist: s.Meta.AlbumArtist,
		Date: s.Meta.Date, Track: s.Meta.Track, Tracks: s.Meta.Tracks, Disc: s.Meta.Disc, Discs: s.Meta.Discs,
		Lyrics: lyr, Cover: c.Cover(ctx, tr.Artwork, 600)}
	out, err := c.Save(ctx, s, filepath.Join(dir, "song.m4a.enc.part"), tags, func(rate int) (string, error) {
		t.Logf("sample rate %d Hz", rate)
		return filepath.Join(dir, "song.m4a"), nil
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, _ := os.Stat(out)
	t.Logf("saved %d bytes", fi.Size())
	if ff, err := exec.LookPath("ffmpeg"); err == nil {
		b, err := exec.Command(ff, "-hide_banner", "-v", "error", "-i", out, "-f", "null", "-").CombinedOutput()
		if err != nil || len(b) > 0 {
			t.Fatalf("ffmpeg decode: %v %s", err, b)
		}
		t.Log("ffmpeg decodes it without an error")
	}
}
