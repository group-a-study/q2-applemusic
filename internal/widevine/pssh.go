package widevine

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
)

// SystemID is Widevine's DRM system id, a PSSH box's and an HLS key's KEYFORMAT
// (urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed).
var SystemID = []byte{0xed, 0xef, 0x8b, 0xa9, 0x79, 0xd6, 0x4a, 0xce, 0xa3, 0xc8, 0x27, 0xdc, 0xd5, 0x1d, 0x21, 0xed}

// InitData is the Widevine data a license request carries for an HLS key's URI, a data: URI as
// Apple's playlists give it (gamdl's reconstruct_pssh): a whole PSSH box, whose data it is, or
// just a 16-byte key id, which becomes a WidevinePsshData naming it (AES-CTR).
func InitData(uri string) ([]byte, error) {
	if i := strings.LastIndexByte(uri, ','); i >= 0 {
		uri = uri[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(uri)
	if err != nil {
		return nil, errors.New("widevine: the key's URI isn't base64")
	}
	if len(raw) <= 30 {
		var b []byte
		b = appendUint(b, psAlgorithm, 1)
		b = appendBytes(b, psKeyIDs, raw)
		return b, nil
	}
	return psshData(raw)
}

// psshData is a PSSH box's data: version 0 or 1 (with key ids), Widevine's system id.
func psshData(box []byte) ([]byte, error) {
	bad := errors.New("widevine: not a Widevine PSSH box")
	if len(box) < 32 || string(box[4:8]) != "pssh" {
		return nil, bad
	}
	size := int(binary.BigEndian.Uint32(box))
	if size < 32 || size > len(box) {
		return nil, bad
	}
	box = box[:size]
	if !bytes.Equal(box[12:28], SystemID) {
		return nil, bad
	}
	off := 28
	if box[8] >= 1 {
		n := int(binary.BigEndian.Uint32(box[off:]))
		off += 4 + 16*n
		if n > 64 || off+4 > len(box) {
			return nil, bad
		}
	}
	n := int(binary.BigEndian.Uint32(box[off:]))
	off += 4
	if n > len(box)-off {
		return nil, bad
	}
	return box[off : off+n], nil
}
