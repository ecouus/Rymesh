package util

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"
)

func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func RandomID(n int) string {
	b, err := RandomBytes(n)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func NewUUID() string {
	b, err := RandomBytes(16)
	if err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// LaterTS returns whichever timestamp is later. RFC3339 strings are compared as
// instants rather than text: "+08:00" offsets and fractional-second spellings
// make lexical order disagree with time order. Unparsable values fall back to
// text order so a malformed timestamp is never dropped.
func LaterTS(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	if ea != nil || eb != nil {
		if a > b {
			return a
		}
		return b
	}
	if tb.After(ta) {
		return b
	}
	return a
}
