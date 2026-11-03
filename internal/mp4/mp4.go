// Package mp4 turns Apple Music's web AAC, a fragmented MP4 whose samples are encrypted with
// CENC (AES-CTR), into a plain M4A the Q2 plays and tags: each sample decrypted with its sample
// description's key, the samples written into one mdat after a moov that indexes them, and the
// song's tags (iTunes atoms) in that moov, as gamdl's ammuxer does.
package mp4

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrFormat is a file that isn't the fragmented MP4 expected.
var ErrFormat = errors.New("mp4: not a fragmented MP4 audio stream")

func u32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func u16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

// boxAt reads the header of the box at b's start: its type, the header's length and the whole
// box's; size 0 runs to the end.
func boxAt(b []byte) (typ string, hdr, size int, ok bool) {
	if len(b) < 8 {
		return "", 0, 0, false
	}
	size, hdr, typ = int(u32(b)), 8, string(b[4:8])
	switch size {
	case 0:
		size = len(b)
	case 1:
		if len(b) < 16 {
			return "", 0, 0, false
		}
		big := binary.BigEndian.Uint64(b[8:])
		if big > uint64(len(b)) {
			return "", 0, 0, false
		}
		size, hdr = int(big), 16
	}
	if size < hdr || size > len(b) {
		return "", 0, 0, false
	}
	return typ, hdr, size, true
}

// child is the payload of the first box of type typ among the boxes in b.
func child(b []byte, typ string) []byte {
	for len(b) > 0 {
		t, hdr, size, ok := boxAt(b)
		if !ok {
			return nil
		}
		if t == typ {
			return b[hdr:size]
		}
		b = b[size:]
	}
	return nil
}

// path is the payload at the end of a path of boxes from b's.
func path(b []byte, types ...string) []byte {
	for _, t := range types {
		if b = child(b, t); b == nil {
			return nil
		}
	}
	return b
}

// each calls fn with each box in b: its type, its payload and the whole box.
func each(b []byte, fn func(typ string, payload, whole []byte)) {
	for len(b) > 0 {
		t, hdr, size, ok := boxAt(b)
		if !ok {
			return
		}
		fn(t, b[hdr:size], b[:size])
		b = b[size:]
	}
}

// encryption is a sample description's protection: its key id and per-sample IV size (tenc).
type encryption struct {
	kid    []byte
	ivSize int
	iv     []byte // a constant IV, for a zero IV size
}

// entry is a sample description, as written: encrypted ones as their original format, no sinf.
type entry struct {
	plain []byte
	enc   *encryption
}

// track is the audio track of a fragmented file's moov.
type track struct {
	moov      []byte // the moov's payload
	trak      []byte // the audio trak's payload
	id        uint32
	timescale uint32
	entries   []entry
	defDesc   uint32 // trex's defaults
	defDur    uint32
	defSize   uint32
}

func parseTrack(moov []byte) (*track, error) {
	tr := &track{moov: moov}
	each(moov, func(typ string, p, _ []byte) {
		if h := path(p, "mdia", "hdlr"); typ == "trak" && tr.trak == nil && len(h) >= 12 && string(h[8:12]) == "soun" {
			tr.trak = p
		}
	})
	if tr.trak == nil {
		return nil, ErrFormat
	}
	tkhd := child(tr.trak, "tkhd")
	mdhd := path(tr.trak, "mdia", "mdhd")
	stsd := path(tr.trak, "mdia", "minf", "stbl", "stsd")
	if len(tkhd) < 24 || len(mdhd) < 24 || len(stsd) < 8 {
		return nil, ErrFormat
	}
	if tkhd[0] == 1 {
		tr.id = u32(tkhd[20:])
	} else {
		tr.id = u32(tkhd[12:])
	}
	if mdhd[0] == 1 {
		tr.timescale = u32(mdhd[20:])
	} else {
		tr.timescale = u32(mdhd[12:])
	}
	each(stsd[8:], func(typ string, p, whole []byte) { tr.entries = append(tr.entries, cleanEntry(whole)) })
	if len(tr.entries) == 0 || tr.timescale == 0 {
		return nil, ErrFormat
	}
	tr.defDesc = 1
	each(child(moov, "mvex"), func(typ string, p, _ []byte) {
		if typ == "trex" && len(p) >= 24 && u32(p[4:]) == tr.id {
			tr.defDesc, tr.defDur, tr.defSize = u32(p[8:]), u32(p[12:]), u32(p[16:])
		}
	})
	return tr, nil
}

// audioEntryHeader is the length of an audio sample entry's fixed part, before its boxes: 28 bytes
// after the box header, and QuickTime's 16 or 36 more for a version 1 or 2 entry.
func audioEntryHeader(whole []byte) int {
	n := 8 + 28
	if len(whole) >= 18 {
		switch u16(whole[16:]) {
		case 1:
			n += 16
		case 2:
			n += 36
		}
	}
	return n
}

// cleanEntry is a sample entry with its protection taken out: an enca as its sinf's original
// format (frma), the sinf dropped, its tenc kept aside.
func cleanEntry(whole []byte) entry {
	audioEntryHeader := audioEntryHeader(whole)
	if len(whole) < audioEntryHeader {
		return entry{plain: whole}
	}
	var e entry
	format := whole[4:8]
	var kids []byte
	each(whole[audioEntryHeader:], func(typ string, p, w []byte) {
		if typ != "sinf" {
			kids = append(kids, w...)
			return
		}
		if f := child(p, "frma"); len(f) >= 4 {
			format = f[:4]
		} else if string(format) == "enca" {
			format = []byte("mp4a")
		}
		if t := path(p, "schi", "tenc"); len(t) >= 24 {
			enc := &encryption{ivSize: int(t[7]), kid: append([]byte(nil), t[8:24]...)}
			if enc.ivSize == 0 && len(t) > 24 && len(t) >= 25+int(t[24]) {
				enc.iv = append([]byte(nil), t[25:25+int(t[24])]...)
			}
			e.enc = enc
		}
	})
	out := make([]byte, 0, audioEntryHeader+len(kids))
	out = append(out, whole[:4]...)
	out = append(out, format...)
	out = append(out, whole[8:audioEntryHeader]...)
	out = append(out, kids...)
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	e.plain = out
	return e
}

// sample is one sample of the input: where it is, its size and duration, its description (from
// 0) and its IV, and, for the rare encrypted sample with clear parts, its subsamples.
type sample struct {
	off  int64
	size uint32
	dur  uint32
	desc uint16
	iv   [16]byte
	subs []subsample
}

type subsample struct{ clear, enc uint32 }

// fragment reads a moof (at off in the file) for the track's samples.
func (tr *track) fragment(moof []byte, off int64) ([]sample, error) {
	var out []sample
	var err error
	each(moof, func(typ string, traf, _ []byte) {
		if typ != "traf" || err != nil {
			return
		}
		tfhd := child(traf, "tfhd")
		if len(tfhd) < 8 || u32(tfhd[4:]) != tr.id {
			return
		}
		flags := u32(tfhd) & 0xffffff
		base, desc, dur, size := off, tr.defDesc, tr.defDur, tr.defSize
		p := 8
		take := func() uint32 {
			if p+4 > len(tfhd) {
				err = ErrFormat
				return 0
			}
			p += 4
			return u32(tfhd[p-4:])
		}
		if flags&1 != 0 {
			if p+8 > len(tfhd) {
				err = ErrFormat
				return
			}
			base = int64(binary.BigEndian.Uint64(tfhd[p:]))
			p += 8
		}
		if flags&2 != 0 {
			desc = take()
		}
		if flags&8 != 0 {
			dur = take()
		}
		if flags&0x10 != 0 {
			size = take()
		}
		if desc == 0 || int(desc) > len(tr.entries) {
			err = ErrFormat
			return
		}
		first := len(out)
		pos := base
		each(traf, func(typ string, trun, _ []byte) {
			if typ != "trun" || err != nil {
				return
			}
			if len(trun) < 8 {
				err = ErrFormat
				return
			}
			f := u32(trun) & 0xffffff
			n := int(u32(trun[4:]))
			q := 8
			if f&1 != 0 {
				if q+4 > len(trun) {
					err = ErrFormat
					return
				}
				pos = base + int64(int32(u32(trun[q:])))
				q += 4
			}
			if f&4 != 0 {
				q += 4
			}
			per := 0
			for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
				if f&bit != 0 {
					per += 4
				}
			}
			if n < 0 || q+n*per > len(trun) {
				err = ErrFormat
				return
			}
			for i := 0; i < n; i++ {
				s := sample{off: pos, size: size, dur: dur, desc: uint16(desc - 1)}
				if f&0x100 != 0 {
					s.dur = u32(trun[q:])
					q += 4
				}
				if f&0x200 != 0 {
					s.size = u32(trun[q:])
					q += 4
				}
				if f&0x400 != 0 {
					q += 4
				}
				if f&0x800 != 0 {
					q += 4
				}
				pos += int64(s.size)
				out = append(out, s)
			}
		})
		if err == nil {
			err = tr.ivs(traf, out[first:], tr.entries[desc-1].enc)
		}
	})
	return out, err
}

// ivs reads a traf's senc into its samples: each one's IV (the tenc's size, or a constant IV) and
// subsamples.
func (tr *track) ivs(traf []byte, ss []sample, enc *encryption) error {
	if enc == nil {
		return nil
	}
	senc := child(traf, "senc")
	if senc == nil {
		for i := range ss {
			copy(ss[i].iv[:], enc.iv)
		}
		return nil
	}
	if len(senc) < 8 {
		return ErrFormat
	}
	flags := u32(senc) & 0xffffff
	n := int(u32(senc[4:]))
	p := 8
	ivSize := enc.ivSize
	if flags&1 != 0 { // its own AlgorithmID, IV size and key id
		if len(senc) < p+20 {
			return ErrFormat
		}
		ivSize = int(senc[p+3])
		p += 20
	}
	if ivSize == 0 && flags&2 == 0 && n > 0 && (len(senc)-p)%n == 0 {
		ivSize = (len(senc) - p) / n
	}
	if n != len(ss) || ivSize > 16 {
		return ErrFormat
	}
	for i := range ss {
		if p+ivSize > len(senc) {
			return ErrFormat
		}
		if ivSize == 0 {
			copy(ss[i].iv[:], enc.iv)
		}
		copy(ss[i].iv[:], senc[p:p+ivSize])
		p += ivSize
		if flags&2 != 0 {
			if p+2 > len(senc) {
				return ErrFormat
			}
			k := int(u16(senc[p:]))
			p += 2
			if p+6*k > len(senc) {
				return ErrFormat
			}
			for j := 0; j < k; j++ {
				ss[i].subs = append(ss[i].subs, subsample{uint32(u16(senc[p:])), u32(senc[p+2:])})
				p += 6
			}
		}
	}
	return nil
}

// Stream is a fragmented MP4 read: its audio track and every sample's place in the file.
type Stream struct {
	tr      *track
	samples []sample
}

// Read indexes a fragmented MP4 of size bytes: its moov, then each moof's samples. Only the
// boxes' headers, the moov and the moofs are read.
func Read(r io.ReaderAt, size int64) (*Stream, error) {
	var st Stream
	hdr := make([]byte, 16)
	for off := int64(0); off < size; {
		if size-off < 8 {
			break
		}
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return nil, err
		}
		n, h := int64(u32(hdr)), int64(8)
		typ := string(hdr[4:8])
		if n == 1 {
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return nil, err
			}
			n, h = int64(binary.BigEndian.Uint64(hdr[8:])), 16
		} else if n == 0 {
			n = size - off
		}
		if n < h || off+n > size {
			return nil, ErrFormat
		}
		if typ == "moov" || typ == "moof" {
			if n > 32<<20 {
				return nil, ErrFormat
			}
			b := make([]byte, n-h)
			if _, err := r.ReadAt(b, off+h); err != nil {
				return nil, err
			}
			if typ == "moov" {
				tr, err := parseTrack(b)
				if err != nil {
					return nil, err
				}
				st.tr = tr
			} else {
				if st.tr == nil {
					return nil, ErrFormat
				}
				ss, err := st.tr.fragment(b, off)
				if err != nil {
					return nil, err
				}
				st.samples = append(st.samples, ss...)
			}
		}
		off += n
	}
	if st.tr == nil || len(st.samples) == 0 {
		return nil, ErrFormat
	}
	for _, s := range st.samples {
		if s.off < 0 || s.off+int64(s.size) > size {
			return nil, ErrFormat
		}
	}
	return &st, nil
}

// KeyIDs are the key ids of the stream's sample descriptions, by index ("" for a clear one).
func (st *Stream) KeyIDs() [][]byte {
	out := make([][]byte, len(st.tr.entries))
	for i, e := range st.tr.entries {
		if e.enc != nil {
			out[i] = e.enc.kid
		}
	}
	return out
}

// Duration is the stream's length in its timescale, and that timescale.
func (st *Stream) Duration() (uint64, uint32) {
	var d uint64
	for _, s := range st.samples {
		d += uint64(s.dur)
	}
	return d, st.tr.timescale
}

// Write writes the stream as a plain M4A to w, reading its samples from r: each decrypted with the
// key of its description (keys by index; a description without one is clear), tags in its moov.
func (st *Stream) Write(w io.Writer, r io.ReaderAt, keys map[int][]byte, tags *Tags) error {
	ciphers := map[int]cipher.Block{}
	for i, e := range st.tr.entries {
		if e.enc == nil {
			continue
		}
		k, ok := keys[i]
		if !ok {
			return fmt.Errorf("mp4: no key for sample description %d", i+1)
		}
		b, err := aes.NewCipher(k)
		if err != nil {
			return err
		}
		ciphers[i] = b
	}
	ftyp := ftypM4A()
	moov, err := st.moov(tags, 0)
	if err != nil {
		return err
	}
	// The mdat follows the moov: its first sample's offset, now that the moov's size is known.
	if moov, err = st.moov(tags, uint32(len(ftyp)+len(moov)+8)); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 64<<10)
	bw.Write(ftyp)
	bw.Write(moov)
	var total int64
	for _, s := range st.samples {
		total += int64(s.size)
	}
	if total+8 > 1<<32-1 {
		return errors.New("mp4: too long for one mdat")
	}
	var mh [8]byte
	binary.BigEndian.PutUint32(mh[:], uint32(total+8))
	copy(mh[4:], "mdat")
	bw.Write(mh[:])
	in := &sequential{r: r}
	buf := make([]byte, 0, 4096)
	for _, s := range st.samples {
		if cap(buf) < int(s.size) {
			buf = make([]byte, 0, s.size)
		}
		b := buf[:s.size]
		if err := in.read(b, s.off); err != nil {
			return err
		}
		if c := ciphers[int(s.desc)]; c != nil {
			decrypt(c, s, b)
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// decrypt is CENC's AES-CTR on a sample: its IV a 128-bit counter, run over the encrypted
// bytes only (all of them without subsamples), as one stream.
func decrypt(c cipher.Block, s sample, b []byte) {
	ctr := cipher.NewCTR(c, s.iv[:])
	if len(s.subs) == 0 {
		ctr.XORKeyStream(b, b)
		return
	}
	p := 0
	for _, sub := range s.subs {
		p += int(sub.clear)
		end := min(p+int(sub.enc), len(b))
		if p < end {
			ctr.XORKeyStream(b[p:end], b[p:end])
		}
		p = end
	}
}

// sequential reads samples in file order through one buffer, seeking only past a gap.
type sequential struct {
	r   io.ReaderAt
	br  *bufio.Reader
	pos int64
}

func (q *sequential) read(b []byte, off int64) error {
	if q.br == nil || off != q.pos {
		q.br = bufio.NewReaderSize(io.NewSectionReader(q.r, off, 1<<62), 64<<10)
		q.pos = off
	}
	n, err := io.ReadFull(q.br, b)
	q.pos += int64(n)
	return err
}

func ftypM4A() []byte {
	return mkbox("ftyp", []byte("M4A \x00\x00\x00\x00M4A mp42isom\x00\x00\x00\x00"))
}

func mkbox(typ string, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	b := make([]byte, 8, n)
	binary.BigEndian.PutUint32(b, uint32(n))
	copy(b[4:], typ)
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

// fullbox is a box with a version and flags.
func fullbox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	vf := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return mkbox(typ, append([][]byte{vf}, payload...)...)
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// usedEntry is the sample description most samples use: the one the M4A keeps (Apple's are the
// same codec, only their keys differ).
func (st *Stream) usedEntry() []byte {
	count := make([]int, len(st.tr.entries))
	for _, s := range st.samples {
		count[s.desc]++
	}
	best := 0
	for i := range count {
		if count[i] > count[best] {
			best = i
		}
	}
	return st.tr.entries[best].plain
}

// chunkSamples is how many samples a chunk of the M4A holds: about a second of AAC, as iTunes
// writes them, so a player's index (an iPod's, the Q2's) never has to take a whole song as one.
const chunkSamples = 44

// SampleRate is the audio's sample rate, from its sample description (16.16 fixed point).
func (st *Stream) SampleRate() int {
	e := st.usedEntry()
	if len(e) < audioEntryHeader(e) || len(e) < 36 {
		return 0
	}
	return int(u16(e[32:]))
}

// moov is the M4A's moov: the input's mvhd, tkhd, mdhd, hdlr and smhd with the real duration, an
// edit list as the input's (the encoder's priming skipped, for gapless play), one sample
// description, the sample tables (chunks of chunkSamples from mdat), and the tags.
func (st *Stream) moov(tags *Tags, mdat uint32) ([]byte, error) {
	tr := st.tr
	dur, ts := st.Duration()
	mvhd := append([]byte(nil), child(tr.moov, "mvhd")...)
	tkhd := append([]byte(nil), child(tr.trak, "tkhd")...)
	mdia := child(tr.trak, "mdia")
	mdhd := append([]byte(nil), child(mdia, "mdhd")...)
	hdlr := child(mdia, "hdlr")
	minf := child(mdia, "minf")
	if len(mvhd) < 20 || len(tkhd) < 24 || len(mdhd) < 20 || hdlr == nil || minf == nil {
		return nil, ErrFormat
	}
	movieTS := u32(mvhd[12:])
	if mvhd[0] == 1 {
		movieTS = u32(mvhd[20:])
	}
	if movieTS == 0 {
		movieTS = ts
	}
	movieDur := dur * uint64(movieTS) / uint64(ts)
	setDuration(mvhd, 16, 24, movieDur)
	setDuration(tkhd, 20, 28, movieDur)
	setDuration(mdhd, 16, 24, dur)
	tkhd[3] |= 7 // enabled, in the movie and its preview
	smhd := child(minf, "smhd")
	if smhd == nil {
		smhd = make([]byte, 8)
	}
	dinf := child(minf, "dinf")
	if dinf == nil {
		dinf = fullbox("dref", 0, 0, be32(1), fullbox("url ", 0, 1))
	} else {
		dinf = append([]byte(nil), dinf...)
	}

	var stts []byte
	runs := 0
	for i := 0; i < len(st.samples); {
		j := i
		for j < len(st.samples) && st.samples[j].dur == st.samples[i].dur {
			j++
		}
		stts = append(stts, be32(uint32(j-i))...)
		stts = append(stts, be32(st.samples[i].dur)...)
		runs++
		i = j
	}
	stsz := make([]byte, 0, 8+4*len(st.samples))
	stsz = append(stsz, be32(0)...)
	stsz = append(stsz, be32(uint32(len(st.samples)))...)
	for _, s := range st.samples {
		stsz = append(stsz, be32(s.size)...)
	}
	// Chunks of chunkSamples, the last one shorter: two stsc runs at most, an stco entry each.
	chunks := (len(st.samples) + chunkSamples - 1) / chunkSamples
	stsc := append(be32(1), be32(chunkSamples)...)
	stsc = append(stsc, be32(1)...)
	runsc := uint32(1)
	if last := len(st.samples) - (chunks-1)*chunkSamples; last != chunkSamples {
		if chunks == 1 {
			stsc = append(append(be32(1), be32(uint32(last))...), be32(1)...)
		} else {
			stsc = append(stsc, be32(uint32(chunks))...)
			stsc = append(stsc, be32(uint32(last))...)
			stsc = append(stsc, be32(1)...)
			runsc = 2
		}
	}
	stco := make([]byte, 0, 4*chunks)
	off := mdat
	for i, s := range st.samples {
		if i%chunkSamples == 0 {
			stco = append(stco, be32(off)...)
		}
		off += s.size
	}
	stbl := mkbox("stbl",
		fullbox("stsd", 0, 0, be32(1), st.usedEntry()),
		fullbox("stts", 0, 0, be32(uint32(runs)), stts),
		fullbox("stsc", 0, 0, be32(runsc), stsc),
		fullbox("stsz", 0, 0, stsz),
		fullbox("stco", 0, 0, be32(uint32(chunks)), stco))
	trak := [][]byte{mkbox("tkhd", tkhd)}
	if e := st.edits(movieTS, movieDur); e != nil {
		trak = append(trak, e)
	}
	trak = append(trak, mkbox("mdia", mkbox("mdhd", mdhd), mkbox("hdlr", hdlr),
		mkbox("minf", mkbox("smhd", smhd), mkbox("dinf", dinf), stbl)))
	parts := [][]byte{mkbox("mvhd", mvhd), mkbox("trak", trak...)}
	if tags != nil {
		parts = append(parts, tags.udta())
	}
	return mkbox("moov", parts...), nil
}

// setDuration writes d at a version 0 box's offset v0 (32 bits) or a version 1's v1 (64 bits).
func setDuration(b []byte, v0, v1 int, d uint64) {
	if b[0] == 1 {
		if len(b) >= v1+8 {
			binary.BigEndian.PutUint64(b[v1:], d)
		}
		return
	}
	if d > 1<<32-1 {
		d = 1<<32 - 1
	}
	binary.BigEndian.PutUint32(b[v0:], uint32(d))
}

// edits is the input's edit list for the M4A: one edit that starts at a media time (the priming
// to skip), its length now the rest of the song. Anything else (none, empty edits) is left out.
func (st *Stream) edits(movieTS uint32, movieDur uint64) []byte {
	elst := path(st.tr.trak, "edts", "elst")
	if len(elst) < 8 || u32(elst[4:]) != 1 {
		return nil
	}
	var start int64
	if elst[0] == 1 {
		if len(elst) < 28 {
			return nil
		}
		start = int64(binary.BigEndian.Uint64(elst[16:]))
	} else {
		if len(elst) < 20 {
			return nil
		}
		start = int64(int32(u32(elst[12:])))
	}
	dur, ts := st.Duration()
	if start <= 0 || uint64(start) >= dur {
		return nil
	}
	seg := (dur - uint64(start)) * uint64(movieTS) / uint64(ts)
	if seg > 1<<32-1 || start > 1<<31-1 {
		return nil
	}
	return mkbox("edts", fullbox("elst", 0, 0, be32(1), be32(uint32(seg)), be32(uint32(start)), be32(1<<16)))
}

// Samples is how many samples the stream has (for tests and checks).
func (st *Stream) Samples() int { return len(st.samples) }
