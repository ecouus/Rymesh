package console

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"rymesh/internal/util"
)

// A QR code carries the same sealed bundle as an export file, made compact
// enough for one code: the payload is gzipped before sealing, stored as
// salt | nonce | ciphertext, and written in base45 so the code can use the
// QR alphanumeric mode. It is sealed with a pairing code the scanning phone
// generates and shows, and the user types on the exporting device: the code
// is never on the screen that shows the QR code, so neither a photo of that
// screen nor the code alone opens anything.

const qrPrefix = "RYMESH1:"

// qrAAD binds the sealed bytes to this format.
var qrAAD = []byte("rymesh-qr1")

// qrMaxPlain bounds what an import unpacks: a bundle of a few dozen devices
// is a few kilobytes.
const qrMaxPlain = 1 << 20

func sealBundleQR(p bundlePayload, code string) (string, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var gz bytes.Buffer
	w, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if _, err := w.Write(plain); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	salt, err := util.RandomBytes(16)
	if err != nil {
		return "", err
	}
	aead, err := bundleAEAD(code, salt, bundleScryptN)
	if err != nil {
		return "", err
	}
	nonce, err := util.RandomBytes(aead.NonceSize())
	if err != nil {
		return "", err
	}
	out := append(append(salt, nonce...), aead.Seal(nil, nonce, gz.Bytes(), qrAAD)...)
	return qrPrefix + base45Encode(out), nil
}

func openBundleQR(text, code string) (*bundlePayload, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, qrPrefix) {
		return nil, errors.New("这不是 Rymesh 导出的二维码")
	}
	if len(normalizeCode(code)) != 16 {
		return nil, errors.New("解密码应为 16 位，形如 XXXX-XXXX-XXXX-XXXX")
	}
	raw, err := base45Decode(strings.TrimPrefix(text, qrPrefix))
	// Checked before the costly key derivation.
	if err != nil || len(raw) < 16+12+16 {
		return nil, errors.New("二维码内容不完整，请重新扫描")
	}
	aead, err := bundleAEAD(code, raw[:16], bundleScryptN)
	if err != nil {
		return nil, err
	}
	gz, err := aead.Open(nil, raw[16:28], raw[28:], qrAAD)
	if err != nil {
		return nil, errors.New("解密码不正确，或二维码已损坏")
	}
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, errors.New("二维码已损坏")
	}
	plain, err := io.ReadAll(io.LimitReader(r, qrMaxPlain+1))
	if err != nil || len(plain) > qrMaxPlain {
		return nil, errors.New("二维码已损坏")
	}
	var p bundlePayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, errors.New("二维码已损坏")
	}
	return &p, nil
}

// qrSVG draws text as a QR code (with its quiet zone), or says why it does
// not fit in one.
func qrSVG(text string) (string, error) {
	var q *qrcode.QRCode
	var err error
	for _, level := range []qrcode.RecoveryLevel{qrcode.Medium, qrcode.Low} {
		if q, err = qrcode.New(text, level); err == nil {
			break
		}
	}
	if err != nil {
		return "", errors.New("选中的设备太多，放不进一个二维码：请少选几台分批导出，或改用文件导出")
	}
	bm := q.Bitmap()
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, len(bm), len(bm))
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, len(bm), len(bm))
	for y, row := range bm {
		for x := 0; x < len(row); {
			if !row[x] {
				x++
				continue
			}
			start := x
			for x < len(row) && row[x] {
				x++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", start, y, x-start, x-start)
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}

// base45 (RFC 9285) packs two bytes into three characters of the QR
// alphanumeric set.
const base45Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

func base45Encode(b []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		n := int(b[i])<<8 | int(b[i+1])
		sb.WriteByte(base45Alphabet[n%45])
		sb.WriteByte(base45Alphabet[n/45%45])
		sb.WriteByte(base45Alphabet[n/45/45])
	}
	if len(b)%2 == 1 {
		n := int(b[len(b)-1])
		sb.WriteByte(base45Alphabet[n%45])
		sb.WriteByte(base45Alphabet[n/45])
	}
	return sb.String()
}

func base45Decode(s string) ([]byte, error) {
	val := func(c byte) (int, error) {
		if i := strings.IndexByte(base45Alphabet, c); i >= 0 {
			return i, nil
		}
		return 0, errors.New("invalid base45")
	}
	if len(s)%3 == 1 {
		return nil, errors.New("invalid base45 length")
	}
	out := make([]byte, 0, len(s)/3*2+1)
	for i := 0; i < len(s); i += 3 {
		if i+2 >= len(s) {
			c, err1 := val(s[i])
			d, err2 := val(s[i+1])
			n := c + d*45
			if err1 != nil || err2 != nil || n > 0xff {
				return nil, errors.New("invalid base45")
			}
			out = append(out, byte(n))
			break
		}
		c, err1 := val(s[i])
		d, err2 := val(s[i+1])
		e, err3 := val(s[i+2])
		n := c + d*45 + e*45*45
		if err1 != nil || err2 != nil || err3 != nil || n > 0xffff {
			return nil, errors.New("invalid base45")
		}
		out = append(out, byte(n>>8), byte(n))
	}
	return out, nil
}
