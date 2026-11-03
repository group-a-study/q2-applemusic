package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// File and folder names on the card (FAT): the characters it refuses replaced, no name ending in a
// dot or a space, and lengths kept well under its limits.

// cut shortens s to at most max bytes without splitting a character.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" {
		s = "song"
	}
	return strings.TrimSpace(cut(s, 180))
}

// folderName is s as an artist's, album's or playlist's folder: at most 100 bytes; unknown without
// a name.
func folderName(s, unknown string) string {
	if strings.Trim(s, " .") == "" {
		return unknown
	}
	return sanitize(cut(strings.TrimSpace(s), 100))
}

// albumFolderName is an album's folder, its year first ("2005 - Tender Buttons") so an artist's
// albums list oldest first wherever they are browsed by name; the title alone without a year.
func albumFolderName(album, year, unknown string) string {
	if !isYear(year) {
		return folderName(album, unknown)
	}
	return year + " - " + folderName(cut(strings.TrimSpace(album), 100-len("0000 - ")), unknown)
}

func isYear(s string) bool { return len(s) == 4 && strings.Trim(s, "0123456789") == "" }

// songName is a song's file name without its extension: its number on the album (disc first, on an
// album of several), its title and its id, "2-07 - Title [1440841363]"; without a number, just the
// title and id.
func songName(id, title string, number, of, disc, discs int) string {
	prefix := ""
	if number > 0 {
		width := 2
		if number >= 100 || of >= 100 {
			width = 3
		}
		prefix = fmt.Sprintf("%0*d - ", width, number)
		if discs > 1 && disc > 0 {
			prefix = fmt.Sprintf("%d-%0*d - ", disc, width, number)
		}
	}
	suffix := " [" + id + "]"
	return sanitize(cut(prefix+title, 180-len(suffix)) + suffix)
}

// flatName is a song's name with Organize Files off: "Title - Artist [id]".
func flatName(id, title, artist string) string {
	suffix := " [" + id + "]"
	name := title
	if artist != "" {
		name += " - " + artist
	}
	return sanitize(cut(name, 180-len(suffix)) + suffix)
}
