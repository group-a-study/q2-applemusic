package apple

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Lyrics is a song's lyrics as LRC, a [mm:ss.xx] stamp a line, when Apple times them by line
// (or by word, each line at its first word); else the plain text, a blank line between verses;
// "" without any. A failure is no lyrics: the song downloads without them.
func (c *Client) Lyrics(ctx context.Context, id string) string {
	u, err := c.catalog(ctx, "/songs/"+url.PathEscape(id)+"/lyrics", nil)
	if err != nil {
		return ""
	}
	var r struct {
		Data []struct {
			Attributes struct {
				TTML string `json:"ttml"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if c.call(ctx, http.MethodGet, u, nil, &r) != nil || len(r.Data) == 0 {
		return ""
	}
	return TTMLToLRC(r.Data[0].Attributes.TTML)
}

// TTMLToLRC turns Apple's TTML lyrics into LRC, or plain text when they aren't timed.
func TTMLToLRC(ttml string) string {
	d := xml.NewDecoder(strings.NewReader(ttml))
	type line struct {
		at   float64
		text string
	}
	var verses [][]line
	var cur *line
	var text strings.Builder
	depth := 0     // inside a <p>: how deep
	inBg := 0      // inside a background vocal span: how deep
	timed := false // any line with a begin
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Local == "div":
				verses = append(verses, nil)
			case t.Name.Local == "p":
				cur, depth = &line{at: -1}, 1
				text.Reset()
				if b := attr(t, "begin"); b != "" {
					if s, ok := seconds(b); ok {
						cur.at, timed = s, true
					}
				}
			case cur != nil:
				depth++
				if inBg > 0 || attr(t, "role") == "x-bg" {
					inBg++
				}
			}
		case xml.EndElement:
			if cur == nil {
				continue
			}
			if inBg > 0 {
				inBg--
			}
			if depth--; depth == 0 {
				cur.text = strings.Join(strings.Fields(text.String()), " ")
				if len(verses) == 0 {
					verses = append(verses, nil)
				}
				verses[len(verses)-1] = append(verses[len(verses)-1], *cur)
				cur = nil
			}
		case xml.CharData:
			if cur != nil && inBg == 0 {
				text.Write(t)
			}
		}
	}
	var b strings.Builder
	for _, v := range verses {
		if len(v) == 0 {
			continue
		}
		if !timed && b.Len() > 0 {
			b.WriteString("\n")
		}
		for _, l := range v {
			if timed {
				if l.at < 0 {
					continue
				}
				cs := int(l.at*100 + 0.5)
				fmt.Fprintf(&b, "[%02d:%02d.%02d]%s\n", cs/6000, cs/100%60, cs%100, l.text)
			} else {
				b.WriteString(l.text + "\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// seconds reads a TTML time: "12.345", "1:02.5", "1:02:03.4", "12.3s" or "1234ms".
func seconds(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasSuffix(s, "ms"):
		v, err := strconv.ParseFloat(s[:len(s)-2], 64)
		return v / 1000, err == nil
	case strings.HasSuffix(s, "s"):
		v, err := strconv.ParseFloat(s[:len(s)-1], 64)
		return v, err == nil
	}
	var total float64
	for _, part := range bytes.Split([]byte(s), []byte(":")) {
		v, err := strconv.ParseFloat(string(part), 64)
		if err != nil {
			return 0, false
		}
		total = total*60 + v
	}
	return total, true
}
