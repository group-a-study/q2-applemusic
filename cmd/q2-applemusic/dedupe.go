package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/group-a-study/q2-applemusic/internal/q2"
)

// An album in both Deezer/ (q2-deezer's FLAC or MP3) and Apple Music/ (this app's AAC) is kept once
// in the library, as Apple Music's (the user's ask, 2026-11-04): its Deezer folder gets a .nomedia
// file, which Q2 Pod's library scan skips with all it holds (q2-pod's ringnav_scan_dir). An album
// only Deezer has stays. Albums match by their artist folder and title (loosely: case, punctuation,
// the year, bracketed editions and " - EP" or " - Single" aside), and only where Apple Music's copy
// has at least as many songs, so no song leaves the library. A .nomedia this app wrote (its text
// says so) goes again when the albums no longer match; one put there otherwise is never touched.
// Nothing is moved or deleted, and q2-deezer still sees its songs.

// deezerDir is q2-deezer's folder on the card.
const deezerDir = "Deezer"

const nomedia = ".nomedia"

// ourMark is the text of a .nomedia this app writes.
const ourMark = "q2-applemusic: this album is in Apple Music/ too, so the library has it once\n"

// deezerSong is a q2-deezer download's name.
var deezerSong = regexp.MustCompile(` \[-?[0-9]+\]\.(?i:flac|mp3)$`)

// albumSuffixYear is q2-deezer v3.2's album folder, "Tender Buttons (2005)".
var albumSuffixYear = regexp.MustCompile(`^(.+) \([0-9]{4}\)$`)

// artistKey is an artist folder's name compared loosely.
func artistKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// albumTitle is an album folder's title: its year left out, before or after.
func albumTitle(folder string) string {
	t, _ := albumYear(folder)
	if m := albumSuffixYear.FindStringSubmatch(t); m != nil {
		t = m[1]
	}
	return t
}

// albumsIn are root's albums, root/<artist>/<album>/, by artist and title key: their song counts
// (songs matched by song). Hidden folders and skip are left out.
func albumsIn(root string, song *regexp.Regexp, skip ...string) map[[2]string]int {
	out := map[[2]string]int{}
	artists, _ := os.ReadDir(root)
	for _, a := range artists {
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") || contains(skip, a.Name()) {
			continue
		}
		albums, _ := os.ReadDir(filepath.Join(root, a.Name()))
		for _, al := range albums {
			if !al.IsDir() || strings.HasPrefix(al.Name(), ".") {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(root, a.Name(), al.Name()))
			n := 0
			for _, f := range files {
				if song.MatchString(f.Name()) {
					n++
				}
			}
			k := [2]string{artistKey(a.Name()), albumKey(albumTitle(al.Name()))}
			if n > 0 && k[0] != "" && k[1] != "" && n > out[k] {
				out[k] = n
			}
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// dedupe marks the Deezer albums Apple Music/ has (and unmarks its own marks that no longer hold);
// how many are kept out of the library now.
func (d *daemon) dedupe() int {
	deezer := filepath.Join(filepath.Dir(d.dl.folder), deezerDir)
	if !cardReady() {
		return int(d.deduped.Load())
	}
	apple := albumsIn(d.dl.folder, named, playlistsDir, hiresDir)
	for k, n := range albumsIn(filepath.Join(d.dl.folder, hiresDir), named) {
		apple[k] = max(apple[k], n)
	}
	kept := 0
	artists, _ := os.ReadDir(deezer)
	for _, a := range artists {
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") {
			continue
		}
		albums, _ := os.ReadDir(filepath.Join(deezer, a.Name()))
		for _, al := range albums {
			if !al.IsDir() || strings.HasPrefix(al.Name(), ".") {
				continue
			}
			dir := filepath.Join(deezer, a.Name(), al.Name())
			files, _ := os.ReadDir(dir)
			n := 0
			for _, f := range files {
				if deezerSong.MatchString(f.Name()) {
					n++
				}
			}
			mark := filepath.Join(dir, nomedia)
			b, err := os.ReadFile(mark)
			ours, marked := err == nil && string(b) == ourMark, err == nil
			have := apple[[2]string{artistKey(a.Name()), albumKey(albumTitle(al.Name()))}]
			switch {
			case n > 0 && have >= n:
				if !marked && q2.WriteFile(mark, []byte(ourMark)) == nil {
					marked = true
				}
				if marked {
					kept++
				}
			case ours: // Apple Music's copy gone, or now short of songs
				os.Remove(mark)
			}
		}
	}
	d.deduped.Store(int64(kept))
	return kept
}

// lookForDuplicates runs dedupe in the background, one at a time.
func (d *daemon) lookForDuplicates() {
	if d.deduping.CompareAndSwap(false, true) {
		go func() {
			defer d.deduping.Store(false)
			d.dedupe()
		}()
	}
}
