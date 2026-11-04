package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/group-a-study/q2-applemusic/internal/apple"
	"github.com/group-a-study/q2-applemusic/internal/q2"
)

// Signing in from a cookies file: a browser's export of music.apple.com's cookies (Netscape's
// cookies.txt, as gamdl takes it), put on the card at its root, in Apple Music/ or in .applemusic/,
// named anything with "cookie" in it and ending .txt (cookies.txt, cookies-music-apple-com.txt,
// music.apple.com_cookies.txt). Its media-user-token is checked with Apple and, if it signs in with
// a subscription, saved as the sign-in, as the phone's page does; the file is then deleted, as such
// an export can hold the Apple ID's own session cookie too. One Apple refuses stays, the menu saying
// why, and isn't asked about again until it changes.

// cookieEvery is how often (in the run loop's seconds) the card is looked at for a cookies file.
const cookieEvery = 10

// cookieFiles are the cookies files on the card, in the folders looked in.
func (d *daemon) cookieFiles() []string {
	var out []string
	for _, dir := range []string{q2.Card, d.dl.folder, d.dir} {
		des, _ := os.ReadDir(dir)
		for _, de := range des {
			n := strings.ToLower(de.Name())
			if de.Type().IsRegular() && strings.HasSuffix(n, ".txt") && strings.Contains(n, "cookie") {
				out = append(out, filepath.Join(dir, de.Name()))
			}
		}
	}
	return out
}

// cookieToken is a cookies file's media-user-token for music.apple.com, "" without one.
func cookieToken(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) >= 7 && f[5] == "media-user-token" && strings.HasSuffix(strings.TrimPrefix(f[0], "#HttpOnly_"), "music.apple.com") {
			if tok := strings.TrimSpace(f[6]); validToken(tok) {
				return tok
			}
		}
	}
	if tok := pastedToken(string(b)); validToken(tok) { // a Cookie header saved as text
		return tok
	}
	return ""
}

// cookieSeen is a cookies file as last looked at, so one Apple refused isn't asked about again
// until it changes.
type cookieSeen struct {
	size int64
	mod  time.Time
}

// importCookies signs in from a cookies file on the card, if there is one with a token that isn't
// the sign-in already.
func (d *daemon) importCookies() {
	if !cardReady() {
		return
	}
	for _, path := range d.cookieFiles() {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() > 1<<20 {
			continue
		}
		seen := cookieSeen{fi.Size(), fi.ModTime()}
		d.mu.Lock()
		was, asked := d.cookies[path]
		d.mu.Unlock()
		if asked && was == seen {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		tok := cookieToken(b)
		note := ""
		switch {
		case tok == "":
			note = filepath.Base(path) + " has no music.apple.com media-user-token"
		case tok == d.token():
			os.Remove(path) // the sign-in already
			continue
		default:
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			a, err := apple.New(tok, filepath.Join(d.dir, "devtoken"), d.dev).Account(ctx)
			cancel()
			switch {
			case apple.IsNoNetwork(err):
				continue // asked again next time
			case errors.Is(err, apple.ErrSignedOut):
				note = "Apple refused " + filepath.Base(path) + ": export it again"
			case err != nil:
				continue
			case !a.Active:
				note = filepath.Base(path) + "'s account has no Apple Music subscription"
			default:
				if q2.WriteFile(filepath.Join(d.dir, tokenFile), []byte(tok+"\n")) != nil {
					continue
				}
				os.Remove(path)
				logf("signed in from %s (deleted)", path)
				d.mu.Lock()
				delete(d.cookies, path)
				d.cookieNote = ""
				d.mu.Unlock()
				d.signedIn()
				return
			}
		}
		logf("cookies file %s: %s", path, note)
		d.mu.Lock()
		d.cookies[path] = seen
		d.cookieNote = note
		d.mu.Unlock()
	}
}

// token is the sign-in on the card now.
func (d *daemon) token() string {
	b, _ := os.ReadFile(filepath.Join(d.dir, tokenFile))
	return strings.TrimSpace(string(b))
}
