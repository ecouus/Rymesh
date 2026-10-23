package console

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"rymesh/internal/i18n"
)

// Chinese text in the sources is the message id; web/i18n/en.json must have
// an English entry for every one of them, and no entry nothing uses.
// RYMESH_I18N_DUMP=<file> writes the missing ones there as a JSON skeleton.

var hanRe = regexp.MustCompile(`[\x{3000}-\x{303f}\x{3400}-\x{9fff}\x{ff00}-\x{ffef}]`)

// Literals that are matched against text rather than shown.
var i18nExempt = map[string]bool{
	"Claude 会话 ": true, "Codex 会话 ": true, "新会话 · ": true, // import_sync.go: default titles
	"Rymesh 密钥库": true,              // the keychain item's label (vaultseal.go)
	"注意：":        true, "登录项": true, // Console.swift / AppDelegate.swift read the client's output
	"中文": true, // a language's own name, the same in every language
}

func loadEN(t *testing.T) map[string]string {
	b, err := os.ReadFile("web/i18n/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("en.json: %v", err)
	}
	return m
}

// goMessages collects Chinese string literals from the Go sources, leaving
// out comments, tests and literals only compared against other text.
func goMessages(t *testing.T) map[string]string {
	out := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") || d.Name() == "android" || d.Name() == "macos" || d.Name() == "dist" || d.Name() == "ref") && path != "." {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		matching := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "strings" {
						for _, a := range c.Args {
							if l, ok := a.(*ast.BasicLit); ok {
								matching[l] = true
							}
						}
					}
				}
			}
			l, ok := n.(*ast.BasicLit)
			if !ok || l.Kind != token.STRING || matching[l] {
				return true
			}
			s, err := strconv.Unquote(l.Value)
			if err != nil || !hanRe.MatchString(s) || i18nExempt[s] {
				return true
			}
			out[s] = fset.Position(l.Pos()).String()
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// jsMessages collects Chinese string literals from a script. Each must be
// passed to T()/TR() where it is written, or be a value in a lookup table
// (after ":", "[" or ","), translated where the table is read.
func jsMessages(t *testing.T, path string, out map[string]string, bad *[]string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	line := 1
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			line += strings.Count(src[i:i+2+end+2], "\n")
			i += end + 3
		case c == '/' && regexStart(src[:i]):
			// A regular expression literal; it may hold quotes.
			class := false
			for i++; i < len(src) && (src[i] != '/' || class); i++ {
				switch src[i] {
				case '\\':
					i++
				case '[':
					class = true
				case ']':
					class = false
				}
			}
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			raw := src[i+1 : j]
			if hanRe.MatchString(raw) {
				s := jsUnescape(raw)
				where := path + ":" + strconv.Itoa(line)
				out[s] = where
				before := strings.TrimRight(src[:i], " \t\n")
				if strings.HasSuffix(before, "indexOf(") {
					// compared with a server's own text, not shown
					delete(out, s)
				} else if !(strings.HasSuffix(before, "T(") || strings.HasSuffix(before, "TR(") ||
					strings.HasSuffix(before, ":") || strings.HasSuffix(before, "[") || strings.HasSuffix(before, ",")) {
					*bad = append(*bad, where+": "+s)
				}
			}
			line += strings.Count(src[i:j], "\n")
			i = j
		}
	}
}

// regexStart says whether a "/" after this text opens a regular expression
// rather than dividing.
func regexStart(before string) bool {
	b := strings.TrimRight(before, " \t\n")
	if b == "" || strings.HasSuffix(b, "return") {
		return true
	}
	return strings.ContainsRune("(,=:[!&|?{};+", rune(b[len(b)-1]))
}

func jsUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'u':
			if r, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
				b.WriteRune(rune(r))
				i += 4
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// htmlMessages collects the text and labelled attributes of index.html, as
// I18N.localize looks them up.
func htmlMessages(t *testing.T, out map[string]string) {
	b, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	src = regexp.MustCompile(`(?s)<noscript>.*?</noscript>`).ReplaceAllString(src, "")
	for _, m := range regexp.MustCompile(`(?s)<[^>]*data-i18n-html[^>]*>(.*?)</p>`).FindAllStringSubmatch(src, -1) {
		out[strings.TrimSpace(m[1])] = "web/index.html"
		src = strings.Replace(src, m[1], "", 1)
	}
	for _, m := range regexp.MustCompile(`>([^<]+)<`).FindAllStringSubmatch(src, -1) {
		if s := strings.TrimSpace(m[1]); hanRe.MatchString(s) {
			out[s] = "web/index.html"
		}
	}
	for _, m := range regexp.MustCompile(`(?:placeholder|title|aria-label)="([^"]*)"`).FindAllStringSubmatch(src, -1) {
		if hanRe.MatchString(m[1]) {
			out[m[1]] = "web/index.html"
		}
	}
}

// nativeMessages collects Chinese string literals of the Mac and Android
// apps, which look them up in the same catalog.
func nativeMessages(t *testing.T, out map[string]string) {
	var files []string
	for _, g := range []string{"macos/Sources/RymeshMac/*.swift", "android/app/src/main/java/dev/rymesh/app/*.java"} {
		m, _ := filepath.Glob(g)
		files = append(files, m...)
	}
	lit := regexp.MustCompile(`"((?:[^"\\\n]|\\.)*)"`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for n, l := range strings.Split(string(b), "\n") {
			if k := strings.Index(l, "//"); k >= 0 && !strings.Contains(l[:k], `"`) {
				l = l[:k]
			}
			for _, m := range lit.FindAllStringSubmatch(l, -1) {
				if hanRe.MatchString(m[1]) {
					out[jsUnescape(m[1])] = f + ":" + strconv.Itoa(n+1)
				}
			}
		}
	}
}

func TestI18nCatalog(t *testing.T) {
	en := loadEN(t)
	used := goMessages(t)
	var bad []string
	for _, f := range []string{"web/app.js", "web/markdown.js", "web/sw.js", "web/i18n.js"} {
		jsMessages(t, f, used, &bad)
	}
	htmlMessages(t, used)
	nativeMessages(t, used)
	for k := range i18nExempt {
		delete(used, k)
	}
	for _, b := range bad {
		t.Errorf("not translated where it is written (wrap it in T()): %s", b)
	}

	missing := map[string]string{}
	for k, where := range used {
		if en[k] == "" {
			missing[k] = ""
			t.Errorf("%s: no English for %q", where, k)
		}
	}
	if p := os.Getenv("RYMESH_I18N_DUMP"); p != "" && len(missing) > 0 {
		b, _ := json.MarshalIndent(missing, "", "  ")
		_ = os.WriteFile(p, b, 0o644)
	}
	var unused []string
	for k := range en {
		if _, ok := used[k]; !ok {
			unused = append(unused, k)
		}
	}
	sort.Strings(unused)
	for _, k := range unused {
		t.Errorf("en.json has an entry nothing uses: %q", k)
	}

	// A translation takes the same arguments as its source.
	verb := regexp.MustCompile(`%(?:\[(\d+)\])?[-+# 0]*\d*(?:\.\d+)?([a-zA-Z%])`)
	count := func(s string) (n, maxIdx int) {
		for _, m := range verb.FindAllStringSubmatch(s, -1) {
			if m[2] == "%" {
				continue
			}
			if m[1] != "" {
				i, _ := strconv.Atoi(m[1])
				maxIdx = max(maxIdx, i)
				continue
			}
			n++
		}
		return n, maxIdx
	}
	for k, v := range en {
		kn, ki := count(k)
		vn, vi := count(v)
		if max(kn, ki) != max(vn, vi) {
			t.Errorf("%q → %q: %d arguments, translation uses %d", k, v, max(kn, ki), max(vn, vi))
		}
		if hanRe.MatchString(v) {
			t.Errorf("%q → %q: translation still has Chinese", k, v)
		}
	}
}

// Messages come back from the server already filled in; the page and the
// command line translate them by the format they were made from.
func TestI18nTranslate(t *testing.T) {
	c, err := i18n.Parse([]byte(`{
		"无法连接 %s：%v": "Cannot connect to %s: %v",
		"连接超时": "Connection timed out",
		"%[1]s 找不到这段对话（可能已在 %[1]s 中删除）": "%[1]s cannot find this conversation (deleted in %[1]s?)",
		"排队中 · 前面还有 %d 个任务": "Queued · %d tasks ahead",
		"已导入 %d 台设备": "Imported %d devices",
		"已导入 %d 台设备、%d 把密钥": "Imported %d devices and %d keys"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"连接超时": "Connection timed out",
		"无法连接 10.0.0.2:22：连接超时":           "Cannot connect to 10.0.0.2:22: Connection timed out",
		"Hermes 找不到这段对话（可能已在 Hermes 中删除）": "Hermes cannot find this conversation (deleted in Hermes?)",
		"排队中 · 前面还有 3 个任务":                "Queued · 3 tasks ahead",
		"已导入 2 台设备、1 把密钥":                 "Imported 2 devices and 1 keys",
		"连接超时\n无法连接 a：连接超时":               "Connection timed out\nCannot connect to a: Connection timed out",
		"agent output, not ours":          "agent output, not ours",
		"未知的消息":                           "未知的消息",
	} {
		if got := c.Translate(in); got != want {
			t.Errorf("Translate(%q) = %q, want %q", in, got, want)
		}
	}
	if got := c.Sprintf("已导入 %d 台设备", 3); got != "Imported 3 devices" {
		t.Errorf("Sprintf = %q", got)
	}
	if got := i18n.Normalize("zh_CN.UTF-8"); got != "zh" {
		t.Errorf("Normalize = %q", got)
	}
	if got := i18n.Normalize("en_US.UTF-8"); got != "en" {
		t.Errorf("Normalize = %q", got)
	}
}
