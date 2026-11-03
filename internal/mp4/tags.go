package mp4

import (
	"bytes"
	"encoding/binary"
)

// Tags are a song's iTunes metadata atoms (moov/udta/meta/ilst), as TagLib reads them: the Q2's
// library, Now Playing (lyrics included) and Navidrome take them from there.
type Tags struct {
	Title, Artist, AlbumArtist, Album, Genre, Composer, Copyright string
	Date                                                          string // "2005-03-14", or a year
	Track, Tracks, Disc, Discs                                    int
	Compilation, Explicit, Clean                                  bool
	Lyrics                                                        string
	Cover                                                         []byte // JPEG or PNG
	ID                                                            uint32 // Apple's song id (cnID)
}

// data atom types.
const (
	dataImplicit = 0
	dataUTF8     = 1
	dataJPEG     = 13
	dataPNG      = 14
	dataInt      = 21
)

func item(name string, typ uint32, v []byte) []byte {
	d := make([]byte, 8, 8+len(v))
	binary.BigEndian.PutUint32(d, typ)
	return mkbox(name, mkbox("data", append(d, v...)))
}

func (t *Tags) udta() []byte {
	var ilst [][]byte
	text := func(name, v string) {
		if v != "" {
			ilst = append(ilst, item(name, dataUTF8, []byte(v)))
		}
	}
	pair := func(name string, n, of int, tail int) {
		if n <= 0 {
			return
		}
		b := make([]byte, 6+tail)
		binary.BigEndian.PutUint16(b[2:], uint16(n))
		binary.BigEndian.PutUint16(b[4:], uint16(max(of, 0)))
		ilst = append(ilst, item(name, dataImplicit, b))
	}
	text("\xa9nam", t.Title)
	text("\xa9ART", t.Artist)
	text("aART", t.AlbumArtist)
	text("\xa9alb", t.Album)
	text("\xa9gen", t.Genre)
	text("\xa9day", t.Date)
	text("\xa9wrt", t.Composer)
	pair("trkn", t.Track, t.Tracks, 2)
	pair("disk", t.Disc, t.Discs, 0)
	if t.Compilation {
		ilst = append(ilst, item("cpil", dataInt, []byte{1}))
	}
	switch {
	case t.Explicit:
		ilst = append(ilst, item("rtng", dataInt, []byte{1}))
	case t.Clean:
		ilst = append(ilst, item("rtng", dataInt, []byte{2}))
	}
	text("cprt", t.Copyright)
	text("\xa9lyr", t.Lyrics)
	if t.ID != 0 {
		ilst = append(ilst, item("cnID", dataInt, binary.BigEndian.AppendUint32(nil, t.ID)))
	}
	ilst = append(ilst, item("stik", dataInt, []byte{1})) // music
	if len(t.Cover) > 0 {
		typ := uint32(dataJPEG)
		if bytes.HasPrefix(t.Cover, []byte("\x89PNG")) {
			typ = dataPNG
		}
		ilst = append(ilst, item("covr", typ, t.Cover))
	}
	// The metadata handler, as iTunes writes it: mdir, by appl.
	hdlr := fullbox("hdlr", 0, 0, be32(0), []byte("mdirappl"), make([]byte, 9))
	return mkbox("udta", fullbox("meta", 0, 0, hdlr, mkbox("ilst", ilst...)))
}

// ReadTags reads the tags of an M4A Write made (for tests and checks): text atoms by name, and the
// track and disc numbers.
func ReadTags(file []byte) map[string]string {
	out := map[string]string{}
	moov := child(file, "moov")
	meta := path(moov, "udta", "meta")
	if len(meta) < 4 {
		return out
	}
	each(child(meta[4:], "ilst"), func(name string, p, _ []byte) {
		d := child(p, "data")
		if len(d) < 8 {
			return
		}
		v := d[8:]
		switch name {
		case "trkn", "disk":
			if len(v) >= 6 {
				out[name] = itoa(int(u16(v[2:]))) + "/" + itoa(int(u16(v[4:])))
			}
		case "covr":
			out[name] = itoa(len(v)) + " bytes"
		case "cpil", "rtng", "stik":
			if len(v) > 0 {
				out[name] = itoa(int(v[0]))
			}
		case "cnID":
			if len(v) == 4 {
				out[name] = itoa(int(u32(v)))
			}
		default:
			out[name] = string(v)
		}
	})
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
