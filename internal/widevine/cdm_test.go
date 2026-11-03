package widevine

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func one(t *testing.T, fs []field, num int) field {
	t.Helper()
	for _, f := range fs {
		if f.num == num {
			return f
		}
	}
	t.Fatalf("no field %d", num)
	return field{}
}

func parse(t *testing.T, b []byte) []field {
	t.Helper()
	fs, err := fields(b)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func license(t *testing.T, dev *Device, challenge []byte, keys []Key) []byte {
	t.Helper()
	b, err := License(dev, challenge, keys)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLicense(t *testing.T) {
	dev := Builtin()
	if dev.Type != deviceAndroid || dev.Level != 3 || dev.Key.N.BitLen() != 2048 {
		t.Fatalf("builtin device: type %d level %d, %d-bit key", dev.Type, dev.Level, dev.Key.N.BitLen())
	}
	init, err := InitData("data:text/plain;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	s := dev.NewSession()
	ch, err := s.Challenge(init)
	if err != nil {
		t.Fatal(err)
	}
	want := Key{ID: bytes.Repeat([]byte{7}, 16), Key: []byte("0123456789abcdef")}
	keys, err := s.Keys(license(t, dev, ch, []Key{want}))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !bytes.Equal(keys[0].ID, want.ID) || !bytes.Equal(keys[0].Key, want.Key) {
		t.Fatalf("keys %x", keys)
	}

	// Another session's license is refused: its keys were derived from another request.
	other := dev.NewSession()
	ch2, _ := other.Challenge(init)
	if _, err := s.Keys(license(t, dev, ch2, []Key{want})); err == nil {
		t.Fatal("a license for another request was taken")
	}
}

func TestRequestID(t *testing.T) {
	id := Builtin().NewSession().requestID()
	if len(id) != 32 || string(id[8:16]) != "00000000" || bytes.ToUpper(id)[0] != id[0] {
		t.Fatalf("request id %s", id)
	}
	if _, err := hex.DecodeString(string(id)); err != nil {
		t.Fatal(err)
	}
}

// RFC 4493's examples.
func TestCMAC(t *testing.T) {
	key, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	msg, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710")
	block, _ := aes.NewCipher(key)
	for _, c := range []struct {
		n    int
		want string
	}{{0, "bb1d6929e95937287fa37d129b756746"}, {16, "070a16b46b4d4144f79bdd9dd04a287c"},
		{40, "dfa66747de9ae63030ca32611497c827"}, {64, "51f0bebf7e3b9d92fc49741779363cfe"}} {
		if got := hex.EncodeToString(cmac(block, msg[:c.n])); got != c.want {
			t.Errorf("CMAC of %d bytes: %s, want %s", c.n, got, c.want)
		}
	}
}

func TestInitData(t *testing.T) {
	// A whole box: the default one Apple's playlists list first (gamdl's DRM_DEFAULT_KEY_MAPPING).
	d, err := InitData("data:text/plain;base64,AAAAOHBzc2gAAAAA7e+LqXnWSs6jyCfc1R0h7QAAABgSEAAAAAAAAAAAczEvZTEgICBI88aJmwY=")
	if err != nil {
		t.Fatal(err)
	}
	fs := parse(t, d)
	if kid := one(t, fs, psKeyIDs).b; string(kid[8:]) != "s1/e1   " {
		t.Fatalf("key id %q", kid)
	}
	// A key id alone.
	d, err = InitData("data:;base64,AAECAwQFBgcICQoLDA0ODw==")
	if err != nil {
		t.Fatal(err)
	}
	fs = parse(t, d)
	if one(t, fs, psAlgorithm).v != 1 || !bytes.Equal(one(t, fs, psKeyIDs).b, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}) {
		t.Fatalf("init data %x", d)
	}
	if _, err := InitData("data:;base64,AAAAOHBzc2gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err == nil {
		t.Fatal("another system's box taken")
	}
}
