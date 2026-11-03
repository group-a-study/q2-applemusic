// Package mp4test makes the fragmented, encrypted MP4 that Apple Music's web AAC is, for tests.
package mp4test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
)

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

func fullbox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	vf := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return mkbox(typ, append([][]byte{vf}, payload...)...)
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// ESDS is an AAC-LC, 44.1 kHz stereo decoder configuration.
var ESDS = fullbox("esds", 0, 0, []byte{3, 25, 0, 1, 0, 4, 17, 0x40, 0x15, 0, 0, 0, 0, 3, 0xe8, 0, 0, 3, 0xe8, 0, 5, 2, 0x12, 0x10, 6, 1, 2})

// encEntry is an enca sample entry protected with CENC under kid, its decoder configuration esds.
func encEntry(kid, esds []byte) []byte {
	body := make([]byte, 28)
	body[7] = 1                     // data reference index
	body[17] = 2                    // channels
	body[19] = 16                   // sample size
	body[24], body[25] = 0xac, 0x44 // 44100 << 16
	tenc := fullbox("tenc", 0, 0, []byte{0, 0, 1, 8}, kid)
	sinf := mkbox("sinf", mkbox("frma", []byte("mp4a")), fullbox("schm", 0, 0, []byte("cenc"), be32(0x10000)),
		mkbox("schi", tenc))
	return mkbox("enca", body, esds, sinf)
}

// Fragmented is a fragmented MP4 as Apple's web AAC is: two sample descriptions, each its own
// key, the first fragment under the first (the prefetch key), the rest under the second, with
// 8-byte IVs in senc, and an edit list skipping the encoder's priming.
func Fragmented(samples [][]byte, keys, kids [][]byte, perFrag int, priming uint32, esds []byte) []byte {
	tkhd := make([]byte, 80)
	tkhd[15] = 1 // track 1
	mvhd := make([]byte, 96)
	copy(mvhd[12:], be32(600))
	mdhd := make([]byte, 24)
	copy(mdhd[12:], be32(44100))
	elst := fullbox("elst", 0, 0, be32(1), be32(0), be32(priming), be32(1<<16))
	stsd := fullbox("stsd", 0, 0, be32(2), encEntry(kids[0], esds), encEntry(kids[1], esds))
	empty := func(typ string) []byte { return fullbox(typ, 0, 0, be32(0)) }
	stbl := mkbox("stbl", stsd, empty("stts"), empty("stsc"), fullbox("stsz", 0, 0, be32(0), be32(0)), empty("stco"))
	minf := mkbox("minf", fullbox("smhd", 0, 0, make([]byte, 4)), mkbox("dinf", fullbox("dref", 0, 0, be32(1), fullbox("url ", 0, 1))), stbl)
	hdlr := fullbox("hdlr", 0, 0, be32(0), []byte("soun"), make([]byte, 13))
	trak := mkbox("trak", mkbox("tkhd", tkhd), mkbox("edts", elst), mkbox("mdia", mkbox("mdhd", mdhd), hdlr, minf))
	trex := fullbox("trex", 0, 0, be32(1), be32(1), be32(1024), be32(0), be32(0))
	out := append(mkbox("ftyp", []byte("iso6\x00\x00\x00\x00iso6mp41")), mkbox("moov", mkbox("mvhd", mvhd), trak, mkbox("mvex", trex))...)
	for f, n := 0, 0; n < len(samples); f++ {
		desc := 2
		if f == 0 {
			desc = 1
		}
		group := samples[n:min(n+perFrag, len(samples))]
		n += len(group)
		block, _ := aes.NewCipher(keys[desc-1])
		var mdat, senc, sizes []byte
		for _, s := range group {
			iv := make([]byte, 16)
			rand.Read(iv[:8])
			enc := make([]byte, len(s))
			cipher.NewCTR(block, iv).XORKeyStream(enc, s)
			mdat = append(mdat, enc...)
			senc = append(senc, iv[:8]...)
			sizes = append(sizes, be32(uint32(len(s)))...)
		}
		build := func(dataOffset uint32) []byte {
			tfhd := fullbox("tfhd", 0, 0x20002, be32(1), be32(uint32(desc)))
			tfdt := fullbox("tfdt", 0, 0, be32(uint32(1024*(n-len(group)))))
			trun := fullbox("trun", 0, 0x201, be32(uint32(len(group))), be32(dataOffset), sizes)
			sencBox := fullbox("senc", 0, 0, be32(uint32(len(group))), senc)
			return mkbox("moof", fullbox("mfhd", 0, 0, be32(uint32(f+1))), mkbox("traf", tfhd, tfdt, trun, sencBox))
		}
		moof := build(0)
		moof = build(uint32(len(moof) + 8))
		out = append(out, moof...)
		out = append(out, mkbox("mdat", mdat)...)
	}
	return out
}
