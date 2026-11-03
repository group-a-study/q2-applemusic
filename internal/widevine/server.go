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
	"errors"
	"fmt"
)

// find is a message's first field numbered num (a zero field without one).
func find(fs []field, num int) field {
	for _, f := range fs {
		if f.num == num {
			return f
		}
	}
	return field{}
}

// License answers a challenge as a license server does (for tests: the CDM never needs it): the request's signature checked with
// the device's public key, a session key wrapped for it, the content keys under the keys
// derived from that and the request, and the whole signed.
func License(dev *Device, challenge []byte, keys []Key) ([]byte, error) {
	sm, err := fields(challenge)
	if err != nil {
		return nil, err
	}
	if find(sm, smType).v != msgLicenseRequest {
		return nil, errors.New("widevine: not a license request")
	}
	req, sig := find(sm, smMsg).b, find(sm, smSignature).b
	h := sha1.Sum(req)
	if err := rsa.VerifyPSS(&dev.Key.PublicKey, crypto.SHA1, h[:], sig, &rsa.PSSOptions{SaltLength: 20}); err != nil {
		return nil, fmt.Errorf("widevine: the challenge's signature: %w", err)
	}
	lr, err := fields(req)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(find(lr, lrClientID).b, dev.ClientID) {
		return nil, errors.New("widevine: another device's client id")
	}
	ci, _ := fields(find(lr, lrContentID).b)
	wp, _ := fields(find(ci, ciWidevinePsshData).b)
	reqID := find(wp, wpRequestID).b

	sessionKey := make([]byte, 16)
	rand.Read(sessionKey)
	enc, mac, err := deriveKeys(sessionKey, req)
	if err != nil {
		return nil, err
	}
	var lic []byte
	lic = appendBytes(lic, licID, appendBytes(nil, idRequestID, reqID))
	block, _ := aes.NewCipher(enc)
	container := func(k Key, typ uint64) []byte {
		iv := make([]byte, 16)
		rand.Read(iv)
		p := 16 - len(k.Key)%16
		padded := append(append([]byte(nil), k.Key...), bytes.Repeat([]byte{byte(p)}, p)...)
		out := make([]byte, len(padded))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
		var kc []byte
		kc = appendBytes(kc, kcID, k.ID)
		kc = appendBytes(kc, kcIV, iv)
		kc = appendBytes(kc, kcKey, out)
		return appendUint(kc, kcType, typ)
	}
	lic = appendBytes(lic, licKey, container(Key{Key: bytes.Repeat([]byte{9}, 64)}, 1)) // SIGNING, skipped
	for _, k := range keys {
		lic = appendBytes(lic, licKey, container(k, keyContent))
	}
	core := []byte("core message")
	m := hmac.New(sha256.New, mac)
	m.Write(core)
	m.Write(lic)
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &dev.Key.PublicKey, sessionKey, nil)
	if err != nil {
		return nil, err
	}
	var reply []byte
	reply = appendUint(reply, smType, msgLicense)
	reply = appendBytes(reply, smMsg, lic)
	reply = appendBytes(reply, smSignature, m.Sum(nil))
	reply = appendBytes(reply, smSessionKey, wrapped)
	reply = appendBytes(reply, smCoreMsg, core)
	return reply, nil
}
