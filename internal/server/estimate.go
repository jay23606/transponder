package server

import (
	"encoding/json"
	"regexp"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/raven-clown/transponder/internal/tokens"
)

// base64DataURI matches an inline image, audio or file payload, which is not text for the model.
var base64DataURI = regexp.MustCompile(`^data:[a-z]+/[a-zA-Z0-9.+-]+;base64,`)

// estimateInput counts every key and string in a request body, and every number in an array,
// which is how token-array inputs arrive. Tool schemas, tool arguments and descriptions all reach
// the model, so they all count. Signatures do not, and neither does an image's base64 data: an
// image counts as tokens.Image instead, whatever its size. Other inline files, such as a PDF,
// count by the size of their data, since the model reads their pages.
//
// It reads the body once and counts as it goes, so a large body is neither decoded into a tree
// nor copied.
func estimateInput(body []byte) int64 {
	if !json.Valid(body) {
		return tokens.Estimate(string(body))
	}
	s := inputScanner{b: body}
	s.space()
	t := s.value(false).t
	if s.badNumber {
		// encoding/json refuses a number beyond float64, so the body is counted as text, as
		// for any body that does not decode.
		return tokens.Estimate(string(body))
	}
	return tokens.FromCounts(t.ascii, t.other) + t.numbers + t.images*tokens.Image
}

// tally is what part of a body adds to the estimate. Each key and string adds its characters
// and one space, as if every key and string were joined with spaces.
type tally struct{ ascii, other, numbers, images int64 }

func (t *tally) add(u tally) {
	t.ascii += u.ascii
	t.other += u.other
	t.numbers += u.numbers
	t.images += u.images
}

func (t *tally) sub(u tally) {
	t.ascii -= u.ascii
	t.other -= u.other
	t.numbers -= u.numbers
	t.images -= u.images
}

// scanned is one value and what its parent object needs to know about it.
type scanned struct {
	t    tally
	kind byte // 's' string, 'o' object, 'a' array, 'n' null, 'x' number or bool
	// lo and hi bound a string's content in the body, and esc says it holds escapes.
	lo, hi int
	esc    bool
	// An object's string "type", and its "data" and "url" fields, key and value.
	typ               []byte
	data, url         tally
	hasData, urlIsURI bool
}

// inputScanner walks a body json.Valid has accepted, so it does not check the syntax again.
type inputScanner struct {
	b []byte
	i int
	// badNumber is set by a number too large for a float64.
	badNumber bool
}

func (s *inputScanner) space() {
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

// value reads one value. A signature's strings do not count, even inside arrays.
func (s *inputScanner) value(signature bool) scanned {
	var v scanned
	switch s.b[s.i] {
	case '{':
		return s.object()
	case '[':
		v.kind = 'a'
		s.i++
		for {
			s.space()
			if s.b[s.i] == ']' {
				s.i++
				return v
			}
			if c := s.b[s.i]; c == '-' || c >= '0' && c <= '9' {
				s.number()
				v.t.numbers++
			} else {
				v.t.add(s.value(signature).t)
			}
			s.space()
			if s.b[s.i] == ',' {
				s.i++
			}
		}
	case '"':
		v.kind = 's'
		var ascii, other int64
		v.lo, v.hi, v.esc, ascii, other = s.str()
		if !signature {
			v.t.ascii, v.t.other = ascii+1, other
		}
	case 'n':
		v.kind = 'n'
		s.i += len("null")
	case 't':
		v.kind = 'x'
		s.i += len("true")
	case 'f':
		v.kind = 'x'
		s.i += len("false")
	default:
		v.kind = 'x'
		s.number()
	}
	return v
}

func (s *inputScanner) number() {
	start, exp := s.i, false
scan:
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case 'e', 'E':
			exp = true
			s.i++
		case '-', '+', '.', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			s.i++
		default:
			break scan
		}
	}
	// Only an exponent or more than 300 digits can leave float64's range.
	if exp || s.i-start > 300 {
		if _, err := strconv.ParseFloat(string(s.b[start:s.i]), 64); err != nil {
			s.badNumber = true
		}
	}
}

// object reads an object. An image part counts as tokens.Image, and its inline data does not
// count: the data of a base64 source, a data: URL in image_url.url, and a data: URL given as
// image_url or file_data itself.
func (s *inputScanner) object() scanned {
	v := scanned{kind: 'o'}
	var (
		hasImageURL, hasSource bool
		drop                   tally
	)
	s.i++
	for {
		s.space()
		if s.b[s.i] == '}' {
			s.i++
			break
		}
		klo, khi, kesc, ka, ko := s.str()
		key := s.text(klo, khi, kesc)
		s.space()
		s.i++ // the colon
		s.space()
		val := s.value(string(key) == "signature")
		field := tally{ascii: ka + 1, other: ko}
		field.add(val.t)
		v.t.add(field)
		switch string(key) {
		case "type":
			if val.kind == 's' {
				v.typ = s.text(val.lo, val.hi, val.esc)
			}
		case "data":
			v.data, v.hasData = field, true
		case "url":
			v.url, v.urlIsURI = field, val.kind == 's' && s.dataURI(val)
		case "source":
			hasSource = val.kind != 'n'
			if val.kind == 'o' && string(val.typ) == "base64" && val.hasData {
				drop.add(val.data)
			}
		case "image_url":
			hasImageURL = val.kind != 'n'
			if val.kind == 'o' && val.urlIsURI {
				drop.add(val.url)
			}
			if val.kind == 's' && s.dataURI(val) {
				drop.add(field)
			}
		case "file_data":
			if val.kind == 's' && s.dataURI(val) {
				drop.add(field)
			}
		}
		s.space()
		if s.b[s.i] == ',' {
			s.i++
		}
	}
	image := false
	switch string(v.typ) {
	case "input_image", "input_file":
		image = true
	case "image_url":
		image = hasImageURL
	case "image":
		image = hasSource
	}
	if image {
		v.t.sub(drop)
		v.t.images++
	}
	return v
}

// str reads the string that starts at s.i and returns where its content lies, whether it holds
// escapes, and its characters once decoded: ascii below 0x80, and other characters. As in
// encoding/json, an invalid UTF-8 byte and an unpaired surrogate each become one U+FFFD.
func (s *inputScanner) str() (lo, hi int, esc bool, ascii, other int64) {
	s.i++
	lo = s.i
	for {
		// Runs of plain ASCII, such as base64 data, are skipped in one tight loop.
		run := s.i
		for s.i < len(s.b) {
			if c := s.b[s.i]; c == '"' || c == '\\' || c >= utf8.RuneSelf {
				break
			}
			s.i++
		}
		ascii += int64(s.i - run)
		c := s.b[s.i]
		switch {
		case c == '"':
			hi = s.i
			s.i++
			return lo, hi, esc, ascii, other
		case c == '\\':
			esc = true
			if s.b[s.i+1] != 'u' {
				s.i += 2
				ascii++
				continue
			}
			r := hex4(s.b[s.i+2:])
			s.i += 6
			if !utf16.IsSurrogate(r) {
				if r < utf8.RuneSelf {
					ascii++
				} else {
					other++
				}
				continue
			}
			// A surrogate pair is one character; anything else after it is read on its own.
			if s.i+6 <= len(s.b) && s.b[s.i] == '\\' && s.b[s.i+1] == 'u' &&
				utf16.DecodeRune(r, hex4(s.b[s.i+2:])) != utf8.RuneError {
				s.i += 6
			}
			other++
		case c < utf8.RuneSelf:
			s.i++
			ascii++
		default:
			_, n := utf8.DecodeRune(s.b[s.i:])
			s.i += n
			other++
		}
	}
}

// text returns a string's decoded content. A string without escapes is a slice of the body;
// only one with escapes is decoded, which allocates, and keys and types rarely have any.
func (s *inputScanner) text(lo, hi int, esc bool) []byte {
	if !esc {
		return s.b[lo:hi]
	}
	var out string
	_ = json.Unmarshal(s.b[lo-1:hi+1], &out)
	return []byte(out)
}

// dataURI reports whether a string value is an inline base64 payload.
func (s *inputScanner) dataURI(v scanned) bool {
	return base64DataURI.Match(s.text(v.lo, v.hi, v.esc))
}

func hex4(b []byte) rune {
	var r rune
	for _, c := range b[:4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r |= rune(c - 'A' + 10)
		}
	}
	return r
}
