// Package i18n translates the program's messages. The Chinese text in the
// source is the message id: a catalog maps it to another language, and a
// message with no entry stays as written.
//
// Messages made with fmt formats are keyed by the format ("无法连接 %s：%v").
// Translate also works backwards from a finished message, matching it against
// those formats and translating the filled-in parts too, so text the server
// stored or another node sent can still be shown in the reader's language.
// web/i18n.js does the same for the page.
package i18n

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Catalog maps source messages to one language.
type Catalog struct {
	exact    map[string]string
	patterns []pattern
}

type pattern struct {
	re      *regexp.Regexp
	verbs   []string // the format's verbs in order, e.g. "%s", "%d"
	target  string
	literal int // length of the fixed text, to try specific formats first
}

// verbRe finds fmt verbs, including explicit argument indexes ("%[2]s").
var verbRe = regexp.MustCompile(`%(?:\[\d+\])?[-+# 0]*\d*(?:\.\d+)?[a-zA-Z%]`)

// Parse reads a catalog: a JSON object from source message to translation.
func Parse(data []byte) (*Catalog, error) {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	c := &Catalog{exact: m}
	for k, v := range m {
		if p, ok := compile(k, v); ok {
			c.patterns = append(c.patterns, p)
		}
	}
	sort.SliceStable(c.patterns, func(i, j int) bool {
		if c.patterns[i].literal != c.patterns[j].literal {
			return c.patterns[i].literal > c.patterns[j].literal
		}
		return c.patterns[i].re.String() < c.patterns[j].re.String()
	})
	return c, nil
}

func compile(format, target string) (pattern, bool) {
	locs := verbRe.FindAllStringIndex(format, -1)
	var b strings.Builder
	b.WriteString(`^`)
	var verbs []string
	last, literal := 0, 0
	for _, l := range locs {
		lit := format[last:l[0]]
		literal += len(lit)
		b.WriteString(regexp.QuoteMeta(lit))
		v := format[l[0]:l[1]]
		last = l[1]
		if v == "%%" {
			b.WriteString(`%`)
			literal++
			continue
		}
		verbs = append(verbs, v)
		switch v[len(v)-1] {
		case 'd':
			b.WriteString(`(-?\d+)`)
		case 'f', 'g':
			b.WriteString(`(-?[\d.]+)`)
		default:
			b.WriteString(`([\s\S]*?)`)
		}
	}
	if len(verbs) == 0 {
		return pattern{}, false
	}
	lit := format[last:]
	literal += len(lit)
	b.WriteString(regexp.QuoteMeta(lit))
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return pattern{}, false
	}
	return pattern{re: re, verbs: verbs, target: target, literal: literal}, true
}

// Lookup returns the translation of a source message (a format or plain
// text), if the catalog has one.
func (c *Catalog) Lookup(s string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.exact[s]
	return v, ok && v != ""
}

// Sprintf formats with the translated format.
func (c *Catalog) Sprintf(format string, args ...any) string {
	if v, ok := c.Lookup(format); ok {
		format = v
	}
	if len(args) == 0 {
		return strings.ReplaceAll(format, "%%", "%")
	}
	return fmt.Sprintf(format, args...)
}

// T translates one source message without arguments.
func (c *Catalog) T(s string) string {
	if v, ok := c.Lookup(s); ok {
		return v
	}
	return s
}

// Translate translates a finished message: as a whole, by the format it was
// made from, or line by line.
func (c *Catalog) Translate(msg string) string {
	return c.translate(msg, 0)
}

func (c *Catalog) translate(msg string, depth int) string {
	if c == nil || msg == "" || depth > 4 || !hasHan(msg) {
		return msg
	}
	if v, ok := c.Lookup(msg); ok {
		return v
	}
	// Several lines are translated one by one before any format is tried: a
	// format's "%s" would otherwise swallow neighbouring lines.
	if strings.Contains(msg, "\n") {
		lines := strings.Split(msg, "\n")
		for i, l := range lines {
			lines[i] = c.translate(l, depth)
		}
		return strings.Join(lines, "\n")
	}
	for _, p := range c.patterns {
		m := p.re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		args := make([]string, len(m)-1)
		for i, a := range m[1:] {
			args[i] = c.translate(a, depth+1)
		}
		return fill(p.target, args)
	}
	return msg

}

// fill puts already formatted arguments into a translated format, in order
// or by explicit index.
func fill(format string, args []string) string {
	next := 0
	return verbRe.ReplaceAllStringFunc(format, func(v string) string {
		if v == "%%" {
			return "%"
		}
		i := next
		if strings.HasPrefix(v, "%[") {
			if n, err := strconv.Atoi(v[2:strings.IndexByte(v, ']')]); err == nil {
				i = n - 1
			}
		}
		next = i + 1
		if i < 0 || i >= len(args) {
			return v
		}
		return args[i]
	})
}

func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x3400 && r <= 0x9fff || r >= 0xff00 && r <= 0xffef || r >= 0x3000 && r <= 0x303f {
			return true
		}
	}
	return false
}

// EnvLang is the language for command-line output: RYMESH_LANG, else the
// locale variables. Chinese locales get "zh"; anything else "en".
func EnvLang() string {
	for _, k := range []string{"RYMESH_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return Normalize(v)
		}
	}
	return "en"
}

// Normalize maps a language tag ("zh-Hans-CN", "zh_CN.UTF-8", "en-US") to
// a supported language.
func Normalize(tag string) string {
	if strings.HasPrefix(strings.ToLower(tag), "zh") {
		return "zh"
	}
	return "en"
}
