// Package widevine is a Widevine CDM, as pywidevine does it and gamdl uses it for Apple Music's web
// AAC: a device's client identity and RSA key (a .wvd file) sign a license request, and the
// license server's reply gives the content keys, decrypted with the session key it sends wrapped
// in the device's key. One session per request; nothing is kept between them.
package widevine

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Device is a .wvd file's: pywidevine's format version 2, "WVD", its version, type, security
// level and flags, then the private key (PKCS#1 DER) and the client identification (a
// ClientIdentification message), each after its big-endian 16-bit length.
type Device struct {
	Type     byte // 1 Chrome, 2 Android
	Level    byte
	Key      *rsa.PrivateKey
	ClientID []byte
}

const (
	deviceChrome  = 1
	deviceAndroid = 2
)

// builtin is gamdl's built-in device (gamdl/interface/wvd.py, MIT): an Android 9 emulator, L3.
//
//go:embed android.wvd
var builtin []byte

// Builtin is the device the CDM uses unless the card has its own.
func Builtin() *Device {
	d, err := ParseWVD(builtin)
	if err != nil {
		panic(err)
	}
	return d
}

// ParseWVD reads a .wvd file (version 2; version 1, which has no flags byte, too).
func ParseWVD(b []byte) (*Device, error) {
	if len(b) < 8 || string(b[:3]) != "WVD" {
		return nil, errors.New("widevine: not a .wvd file")
	}
	d := &Device{Type: b[4], Level: b[5]}
	var rest []byte
	switch b[3] {
	case 1:
		rest = b[6:]
	case 2:
		rest = b[7:]
	default:
		return nil, fmt.Errorf("widevine: .wvd version %d", b[3])
	}
	next := func() ([]byte, error) {
		if len(rest) < 2 || len(rest) < 2+int(binary.BigEndian.Uint16(rest)) {
			return nil, errors.New("widevine: .wvd file cut short")
		}
		n := int(binary.BigEndian.Uint16(rest))
		v := rest[2 : 2+n]
		rest = rest[2+n:]
		return v, nil
	}
	der, err := next()
	if err != nil {
		return nil, err
	}
	if d.ClientID, err = next(); err != nil {
		return nil, err
	}
	if d.Key, err = x509.ParsePKCS1PrivateKey(der); err != nil {
		return nil, fmt.Errorf("widevine: the .wvd file's key: %w", err)
	}
	return d, nil
}

// Key is a content key from a license.
type Key struct {
	ID  []byte
	Key []byte
}

// Session is one license request and its reply.
type Session struct {
	dev *Device
	msg []byte // the signed LicenseRequest: the keys are derived from it
}

// NewSession starts a session on the device.
func (d *Device) NewSession() *Session { return &Session{dev: d} }

var sessions atomic.Uint64

// requestID is the license request's id. An Android CDM's (OEMCrypto's) is an AES-CTR counter
// block written as 32 upper-case hex digits: 4 random bytes, 4 zero bytes, the session's number.
func (s *Session) requestID() []byte {
	b := make([]byte, 16)
	if s.dev.Type != deviceAndroid {
		rand.Read(b)
		return b
	}
	rand.Read(b[:4])
	binary.LittleEndian.PutUint64(b[8:], sessions.Add(1))
	return []byte(strings.ToUpper(hex.EncodeToString(b)))
}

// Challenge is a license request for initData (a PSSH box's Widevine data): a SignedMessage
// holding a LicenseRequest for streaming with the device's client identification in the clear,
// signed with the device's key (RSASSA-PSS, SHA-1).
func (s *Session) Challenge(initData []byte) ([]byte, error) {
	var wp []byte
	wp = appendBytes(wp, wpPsshData, initData)
	wp = appendUint(wp, wpLicenseType, 1) // STREAMING
	wp = appendBytes(wp, wpRequestID, s.requestID())
	var nonce [4]byte
	rand.Read(nonce[:])
	var lr []byte
	lr = appendBytes(lr, lrClientID, s.dev.ClientID)
	lr = appendBytes(lr, lrContentID, appendBytes(nil, ciWidevinePsshData, wp))
	lr = appendUint(lr, lrType, 1) // NEW
	lr = appendUint(lr, lrRequestTime, uint64(time.Now().Unix()))
	lr = appendUint(lr, lrProtocolVersion, 21) // VERSION_2_1
	lr = appendUint(lr, lrKeyControlNonce, uint64(binary.BigEndian.Uint32(nonce[:])&0x7fffffff|1))
	h := sha1.Sum(lr)
	sig, err := rsa.SignPSS(rand.Reader, s.dev.Key, crypto.SHA1, h[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	if err != nil {
		return nil, err
	}
	s.msg = lr
	var sm []byte
	sm = appendUint(sm, smType, msgLicenseRequest)
	sm = appendBytes(sm, smMsg, lr)
	sm = appendBytes(sm, smSignature, sig)
	return sm, nil
}

// ErrNoKeys is a license that gave no content key.
var ErrNoKeys = errors.New("widevine: the license has no content key")

// Keys reads the license server's reply to the challenge: a SignedMessage holding a License, its
// session key wrapped with the device's key (RSA-OAEP, SHA-1). The keys derived from that and
// the request (AES-CMAC, as OEMCrypto derives them) check the reply's HMAC-SHA256 and unwrap each
// content key (AES-CBC, PKCS#7 padding).
func (s *Session) Keys(reply []byte) ([]Key, error) {
	if s.msg == nil {
		return nil, errors.New("widevine: no challenge in this session")
	}
	fs, err := fields(reply)
	if err != nil {
		return nil, err
	}
	var typ uint64
	var msg, sig, wrapped, core []byte
	for _, f := range fs {
		switch f.num {
		case smType:
			typ = f.v
		case smMsg:
			msg = f.b
		case smSignature:
			sig = f.b
		case smSessionKey:
			wrapped = f.b
		case smCoreMsg:
			core = f.b
		}
	}
	if typ != msgLicense {
		return nil, fmt.Errorf("widevine: the reply is message type %d, not a license", typ)
	}
	sessionKey, err := rsa.DecryptOAEP(sha1.New(), nil, s.dev.Key, wrapped, nil)
	if err != nil {
		return nil, fmt.Errorf("widevine: the session key: %w", err)
	}
	encKey, macKey, err := deriveKeys(sessionKey, s.msg)
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, macKey)
	m.Write(core)
	m.Write(msg)
	if !hmac.Equal(m.Sum(nil), sig) {
		return nil, errors.New("widevine: the license's signature doesn't match")
	}
	lfs, err := fields(msg)
	if err != nil {
		return nil, err
	}
	var keys []Key
	for _, f := range lfs {
		if f.num != licKey || f.wire != 2 {
			continue
		}
		kfs, err := fields(f.b)
		if err != nil {
			return nil, err
		}
		var k Key
		var iv, enc []byte
		var kt uint64
		for _, kf := range kfs {
			switch kf.num {
			case kcID:
				k.ID = kf.b
			case kcIV:
				iv = kf.b
			case kcKey:
				enc = kf.b
			case kcType:
				kt = kf.v
			}
		}
		if kt != keyContent {
			continue
		}
		if k.Key, err = unwrap(encKey, iv, enc); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}
	return keys, nil
}

// deriveKeys is OEMCrypto's: the encryption key AES-CMAC(session key, 1 || "ENCRYPTION" 0 request
// 128), the server's MAC key the CMACs for counters 1 and 2 of "AUTHENTICATION" 0 request 512.
func deriveKeys(sessionKey, request []byte) (enc, mac []byte, err error) {
	ctx := func(label string, bits uint32) []byte {
		b := append([]byte(label), 0)
		b = append(b, request...)
		return binary.BigEndian.AppendUint32(b, bits)
	}
	encCtx, macCtx := ctx("ENCRYPTION", 128), ctx("AUTHENTICATION", 512)
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, nil, fmt.Errorf("widevine: the session key: %w", err)
	}
	enc = cmac(block, append([]byte{1}, encCtx...))
	mac = append(cmac(block, append([]byte{1}, macCtx...)), cmac(block, append([]byte{2}, macCtx...))...)
	return enc, mac, nil
}

func unwrap(key, iv, enc []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != aes.BlockSize || len(enc) == 0 || len(enc)%aes.BlockSize != 0 {
		return nil, errors.New("widevine: a key container is malformed")
	}
	out := make([]byte, len(enc))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, enc)
	p := int(out[len(out)-1])
	if p == 0 || p > aes.BlockSize || !bytes.Equal(out[len(out)-p:], bytes.Repeat([]byte{byte(p)}, p)) {
		return nil, errors.New("widevine: a content key's padding is wrong")
	}
	return out[:len(out)-p], nil
}

// cmac is AES-CMAC (RFC 4493) of msg.
func cmac(block cipher.Block, msg []byte) []byte {
	const rb = 0x87
	dbl := func(in []byte) []byte {
		out := make([]byte, 16)
		carry := byte(0)
		for i := 15; i >= 0; i-- {
			out[i] = in[i]<<1 | carry
			carry = in[i] >> 7
		}
		if carry != 0 {
			out[15] ^= rb
		}
		return out
	}
	l := make([]byte, 16)
	block.Encrypt(l, l)
	k1 := dbl(l)
	k2 := dbl(k1)
	n := (len(msg) + 15) / 16
	last := make([]byte, 16)
	if n == 0 || len(msg)%16 != 0 {
		if n == 0 {
			n = 1
		}
		rest := msg[(n-1)*16:]
		copy(last, rest)
		last[len(rest)] = 0x80
		for i := range last {
			last[i] ^= k2[i]
		}
	} else {
		copy(last, msg[(n-1)*16:])
		for i := range last {
			last[i] ^= k1[i]
		}
	}
	x := make([]byte, 16)
	for i := 0; i < n-1; i++ {
		for j := 0; j < 16; j++ {
			x[j] ^= msg[i*16+j]
		}
		block.Encrypt(x, x)
	}
	for j := 0; j < 16; j++ {
		x[j] ^= last[j]
	}
	block.Encrypt(x, x)
	return x
}
