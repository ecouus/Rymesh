package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesListDownloadUpload(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: &Config{DefaultCWD: dir}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello world"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644))
	must(os.Symlink("sub", filepath.Join(dir, "link-to-sub")))

	// No path lists the default directory: folders first, hidden files
	// included (the page decides whether to show them), links resolved.
	rec := httptest.NewRecorder()
	s.handleFilesList(rec, httptest.NewRequest("GET", "/api/files", nil))
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Path    string      `json:"path"`
		Parent  string      `json:"parent"`
		Entries []fileEntry `json:"entries"`
	}
	must(json.Unmarshal(rec.Body.Bytes(), &list))
	var names []string
	for _, e := range list.Entries {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "link-to-sub,sub,.hidden,b.txt" {
		t.Fatalf("entries = %s", got)
	}
	if !list.Entries[0].Dir || !list.Entries[0].Link || list.Entries[3].Size != 11 {
		t.Fatalf("entry details wrong: %+v", list.Entries)
	}
	if list.Path != dir || list.Parent != filepath.Dir(dir) {
		t.Fatalf("path %q parent %q", list.Path, list.Parent)
	}

	// Relative paths are refused; a missing one is a 404.
	for path, code := range map[string]int{"relative/x": 400, filepath.Join(dir, "nope"): 404} {
		rec = httptest.NewRecorder()
		s.handleFilesList(rec, httptest.NewRequest("GET", "/api/files?path="+url.QueryEscape(path), nil))
		if rec.Code != code {
			t.Fatalf("%s: got %d, want %d", path, rec.Code, code)
		}
	}

	// Download: an attachment with the UTF-8 name, and ranges for resuming.
	must(os.WriteFile(filepath.Join(dir, "报告 1.txt"), []byte("0123456789"), 0o644))
	q := "/api/files/download?path=" + url.QueryEscape(filepath.Join(dir, "报告 1.txt"))
	rec = httptest.NewRecorder()
	s.handleFilesDownload(rec, httptest.NewRequest("GET", q, nil))
	if rec.Code != 200 || rec.Body.String() != "0123456789" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "attachment; filename*=UTF-8''%E6%8A%A5%E5%91%8A%201.txt" {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	req := httptest.NewRequest("GET", q, nil)
	req.Header.Set("Range", "bytes=4-")
	rec = httptest.NewRecorder()
	s.handleFilesDownload(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "456789" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.handleFilesDownload(rec, httptest.NewRequest("GET", "/api/files/download?path="+url.QueryEscape(filepath.Join(dir, "sub")), nil))
	if rec.Code != 400 {
		t.Fatalf("downloading a folder: %d", rec.Code)
	}

	// Upload never overwrites: the second copy gets a numbered name, and no
	// temporary file is left behind.
	upload := func(name, body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		u := "/api/files/upload?dir=" + url.QueryEscape(filepath.Join(dir, "sub")) + "&name=" + url.QueryEscape(name)
		s.handleFilesUpload(rec, httptest.NewRequest("POST", u, strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("upload %s: %d %s", name, rec.Code, rec.Body)
		}
		var res struct{ Name string }
		must(json.Unmarshal(rec.Body.Bytes(), &res))
		return res.Name
	}
	if n := upload("photo.jpg", "one"); n != "photo.jpg" {
		t.Fatalf("first upload named %q", n)
	}
	if n := upload("photo.jpg", "two"); n != "photo (1).jpg" {
		t.Fatalf("second upload named %q", n)
	}
	if n := upload(".env", "three"); n != ".env" {
		t.Fatalf("dotfile upload named %q", n)
	}
	if n := upload(".env", "four"); n != ".env (1)" {
		t.Fatalf("second dotfile upload named %q", n)
	}
	for name, want := range map[string]string{"photo.jpg": "one", "photo (1).jpg": "two", ".env (1)": "four"} {
		b, err := os.ReadFile(filepath.Join(dir, "sub", name))
		if err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	left, _ := filepath.Glob(filepath.Join(dir, "sub", ".rymesh-upload-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	for _, bad := range []string{"", "..", "a/b"} {
		rec = httptest.NewRecorder()
		u := "/api/files/upload?dir=" + url.QueryEscape(dir) + "&name=" + url.QueryEscape(bad)
		s.handleFilesUpload(rec, httptest.NewRequest("POST", u, strings.NewReader("x")))
		if rec.Code != 400 {
			t.Fatalf("name %q accepted: %d", bad, rec.Code)
		}
	}

	// A hub-only console has no files of its own.
	hub := &Server{cfg: &Config{DefaultCWD: dir, HubOnly: true}}
	rec = httptest.NewRecorder()
	hub.handleFilesList(rec, httptest.NewRequest("GET", "/api/files", nil))
	if rec.Code != 400 {
		t.Fatalf("hub-only list: %d", rec.Code)
	}
}
