package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/group-a-study/q2-applemusic/internal/apple"
	"github.com/group-a-study/q2-applemusic/internal/mp4"
	"github.com/group-a-study/q2-applemusic/internal/q2"
)

// coverSize is the embedded covers' and the playlist albums' cover.jpg's, in pixels: more than the
// Q2's screen shows, under what its decoder takes.
const coverSize = 600

// hiresRate is the sample rate from which a download goes in hiresDir: iPods skip on such songs,
// so they are kept apart (Apple's web stream is 44.1 kHz, so this is a safety net).
const hiresRate = 48000

// hiresDir is the folder in Apple Music/ holding songs of hiresRate or more, as Artist/Album/Song.
const hiresDir = "48khz"

// spot is where a song goes: its folder and file name (without .m4a), its folder should its stream
// be hiresRate or more ("" the same), the folder its encrypted stream waits in, and its tags.
type spot struct {
	dir, name, hires, tmp string
	tags                  *mp4.Tags
}

// fetchSong downloads a song: asked of the playback service, placed by at, its cover and lyrics
// fetched, saved as <name>.m4a in its folder for the stream's sample rate. A playlist's album gets
// its marker and the playlist's cover. Its path, or why not; a folder it made for nothing is removed.
func (d *daemon) fetchSong(ctx context.Context, am *apple.Client, t apple.Track,
	at func(apple.Track, apple.Meta) spot) (string, error) {
	s, err := am.Prepare(ctx, t.ID)
	if err != nil {
		return "", err
	}
	sp := at(t, s.Meta)
	if err := os.MkdirAll(sp.tmp, 0o755); err != nil {
		return "", err
	}
	sp.tags.Cover = am.Cover(ctx, t.Artwork, coverSize)
	sp.tags.Lyrics = am.Lyrics(ctx, t.ID)
	made := ""
	where := func(rate int) (string, error) {
		dir := sp.dir
		if rate >= hiresRate && sp.hires != "" {
			dir = sp.hires
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		made = dir
		if t.List != nil {
			d.playlistAlbum(ctx, am, dir, t.List)
		}
		return filepath.Join(dir, sp.name+".m4a"), nil
	}
	path, err := am.Save(ctx, s, filepath.Join(sp.tmp, sp.name+".m4a.enc.part"), sp.tags, where)
	if err != nil {
		if made != "" && made != sp.tmp {
			os.Remove(made) // only if empty: an album's folder this song would have started
			os.Remove(filepath.Dir(made))
		}
		return "", err
	}
	return path, nil
}

// playlistAlbum readies a playlist's album folder: its marker (the playlist's id and the album's
// name), and the playlist's cover as cover.jpg, Coverflow's and Albums' picture for it.
func (d *daemon) playlistAlbum(ctx context.Context, am *apple.Client, dir string, l *apple.ListRef) {
	if id, _ := readMarker(dir); id == "" {
		q2.WriteFile(filepath.Join(dir, marker), []byte(l.ID+"\n"+l.Name+"\n"))
	}
	cover := filepath.Join(dir, "cover.jpg")
	if _, err := os.Stat(cover); err == nil || l.Cover == "" {
		return
	}
	if b := am.Cover(ctx, l.Cover, coverSize); b != nil {
		q2.WriteFile(cover, b)
	}
}
