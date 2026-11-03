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

// fetchSong downloads a song: asked of the playback service, placed by at (its folder, file name
// and tags), its cover and lyrics fetched, saved as <name>.m4a. A playlist's album gets its marker
// and the playlist's cover first. Its path, or why not; a folder it made for nothing is removed.
func (d *daemon) fetchSong(ctx context.Context, am *apple.Client, t apple.Track,
	at func(apple.Track, apple.Meta) (string, string, *mp4.Tags)) (string, error) {
	s, err := am.Prepare(ctx, t.ID)
	if err != nil {
		return "", err
	}
	dir, name, tags := at(t, s.Meta)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if t.List != nil {
		d.playlistAlbum(ctx, am, dir, t.List)
	}
	tags.Cover = am.Cover(ctx, t.Artwork, coverSize)
	tags.Lyrics = am.Lyrics(ctx, t.ID)
	path := filepath.Join(dir, name+".m4a")
	if err := am.Save(ctx, s, path, tags); err != nil {
		os.Remove(dir) // only if empty: an album's folder this song would have started
		os.Remove(filepath.Dir(dir))
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
