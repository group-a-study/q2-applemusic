package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/q2"
)

// Download All's record (the user's ask, 2026-11-04): Apple Music/Downloaded Albums and
// Playlists.txt lists the library's albums and playlists Download All has finished, every song of
// each on the card or in Not Downloaded.txt, with what the library's listing said of it then: an
// album's song count, a playlist's last change. Download All leaves out a list whose listing still
// says so, without asking Apple for its songs, so pressing it again fetches only the lists added or
// changed since, and doesn't try failed songs again (Try the Failed Again does that). A line deleted
// by hand has its list looked at again. With no line of a kind (the record new, or emptied by hand),
// that kind's lists already on the card count as finished: an album whose folder holds its song
// count, failed songs counted; a playlist whose folder was started after its last change.

// recordName is the record's file, in Apple Music/.
const recordName = "Downloaded Albums and Playlists.txt"

// libraryLink is a library list's page on music.apple.com, the record's key for it.
var libraryLink = map[string]string{"album": "https://music.apple.com/library/albums/",
	"playlist": "https://music.apple.com/library/playlist/"}

// finished is a line of the record: the list as the listing gave it when it was finished (Songs,
// for a playlist, how many it had), how many of its songs failed, and when.
type finished struct {
	List   bulkList
	Failed int
	When   string
}

// openList is a list Download All has queued and not finished: the songs it waits for.
type openList struct {
	List   bulkList `json:"list"`
	Keys   []string `json:"keys"`
	Failed int      `json:"failed,omitempty"`
}

// sig is what a list's listing says of it, which tells a finished one: an album's song count, a
// playlist's last change; "" when it says nothing (the list is then always looked at).
func (l bulkList) sig() string {
	kind, _, _ := strings.Cut(l.Ctx, ":")
	switch {
	case kind == "album" && l.Songs > 0:
		return strconv.Itoa(l.Songs)
	case kind == "playlist":
		return l.Changed
	}
	return ""
}

func (dl *downloads) recordFile() string { return filepath.Join(dl.folder, recordName) }

// readRecord is the record's lines, in its order; lines that name no library list are left out.
func readRecord(path string) []finished {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []finished
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 7 {
			continue
		}
		ctx := ""
		for kind, link := range libraryLink {
			if id, ok := strings.CutPrefix(strings.TrimSpace(f[6]), link); ok && id != "" && !strings.ContainsAny(id, "/?#") {
				ctx = kind + ":" + id
			}
		}
		if ctx == "" {
			continue
		}
		songs, _ := strconv.Atoi(strings.TrimSpace(f[3]))
		failed, _ := strconv.Atoi(strings.TrimSpace(f[5]))
		when := ""
		if len(f) > 7 {
			when = f[7]
		}
		out = append(out, finished{List: bulkList{Ctx: ctx, Name: f[1], Artist: f[2], Songs: songs,
			Changed: strings.TrimSpace(f[4])}, Failed: failed, When: when})
	}
	return out
}

// finishedSigs is the record's lists by context: each one's sig when it was finished; and whether
// the record has an album, and a playlist.
func (dl *downloads) finishedSigs() (map[string]string, map[string]bool) {
	dl.rmu.Lock()
	defer dl.rmu.Unlock()
	sigs, kinds := map[string]string{}, map[string]bool{}
	for _, f := range readRecord(dl.recordFile()) {
		sigs[f.List.Ctx] = f.List.sig()
		kind, _, _ := strings.Cut(f.List.Ctx, ":")
		kinds[kind] = true
	}
	return sigs, kinds
}

// finish writes lists to the record as finished, each in place of its line if it has one, else
// after the others.
func (dl *downloads) finish(fs ...finished) {
	if len(fs) == 0 || !cardReady() {
		return
	}
	dl.rmu.Lock()
	defer dl.rmu.Unlock()
	path := dl.recordFile()
	lines := readRecord(path)
	at := map[string]int{}
	for i, f := range lines {
		at[f.List.Ctx] = i
	}
	now := time.Now().Format("2006-01-02 15:04")
	for _, f := range fs {
		if f.When == "" {
			f.When = now
		}
		if i, ok := at[f.List.Ctx]; ok {
			lines[i] = f
		} else {
			at[f.List.Ctx] = len(lines)
			lines = append(lines, f)
		}
	}
	clean := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")
	var t strings.Builder
	t.WriteString("Albums and playlists Download All has finished: each one's songs are on the card, or in " + notDownloaded + ".\n")
	t.WriteString("Download All leaves these out until Apple Music lists an album with another song count, or a playlist changed since.\n")
	t.WriteString("Delete a line, and Download All looks at that one again.\n\n")
	t.WriteString("Kind\tName\tArtist\tSongs\tChanged\tNot Downloaded\tLink\tWhen\n")
	for _, f := range lines {
		kind, id, _ := strings.Cut(f.List.Ctx, ":")
		songs := ""
		if f.List.Songs > 0 {
			songs = itoa(f.List.Songs)
		}
		row := []string{strings.ToUpper(kind[:1]) + kind[1:], f.List.Name, f.List.Artist, songs, f.List.Changed,
			itoa(f.Failed), libraryLink[kind] + id, f.When}
		for i, v := range row {
			if i > 0 {
				t.WriteString("\t")
			}
			t.WriteString(clean.Replace(v))
		}
		t.WriteString("\n")
	}
	if os.MkdirAll(dl.folder, 0o755) == nil {
		q2.WriteFile(path, []byte(t.String()))
	}
}

// saveOpen writes the lists Download All waits on to .applemusic/lists.json (or removes it).
func (dl *downloads) saveOpen() {
	dl.wmu.Lock()
	defer dl.wmu.Unlock()
	dl.mu.Lock()
	b, _ := json.Marshal(dl.open)
	empty := len(dl.open) == 0
	dl.mu.Unlock()
	if empty {
		os.Remove(dl.openFile)
		return
	}
	if cardReady() {
		q2.WriteFile(dl.openFile, b)
	}
}

// settle is a song Download All may wait on that is now on the card, or failed: a list it was the
// last of is finished.
func (dl *downloads) settle(k string, failed bool) {
	dl.mu.Lock()
	var done []finished
	changed := false
	for ctx, o := range dl.open {
		i := index(o.Keys, k)
		if i < 0 {
			continue
		}
		o.Keys = append(o.Keys[:i], o.Keys[i+1:]...)
		if failed {
			o.Failed++
		}
		changed = true
		if len(o.Keys) == 0 {
			done = append(done, finished{List: o.List, Failed: o.Failed})
			delete(dl.open, ctx)
		}
	}
	dl.mu.Unlock()
	if changed {
		dl.finish(done...)
		dl.saveOpen()
	}
}

func index(l []string, s string) int {
	for i, v := range l {
		if v == s {
			return i
		}
	}
	return -1
}

// queuedKeys is the songs waiting or downloading (dl.mu held).
func (dl *downloads) queuedKeys() map[string]bool {
	q := map[string]bool{}
	if dl.cur != nil {
		q[key(dl.cur.Track)] = true
	}
	for _, it := range dl.waiting {
		q[key(it.Track)] = true
	}
	return q
}

// underWay is whether Download All waits on l's songs, all of them still queued, as its listing
// gives it now (dl.mu held).
func (dl *downloads) underWay(l bulkList, queued map[string]bool) bool {
	o := dl.open[l.Ctx]
	if o == nil || o.List.sig() != l.sig() {
		return false
	}
	for _, k := range o.Keys {
		if !queued[k] {
			return false
		}
	}
	return true
}

// onCardLists is, for a record with no line of a kind, that kind's lists already on the card
// (dl.mu held, the card read): finished as the record would have them.
func (dl *downloads) onCardLists(lists []bulkList, kinds map[string]bool, queued map[string]bool) []finished {
	var albums map[[2]string]int
	var playlists map[string][]string // id: its folders
	plSongs, failedIn := map[string]int{}, map[string]int{}
	for _, it := range dl.failed {
		failedIn[key(it.Track)]++
	}
	var out []finished
	for _, l := range lists {
		kind, id, _ := strings.Cut(l.Ctx, ":")
		if kinds[kind] || l.sig() == "" {
			continue
		}
		switch kind {
		case "album":
			if albums == nil {
				albums = dl.albumsOnCard()
			}
			n := albums[[2]string{artistKey(l.Artist), albumKey(l.Name)}]
			failed := 0
			for _, it := range dl.failed {
				t := it.Track
				if t.InAlbum != nil && t.InAlbum.ID == id ||
					t.InAlbum == nil && t.List == nil && albumKey(t.Album) == albumKey(l.Name) &&
						strings.Contains(artistKey(t.Artist), artistKey(l.Artist)) {
					failed++
				}
			}
			if n == 0 || n+failed < l.Songs || queuedWith(queued, "a"+id+":") {
				continue
			}
			out = append(out, finished{List: l, Failed: failed})
		case "playlist":
			if playlists == nil {
				playlists = map[string][]string{}
				for k, p := range dl.have {
					if list, _, ok := strings.Cut(strings.TrimPrefix(k, "p"), ":"); ok && strings.HasPrefix(k, "p") {
						dir := filepath.Dir(p)
						if index(playlists[list], dir) < 0 {
							playlists[list] = append(playlists[list], dir)
						}
						plSongs[list]++
					}
				}
			}
			changed, err := time.Parse(time.RFC3339, l.Changed)
			if err != nil || plSongs[id] == 0 || queuedWith(queued, "p"+id+":") {
				continue
			}
			started := true // every folder of it begun after the playlist's last change
			for _, dir := range playlists[id] {
				if fi, err := os.Stat(filepath.Join(dir, marker)); err != nil || fi.ModTime().Before(changed) {
					started = false
				}
			}
			if !started {
				continue
			}
			failed := 0
			for k, n := range failedIn {
				if strings.HasPrefix(k, "p"+id+":") {
					failed += n
				}
			}
			l.Songs = plSongs[id] + failed
			out = append(out, finished{List: l, Failed: failed})
		}
	}
	return out
}

func queuedWith(queued map[string]bool, prefix string) bool {
	for k := range queued {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// albumsOnCard is the songs on the card in album folders, <artist>/<album>/ in Apple Music/ or
// 48khz/, by artist and title key (dl.mu held).
func (dl *downloads) albumsOnCard() map[[2]string]int {
	out, seen := map[[2]string]int{}, map[string]bool{}
	for _, p := range dl.have {
		if seen[p] {
			continue
		}
		seen[p] = true
		dir := filepath.Dir(p)
		artist := filepath.Dir(dir)
		base := filepath.Dir(artist)
		if dir == dl.folder || filepath.Base(artist) == playlistsDir || artist == filepath.Join(dl.folder, hiresDir) ||
			base != dl.folder && base != filepath.Join(dl.folder, hiresDir) {
			continue
		}
		out[[2]string{artistKey(filepath.Base(artist)), albumKey(albumTitle(filepath.Base(dir)))}]++
	}
	return out
}
