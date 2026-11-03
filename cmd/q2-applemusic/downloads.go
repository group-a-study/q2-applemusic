package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/apple"
	"github.com/group-a-study/q2-applemusic/internal/mp4"
	"github.com/group-a-study/q2-applemusic/internal/q2"
)

// The download queue (contrib/shanlingq2/README, Downloads): one song at a time (the Q2's card,
// CPU and Wi-Fi), each fetched, decrypted and tagged (title, artists, album, numbers, date, cover,
// lyrics) and renamed into place under Apple Music/ on the card:
//
//   - from a playlist, in its own album, Playlists/<playlist>/07 - Title [id].m4a: tagged with the
//     playlist's name as its album (album artist "Playlists", numbered in the playlist's order),
//     each song keeping its own cover, the playlist's in the folder as cover.jpg;
//   - else, with Organize Files on, in its album's folder, <album artist>/<year> - <album>/07 -
//     Title [id].m4a; off, in Apple Music/ itself as "Title - Artist [id].m4a".
//
// The Q2 plays them from there, as local music. The ones waiting are kept in .applemusic/queue.json,
// so they carry on after a restart or USB storage. Download All (bulk.json) queues the library's
// albums or playlists one at a time: the next one's songs are fetched once the last one's are
// downloaded, so a library of hundreds asks Apple for one list at a time, and the queue stays short.

// cardReady is whether the card is there (not lent to USB storage); tests have one that is.
var cardReady = q2.CardMounted

// retryIn is how long a stalled queue (offline, signed out, the card away) waits before it tries again.
var retryIn = 30 * time.Second

// gap is the pause between two downloads, so a long queue asks Apple at a person's pace.
var gap = 2 * time.Second

// maxTries is how many times a song may be under way when q2-applemusic dies (killed, out of
// memory, a crash) before the queue moves it to the failed ones, so one song can't keep the add-on
// down: its launcher restarts it, and the queue would start that song again first.
const maxTries = 3

// playlistsDir is the folder in Apple Music/ that holds the playlists kept as albums, and their
// album artist.
const playlistsDir = "Playlists"

// marker is a playlist album's file naming its playlist: its id, then the album's name.
const marker = ".q2-playlist"

type item struct {
	Track apple.Track `json:"track"`
	Err   string      `json:"err,omitempty"` // a failed one's reason
	Path  string      `json:"path,omitempty"`
	Tries int         `json:"tries,omitempty"` // the times it was under way when q2-applemusic died
}

// key is what a song is on the card as: its id, or in a playlist's album, the playlist's and its.
func key(t apple.Track) string {
	if t.List != nil {
		return "p" + t.List.ID + ":" + t.ID
	}
	return t.ID
}

// bulkList is a list Download All will queue: its context (album:<id>, playlist:<id>) and name.
type bulkList struct {
	Ctx  string `json:"ctx"`
	Name string `json:"name"`
}

type bulkJob struct {
	Lists []bulkList `json:"lists"`
	Total int        `json:"total"`
}

type downloads struct {
	d        *daemon
	folder   string // Apple Music/ on the card
	file     string // .applemusic/queue.json
	bulkFile string // .applemusic/bulk.json

	mu        sync.Mutex
	wmu       sync.Mutex // one queue.json write at a time: they share its .tmp
	waiting   []*item
	cur       *item
	cancel    context.CancelFunc // the current download's
	dropped   bool               // the current one removed from the queue, not stopped by a quit
	failed    []*item
	done      []*item           // this session's, oldest first
	have      map[string]string // key: its file
	ids       map[string]string // song id: a file of it, any
	scanned   time.Time
	stalled   string // why the queue doesn't move now; "" when it does
	wake      chan struct{}
	saveTimer *time.Timer
	bulk      bulkJob
	expanding string // the list Download All is fetching now
}

func newDownloads(d *daemon, folder, file, bulkFile string) *downloads {
	dl := &downloads{d: d, folder: folder, file: file, bulkFile: bulkFile, wake: make(chan struct{}, 1),
		have: map[string]string{}, ids: map[string]string{}}
	var kept []*item
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &kept)
	}
	for _, it := range kept {
		if it != nil && it.Track.ID != "" {
			dl.waiting = append(dl.waiting, &item{Track: it.Track, Tries: it.Tries})
		}
	}
	if b, err := os.ReadFile(bulkFile); err == nil {
		json.Unmarshal(b, &dl.bulk)
	}
	dl.sweep()
	dl.rescan()
	return dl
}

// leftover is a download's temp files (apple.Client.Save): "<name>.m4a.enc.part", "<name>.m4a.part".
var leftover = regexp.MustCompile(`\.(?i:m4a)(\.enc)?\.part$`)

// walk calls fn for each file in the folder and its folders (hidden ones skipped).
func (dl *downloads) walk(fn func(path, name string)) {
	filepath.WalkDir(dl.folder, func(p string, de fs.DirEntry, err error) error {
		switch {
		case err != nil: // unreadable, or no folder yet
		case de.IsDir() && p != dl.folder && strings.HasPrefix(de.Name(), "."):
			return filepath.SkipDir
		case !de.IsDir():
			fn(p, de.Name())
		}
		return nil
	})
}

// sweep deletes the temp files an interrupted download left (q2-applemusic stopped or killed
// under way); nothing downloads yet, so none is in use.
func (dl *downloads) sweep() {
	dl.walk(func(path, name string) {
		if leftover.MatchString(name) {
			os.Remove(path)
		}
	})
}

// named is a downloaded song's id: every name ends " [<id>].m4a".
var named = regexp.MustCompile(` \[([0-9]+)\]\.(?i:m4a)$`)

// readMarker is a playlist album folder's playlist id and album name ("" when dir isn't one).
func readMarker(dir string) (id, name string) {
	f, err := os.Open(filepath.Join(dir, marker))
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		id = strings.TrimSpace(sc.Text())
	}
	if sc.Scan() {
		name = strings.TrimSpace(sc.Text())
	}
	return id, name
}

// rescan reads the folder for the songs already downloaded.
func (dl *downloads) rescan() {
	have, ids := map[string]string{}, map[string]string{}
	lists := map[string]string{} // folder: its playlist
	dl.walk(func(path, name string) {
		m := named.FindStringSubmatch(name)
		if m == nil {
			return
		}
		dir := filepath.Dir(path)
		list, ok := lists[dir]
		if !ok {
			list, _ = readMarker(dir)
			lists[dir] = list
		}
		if list != "" {
			have["p"+list+":"+m[1]] = path
		} else {
			have[m[1]] = path
		}
		ids[m[1]] = path
	})
	dl.mu.Lock()
	dl.have, dl.ids, dl.scanned = have, ids, time.Now()
	dl.mu.Unlock()
}

// order is the local row context for songs in dir: "album" in an album's folder, so the Q2 lists
// and plays them in track order (by name); "" in Apple Music/ itself, newest first.
func (dl *downloads) order(dir string) string {
	if dir != dl.folder {
		return "album"
	}
	return ""
}

// fresh rescans the folder when the last scan is a minute old (files deleted or added by hand).
func (dl *downloads) fresh() {
	dl.mu.Lock()
	old := time.Since(dl.scanned) > time.Minute
	dl.mu.Unlock()
	if old {
		dl.rescan()
	}
}

// count is how many songs are on the card (a song in an album and a playlist counts twice).
func (dl *downloads) count() int {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return len(dl.have)
}

// anyCopy is a file of the song id on the card, "" without one.
func (dl *downloads) anyCopy(id string) string {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.ids[id]
}

// state is a song's: "done" (its file), "downloading", "waiting", "failed" (its reason), or "".
func (dl *downloads) state(t apple.Track) (string, string) {
	k := key(t)
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if p, ok := dl.have[k]; ok {
		return "done", p
	}
	if dl.cur != nil && key(dl.cur.Track) == k {
		return "downloading", ""
	}
	for _, it := range dl.waiting {
		if key(it.Track) == k {
			return "waiting", ""
		}
	}
	for _, it := range dl.failed {
		if key(it.Track) == k {
			return "failed", it.Err
		}
	}
	return "", ""
}

// add queues the songs not on the card or queued already (a failed one is tried again); it
// returns how many it queued.
func (dl *downloads) add(ts []apple.Track) int {
	dl.mu.Lock()
	n := 0
	queued := map[string]bool{}
	if dl.cur != nil {
		queued[key(dl.cur.Track)] = true
	}
	for _, it := range dl.waiting {
		queued[key(it.Track)] = true
	}
	for _, t := range ts {
		k := key(t)
		if _, on := dl.have[k]; on || queued[k] || t.ID == "" {
			continue
		}
		queued[k] = true
		dl.failed = drop(dl.failed, k)
		dl.waiting = append(dl.waiting, &item{Track: t})
		n++
	}
	dl.mu.Unlock()
	if n > 0 {
		dl.save()
		dl.poke()
	}
	return n
}

func drop(l []*item, k string) []*item {
	out := l[:0]
	for _, it := range l {
		if key(it.Track) != k {
			out = append(out, it)
		}
	}
	return out
}

// remove takes a song out of the queue (by key): waiting, or stopping it if it is downloading.
func (dl *downloads) remove(k string) bool {
	dl.mu.Lock()
	n := len(dl.waiting)
	dl.waiting = drop(dl.waiting, k)
	found := len(dl.waiting) != n
	if dl.cur != nil && key(dl.cur.Track) == k && dl.cancel != nil {
		dl.dropped = true
		dl.cancel()
		found = true
	}
	dl.mu.Unlock()
	if found {
		dl.save()
	}
	return found
}

// clear empties the waiting songs, and Download All's lists.
func (dl *downloads) clear() int {
	dl.mu.Lock()
	n := len(dl.waiting)
	dl.waiting = nil
	dl.bulk = bulkJob{}
	dl.mu.Unlock()
	dl.save()
	dl.saveBulk()
	return n
}

// retry queues the failed songs again (k "" for all of them).
func (dl *downloads) retry(k string) int {
	dl.mu.Lock()
	var again []apple.Track
	keep := dl.failed[:0]
	for _, it := range dl.failed {
		if k == "" || key(it.Track) == k {
			again = append(again, it.Track)
		} else {
			keep = append(keep, it)
		}
	}
	dl.failed = keep
	dl.mu.Unlock()
	return dl.add(again)
}

func (dl *downloads) clearFailed() {
	dl.mu.Lock()
	dl.failed = nil
	dl.mu.Unlock()
}

// busy is whether a song is downloading: the Q2 kept awake.
func (dl *downloads) busy() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.cur != nil || dl.expanding != ""
}

// pending is whether anything is downloading or waiting, Download All's lists included.
func (dl *downloads) pending() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.cur != nil || len(dl.waiting) > 0 || len(dl.bulk.Lists) > 0
}

func (dl *downloads) poke() {
	select {
	case dl.wake <- struct{}{}:
	default:
	}
}

// save writes the waiting songs (the one downloading first) to the card, a moment after the last
// change, so a whole album queued is one write.
func (dl *downloads) save() {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.saveTimer != nil {
		dl.saveTimer.Stop()
	}
	dl.saveTimer = time.AfterFunc(500*time.Millisecond, dl.write)
}

func (dl *downloads) write() {
	dl.wmu.Lock()
	defer dl.wmu.Unlock()
	dl.mu.Lock()
	var l []*item
	if dl.cur != nil && !dl.dropped {
		l = append(l, &item{Track: dl.cur.Track, Tries: dl.cur.Tries})
	}
	for _, it := range dl.waiting {
		l = append(l, &item{Track: it.Track, Tries: it.Tries})
	}
	dl.mu.Unlock()
	if len(l) == 0 {
		os.Remove(dl.file)
		return
	}
	b, _ := json.Marshal(l)
	if cardReady() {
		q2.WriteFile(dl.file, b)
	}
}

// saveBulk writes Download All's lists to the card (or removes the file when there are none).
func (dl *downloads) saveBulk() {
	dl.wmu.Lock()
	defer dl.wmu.Unlock()
	dl.mu.Lock()
	b, _ := json.Marshal(dl.bulk)
	empty := len(dl.bulk.Lists) == 0
	dl.mu.Unlock()
	if empty {
		os.Remove(dl.bulkFile)
		return
	}
	if cardReady() {
		q2.WriteFile(dl.bulkFile, b)
	}
}

// bulkAdd adds lists to Download All (those it doesn't have); how many it added.
func (dl *downloads) bulkAdd(lists []bulkList) int {
	dl.mu.Lock()
	have := map[string]bool{}
	for _, l := range dl.bulk.Lists {
		have[l.Ctx] = true
	}
	n := 0
	for _, l := range lists {
		if !have[l.Ctx] {
			have[l.Ctx] = true
			dl.bulk.Lists = append(dl.bulk.Lists, l)
			n++
		}
	}
	if len(dl.bulk.Lists) == n { // a new run: counted from here
		dl.bulk.Total = 0
	}
	dl.bulk.Total += n
	dl.mu.Unlock()
	if n > 0 {
		dl.saveBulk()
		dl.poke()
	}
	return n
}

// bulkStop ends Download All: the songs already queued still download.
func (dl *downloads) bulkStop() int {
	dl.mu.Lock()
	n := len(dl.bulk.Lists)
	dl.bulk = bulkJob{}
	dl.mu.Unlock()
	dl.saveBulk()
	return n
}

// bulkState is Download All's lists left, of how many, and the one being fetched.
func (dl *downloads) bulkState() (left, total int, now string) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return len(dl.bulk.Lists), dl.bulk.Total, dl.expanding
}

// stall puts it back first in the queue: the queue waits (why), then tries again.
func (dl *downloads) stall(it *item, why string) {
	dl.mu.Lock()
	dl.cur, dl.cancel = nil, nil
	if it != nil {
		dl.waiting = append([]*item{it}, dl.waiting...)
	}
	dl.stalled = why
	dl.mu.Unlock()
}

// run downloads the waiting songs one after another, then Download All's next list's, until ctx
// ends (the add-on stopping, which leaves the one downloading for next time).
func (dl *downloads) run(ctx context.Context) {
	for {
		dl.mu.Lock()
		stalled := dl.stalled != ""
		dl.mu.Unlock()
		if !stalled {
			for dl.next(ctx) || dl.expand(ctx) {
			}
		}
		wait := time.NewTimer(retryIn)
		select {
		case <-ctx.Done():
			wait.Stop()
			dl.write()
			return
		case <-dl.wake:
		case <-wait.C:
		}
		wait.Stop()
		dl.mu.Lock()
		dl.stalled = ""
		dl.mu.Unlock()
	}
}

// why is a session's failure as the queue's stall says it ("" when it isn't one that waits).
func (dl *downloads) why(err error) string {
	switch {
	case errors.Is(err, errNoToken):
		return "Sign in to Apple Music"
	case errors.Is(err, apple.ErrSignedOut):
		dl.d.setAuth(authExpired)
		return "Your sign-in expired: sign in again"
	case apple.IsNoNetwork(err):
		return "Apple Music can't be reached"
	case errors.Is(err, apple.ErrNoSubscription):
		return "Downloads need an Apple Music subscription"
	}
	return ""
}

// ready is the session to download with; why the queue waits, when it can't.
func (dl *downloads) ready() (*apple.Client, string) {
	if !cardReady() {
		return nil, "The card is away"
	}
	am, err := dl.d.session()
	if err != nil {
		if w := dl.why(err); w != "" {
			return nil, w
		}
		return nil, "Apple Music didn't answer"
	}
	if a := am.Known(); a != nil && !a.Active {
		return nil, "Downloads need an Apple Music subscription"
	}
	return am, ""
}

// expand takes Download All's next list, once nothing else waits: its songs fetched and queued.
// False when there is none, or the queue stalled.
func (dl *downloads) expand(ctx context.Context) bool {
	dl.mu.Lock()
	if len(dl.bulk.Lists) == 0 || len(dl.waiting) > 0 || dl.cur != nil || ctx.Err() != nil {
		dl.mu.Unlock()
		return false
	}
	l := dl.bulk.Lists[0]
	dl.expanding = l.Name
	dl.mu.Unlock()
	defer func() {
		dl.mu.Lock()
		dl.expanding = ""
		dl.mu.Unlock()
	}()
	am, why := dl.ready()
	if am == nil {
		dl.stall(nil, why)
		return false
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	ts, err := dl.d.fetchTracks(lctx, am, l.Ctx)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		if w := dl.why(err); w != "" {
			dl.stall(nil, w)
			return false
		}
		// Gone from the library, or Apple won't list it: Download All goes on without it.
	}
	dl.mu.Lock()
	if len(dl.bulk.Lists) > 0 && dl.bulk.Lists[0] == l {
		dl.bulk.Lists = dl.bulk.Lists[1:]
	}
	if len(dl.bulk.Lists) == 0 {
		dl.bulk.Total = 0
	}
	dl.mu.Unlock()
	dl.saveBulk()
	dl.add(ts)
	dl.pause(ctx)
	return true
}

// pause waits gap, or until ctx ends.
func (dl *downloads) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(gap):
	}
}

// next downloads the first waiting song; false when there is none, or the queue stalled.
func (dl *downloads) next(ctx context.Context) bool {
	dl.mu.Lock()
	if dl.cur != nil || len(dl.waiting) == 0 || ctx.Err() != nil {
		dl.mu.Unlock()
		return false
	}
	it := dl.waiting[0]
	dl.waiting = dl.waiting[1:]
	dl.cur, dl.dropped = it, false
	one, cancel := context.WithCancel(ctx)
	dl.cancel = cancel
	dl.mu.Unlock()
	defer cancel()
	am, why := dl.ready()
	if am == nil {
		dl.stall(it, why)
		return false
	}
	if it.Tries >= maxTries { // under way each time q2-applemusic died: not again by itself
		it.Err, it.Tries = "it stopped Apple Music each time", 0
		dl.mu.Lock()
		dl.failed = append(dl.failed, it)
		dl.cur, dl.cancel = nil, nil
		dl.mu.Unlock()
		dl.save()
		return true
	}
	// Counted on the card first, so a death under way counts; taken back once it returns.
	dl.mu.Lock()
	it.Tries++
	dl.mu.Unlock()
	dl.write()
	path, err := dl.d.fetchSong(one, am, it.Track, dl.place)
	dl.mu.Lock()
	it.Tries--
	dropped := dl.dropped
	dl.mu.Unlock()
	switch {
	case err == nil:
		dl.mu.Lock()
		it.Path = path
		dl.have[key(it.Track)] = path
		dl.ids[it.Track.ID] = path
		dl.done = append(dl.done, it)
		if len(dl.done) > 200 {
			dl.done = dl.done[len(dl.done)-200:]
		}
		dl.cur, dl.cancel = nil, nil
		more := len(dl.waiting) > 0
		dl.mu.Unlock()
		if more {
			dl.save()
			dl.pause(ctx)
		}
	case dropped:
		dl.mu.Lock()
		dl.cur, dl.cancel = nil, nil
		dl.mu.Unlock()
	case ctx.Err() != nil: // stopping: it carries on next time
		dl.stall(it, "")
		return false
	case dl.why(err) != "":
		dl.stall(it, dl.why(err))
		return false
	default:
		it.Err = dl.reason(err, am)
		dl.mu.Lock()
		dl.failed = append(dl.failed, it)
		dl.cur, dl.cancel = nil, nil
		dl.mu.Unlock()
	}
	dl.save()
	return true
}

// reason is a failure as the Q2 shows it.
func (dl *downloads) reason(err error, am *apple.Client) string {
	switch {
	case errors.Is(err, apple.ErrUnavailable):
		if a := am.Known(); a != nil {
			return "not available in your Apple Music country (" + strings.ToUpper(a.Storefront) + ")"
		}
		return "not available here"
	case os.IsPermission(err) || errors.Is(err, os.ErrPermission):
		return "the card refused it"
	}
	return cut(err.Error(), 60)
}

// place is where a song downloads, as what, with its tags (m the playback service's): a
// playlist's in the playlist's album; else its album's folder, or Apple Music/ itself with
// Organize Files off.
func (dl *downloads) place(t apple.Track, m apple.Meta) (string, string, *mp4.Tags) {
	tags := tagsOf(t, m)
	if l := t.List; l != nil {
		dir, album := dl.playlistDir(l)
		tags.Album, tags.AlbumArtist, tags.Compilation = album, playlistsDir, true
		tags.Track, tags.Tracks, tags.Disc, tags.Discs = l.Pos, l.Count, 0, 0
		return dir, songName(t.ID, tags.Title, l.Pos, l.Count, 0, 0), tags
	}
	if !dl.d.organized() {
		return dl.folder, flatName(t.ID, tags.Title, tags.Artist), tags
	}
	year := m.Year()
	if year == "" {
		year = t.Year
	}
	dir := filepath.Join(dl.folder, folderName(tags.AlbumArtist, "Unknown Artist"),
		albumFolderName(tags.Album, year, "Unknown Album"))
	return dir, songName(t.ID, tags.Title, tags.Track, tags.Tracks, tags.Disc, tags.Discs), tags
}

// tagsOf is a song's tags: the playback service's, the list's where it said nothing.
func tagsOf(t apple.Track, m apple.Meta) *mp4.Tags {
	or := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	tags := &mp4.Tags{Title: or(m.Title, t.Name), Artist: or(m.Artist, t.Artist), Album: or(m.Album, t.Album),
		AlbumArtist: or(m.AlbumArtist, t.Artist), Genre: m.Genre, Composer: m.Composer, Copyright: m.Copyright,
		Date: or(m.Date, t.Year), Track: m.Track, Tracks: m.Tracks, Disc: m.Disc, Discs: m.Discs,
		Compilation: m.Compilation, Explicit: m.Explicit, Clean: m.Clean}
	if tags.Track == 0 {
		tags.Track = t.Number
	}
	if tags.Disc == 0 {
		tags.Disc = t.Disc
	}
	var id uint32
	for _, c := range t.ID {
		id = id*10 + uint32(c-'0')
	}
	tags.ID = id
	return tags
}

// playlistDir is a playlist's album folder and the album's name: the one it already has (its
// marker names it, though the playlist was renamed since), else Playlists/<name>, numbered when
// another playlist of that name has it.
func (dl *downloads) playlistDir(l *apple.ListRef) (string, string) {
	root := filepath.Join(dl.folder, playlistsDir)
	des, _ := os.ReadDir(root)
	for _, de := range des {
		if de.IsDir() {
			if id, name := readMarker(filepath.Join(root, de.Name())); id == l.ID {
				return filepath.Join(root, de.Name()), firstNonEmpty(name, l.Name)
			}
		}
	}
	base := folderName(l.Name, "Playlist")
	dir := filepath.Join(root, base)
	for n := 2; ; n++ { // a folder of that name that isn't this playlist's: the next number
		_, err := os.Stat(dir)
		if id, _ := readMarker(dir); err != nil || id == l.ID || (id == "" && empty(dir)) {
			break
		}
		dir = filepath.Join(root, base+" ("+itoa(n)+")")
	}
	return dir, l.Name
}

func empty(dir string) bool {
	des, err := os.ReadDir(dir)
	return err == nil && len(des) == 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// snapshot is the queue as the Q2's page shows it.
func (dl *downloads) snapshot() (cur *item, waiting, failed, done []item, stalled string) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.cur != nil {
		c := *dl.cur
		cur = &c
	}
	for _, it := range dl.waiting {
		waiting = append(waiting, *it)
	}
	for _, it := range dl.failed {
		failed = append(failed, *it)
	}
	for i := len(dl.done) - 1; i >= 0; i-- {
		done = append(done, *dl.done[i])
	}
	return cur, waiting, failed, done, dl.stalled
}
