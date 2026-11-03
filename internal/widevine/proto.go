package widevine

import (
	"encoding/binary"
	"errors"
)

// The few protobuf messages of Widevine's license protocol (license_protocol.proto) the CDM needs,
// written and read by hand: field numbers and wire types, no generated code.

var errProto = errors.New("widevine: malformed protobuf")

func appendVarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

// appendUint is a varint field.
func appendUint(b []byte, field int, v uint64) []byte {
	return appendVarint(appendVarint(b, uint64(field)<<3), v)
}

// appendBytes is a length-delimited field: bytes, a string or a message.
func appendBytes(b []byte, field int, v []byte) []byte {
	b = appendVarint(b, uint64(field)<<3|2)
	b = appendVarint(b, uint64(len(v)))
	return append(b, v...)
}

// field is one field of a message as read: its varint, or its bytes for a length-delimited one.
type field struct {
	num  int
	wire int
	v    uint64
	b    []byte
}

// fields reads a message's fields in order; groups, which the protocol doesn't use, are refused.
func fields(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errProto
		}
		b = b[n:]
		f := field{num: int(key >> 3), wire: int(key & 7)}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errProto
			}
			f.v, b = v, b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errProto
			}
			f.v, b = binary.LittleEndian.Uint64(b), b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return nil, errProto
			}
			f.b, b = b[n:n+int(l)], b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, errProto
			}
			f.v, b = uint64(binary.LittleEndian.Uint32(b)), b[4:]
		default:
			return nil, errProto
		}
		out = append(out, f)
	}
	return out, nil
}

// SignedMessage (license_protocol.proto), as the CDM sends and reads it.
const (
	smType       = 1 // MessageType
	smMsg        = 2
	smSignature  = 3
	smSessionKey = 4
	smCoreMsg    = 9 // oemcrypto_core_message

	msgLicenseRequest = 1
	msgLicense        = 2
	msgError          = 3
)

// LicenseRequest.
const (
	lrClientID        = 1
	lrContentID       = 2
	lrType            = 3 // RequestType: NEW 1
	lrRequestTime     = 4
	lrProtocolVersion = 6 // VERSION_2_1 21
	lrKeyControlNonce = 7

	// LicenseRequest.ContentIdentification and its WidevinePsshData.
	ciWidevinePsshData = 1
	wpPsshData         = 1
	wpLicenseType      = 2 // STREAMING 1
	wpRequestID        = 3
)

// License, its LicenseIdentification and KeyContainers.
const (
	licID       = 1
	licKey      = 3
	idRequestID = 1
	kcID        = 1
	kcIV        = 2
	kcKey       = 3
	kcType      = 4 // KeyType: CONTENT 2

	keyContent = 2
)

// WidevinePsshData (a PSSH box's data for Widevine's system id).
const (
	psAlgorithm = 1 // AESCTR 1
	psKeyIDs    = 2
)
