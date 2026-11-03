package mp4

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/group-a-study/q2-applemusic/internal/mp4/mp4test"
)

func TestWrite(t *testing.T) {
	var samples [][]byte
	var plain []byte
	for i := 0; i < 100; i++ {
		s := make([]byte, 200+i*7)
		rand.Read(s)
		samples = append(samples, s)
		plain = append(plain, s...)
	}
	keys := [][]byte{[]byte("prefetch-key-016"), []byte("the-song-key-016")}
	kids := [][]byte{[]byte("\x00\x00\x00\x00\x00\x00\x00\x00s1/e1   "), bytes.Repeat([]byte{0xab}, 16)}
	in := mp4test.Fragmented(samples, keys, kids, 30, 2112, mp4test.ESDS)
	st, err := Read(bytes.NewReader(in), int64(len(in)))
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples() != 100 || !bytes.Equal(st.KeyIDs()[1], kids[1]) {
		t.Fatalf("%d samples, key ids %x", st.Samples(), st.KeyIDs())
	}
	tags := &Tags{Title: "Tears in the Typing Pool", Artist: "Broadcast", AlbumArtist: "Broadcast", Album: "Tender Buttons",
		Date: "2005-09-19", Track: 1, Tracks: 14, Disc: 1, Discs: 1, Lyrics: "[00:01.00]A line\n", Cover: []byte("\xff\xd8\xffjpeg"), ID: 12345}
	var out bytes.Buffer
	if err := st.Write(&out, bytes.NewReader(in), map[int][]byte{0: keys[0], 1: keys[1]}, tags); err != nil {
		t.Fatal(err)
	}
	b := out.Bytes()
	if string(child(b, "ftyp")[:4]) != "M4A " {
		t.Fatal("ftyp")
	}
	moov := child(b, "moov")
	stbl := path(moov, "trak", "mdia", "minf", "stbl")
	stsd := child(stbl, "stsd")
	if u32(stsd[4:]) != 1 || string(stsd[12:16]) != "mp4a" || bytes.Contains(stsd, []byte("sinf")) || !bytes.Contains(stsd, mp4test.ESDS) {
		t.Fatalf("stsd %x", stsd)
	}
	stsz := child(stbl, "stsz")
	if u32(stsz[8:]) != 100 || u32(stsz[12:]) != 200 || u32(stsz[12+4*99:]) != 200+99*7 {
		t.Fatal("stsz")
	}
	if stts := child(stbl, "stts"); u32(stts[4:]) != 1 || u32(stts[8:]) != 100 || u32(stts[12:]) != 1024 {
		t.Fatalf("stts %x", stts)
	}
	// 100 samples: chunks of 44, 44 and 12, each at its first sample's offset.
	stco, stsc := child(stbl, "stco"), child(stbl, "stsc")
	if u32(stco[4:]) != 3 || u32(stsc[4:]) != 2 || u32(stsc[8+12:]) != 3 || u32(stsc[8+16:]) != 12 {
		t.Fatalf("stco %x stsc %x", stco, stsc)
	}
	off := u32(stco[8:])
	if !bytes.Equal(b[off:], plain) || string(b[off-4:off]) != "mdat" {
		t.Fatal("the samples aren't the plain ones, at stco's offset")
	}
	var at uint32
	for i := 0; i < 44; i++ {
		at += uint32(200 + i*7)
	}
	if u32(stco[12:]) != off+at {
		t.Fatalf("second chunk at %d, want %d", u32(stco[12:]), off+at)
	}
	if st.SampleRate() != 44100 {
		t.Fatalf("sample rate %d", st.SampleRate())
	}
	// 102400 samples at 44.1 kHz, less the 2112 primed: the edit's length in the movie's 600.
	if elst := path(moov, "trak", "edts", "elst"); u32(elst[8:]) != (102400-2112)*600/44100 || u32(elst[12:]) != 2112 {
		t.Fatalf("elst %x", elst)
	}
	if d := u32(child(moov, "mvhd")[16:]); d != 102400*600/44100 {
		t.Fatalf("mvhd duration %d", d)
	}
	if d := u32(path(moov, "trak", "mdia", "mdhd")[16:]); d != 102400 {
		t.Fatalf("mdhd duration %d", d)
	}
	got := ReadTags(b)
	for k, v := range map[string]string{"\xa9nam": tags.Title, "\xa9ART": "Broadcast", "aART": "Broadcast",
		"\xa9alb": "Tender Buttons", "\xa9day": "2005-09-19", "trkn": "1/14", "disk": "1/1", "\xa9lyr": tags.Lyrics,
		"covr": "7 bytes", "cnID": "12345", "stik": "1"} {
		if got[k] != v {
			t.Errorf("tag %q: %q, want %q", k, got[k], v)
		}
	}

	// A wrong key is caught by nothing here, but a missing one is.
	if err := st.Write(&out, bytes.NewReader(in), map[int][]byte{1: keys[1]}, nil); err == nil {
		t.Fatal("wrote without the prefetch key")
	}
}

func TestNotFragmented(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("garbage!"), mkbox("ftyp", []byte("M4A ")), mkbox("moov", mkbox("mvhd", make([]byte, 96)))} {
		if _, err := Read(bytes.NewReader(b), int64(len(b))); err == nil {
			t.Errorf("%x read", b)
		}
	}
}

// TestFFmpeg checks the M4A against a real encoder's: ffmpeg encodes a tone to AAC, fragmented;
// its frames, encrypted as Apple's are (ffmpeg's own CENC leaves fragments without their IVs),
// make an M4A that ffmpeg decodes to the same audio as its own file, with the tags it reads.
func TestFFmpeg(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	run := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command(ff, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).Output()
		if err != nil {
			t.Fatalf("ffmpeg %v: %v", args, err)
		}
		return out
	}
	src := []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=44100", "-ac", "2", "-c:a", "aac", "-b:a", "128k"}
	run(append(src, "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-frag_duration", "500000",
		filepath.Join(dir, "frag.mp4"))...)
	frag, _ := os.ReadFile(filepath.Join(dir, "frag.mp4"))
	clear, err := Read(bytes.NewReader(frag), int64(len(frag)))
	if err != nil {
		t.Fatal(err)
	}
	var samples [][]byte
	for _, s := range clear.samples {
		samples = append(samples, frag[s.off:s.off+int64(s.size)])
	}
	entry := clear.tr.entries[0].plain
	var esds []byte
	each(entry[audioEntryHeader(entry):], func(typ string, _, whole []byte) {
		if typ == "esds" {
			esds = whole
		}
	})
	keys := [][]byte{[]byte("prefetch-key-016"), []byte("the-song-key-016")}
	in := mp4test.Fragmented(samples, keys, [][]byte{make([]byte, 16), bytes.Repeat([]byte{1}, 16)}, 20, 0, esds)
	st, err := Read(bytes.NewReader(in), int64(len(in)))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Create(filepath.Join(dir, "out.m4a"))
	if err := st.Write(f, bytes.NewReader(in), map[int][]byte{0: keys[0], 1: keys[1]}, &Tags{Title: "Tone", Lyrics: "[00:00.50]Hum"}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	pcm := func(name string) [16]byte {
		return md5.Sum(run("-i", filepath.Join(dir, name), "-f", "s16le", "-"))
	}
	if pcm("out.m4a") != pcm("frag.mp4") {
		t.Fatal("the decrypted M4A doesn't decode to ffmpeg's own file's audio")
	}
	meta := string(run("-i", filepath.Join(dir, "out.m4a"), "-f", "ffmetadata", "-"))
	if !bytes.Contains([]byte(meta), []byte("title=Tone")) || !bytes.Contains([]byte(meta), []byte("lyrics=[00:00.50]Hum")) {
		t.Fatalf("ffmpeg reads the tags as %q", meta)
	}
}
