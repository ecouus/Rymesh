package console

// Files: browse, download and upload files on this machine for the page's
// file view. Like the terminal (terminal.go), a hub reaches a server's files
// through the /n/{node} relay, so one implementation serves this machine and
// every node, and the access is the console user's, as over SSH. The whole
// filesystem is reachable: the terminal already is, and logs or configs
// outside the conversation directories are what people come for.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// filesMaxEntries bounds one listing; a directory with more says so.
const filesMaxEntries = 5000

type fileEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
	Link  bool   `json:"link,omitempty"`
}

// filePath turns ?path= into a clean absolute path; empty means the default
// conversation directory.
func (s *Server) filePath(w http.ResponseWriter, raw string) (string, bool) {
	cfg := s.config()
	if cfg.HubOnly {
		writeErr(w, 400, "这台设备没有本机文件")
		return "", false
	}
	if raw == "" {
		raw = cfg.defaultCWD()
	}
	if strings.HasPrefix(raw, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			raw = home + raw[1:]
		}
	}
	if !filepath.IsAbs(raw) {
		writeErr(w, 400, "路径必须是绝对路径")
		return "", false
	}
	return filepath.Clean(raw), true
}

func fileErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeErr(w, 404, "不存在")
	case errors.Is(err, fs.ErrPermission):
		writeErr(w, 403, "没有权限")
	default:
		writeErr(w, 500, err.Error())
	}
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.filePath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fileErr(w, err)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.IsDir() {
		writeErr(w, 400, "不是文件夹")
		return
	}
	names, err := f.Readdirnames(filesMaxEntries + 1)
	if err != nil && err != io.EOF {
		fileErr(w, err)
		return
	}
	truncated := len(names) > filesMaxEntries
	if truncated {
		names = names[:filesMaxEntries]
	}
	entries := make([]fileEntry, 0, len(names))
	for _, n := range names {
		full := filepath.Join(p, n)
		lst, err := os.Lstat(full)
		if err != nil {
			continue
		}
		e := fileEntry{Name: n, Size: lst.Size(), MTime: lst.ModTime().UTC().Format(time.RFC3339)}
		if lst.Mode()&fs.ModeSymlink != 0 {
			e.Link = true
			// A link shows what it points at; a dangling one stays a file.
			if st, err := os.Stat(full); err == nil {
				e.Dir, e.Size, e.MTime = st.IsDir(), st.Size(), st.ModTime().UTC().Format(time.RFC3339)
			}
		} else {
			e.Dir = lst.IsDir()
		}
		if e.Dir {
			e.Size = 0
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	home, _ := os.UserHomeDir()
	writeJSON(w, 200, map[string]any{"path": p, "parent": parent, "home": home,
		"entries": entries, "truncated": truncated})
}

// handleFilesDownload serves one file as an attachment, with range requests
// so an interrupted download can resume.
func (s *Server) handleFilesDownload(w http.ResponseWriter, r *http.Request) {
	p, ok := s.filePath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fileErr(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fileErr(w, err)
		return
	}
	if !st.Mode().IsRegular() {
		writeErr(w, 400, "只能下载普通文件")
		return
	}
	name := filepath.Base(p)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// handleFilesUpload writes the request body to ?name= in folder ?dir=. It
// never overwrites: a taken name becomes "name (1).ext". The data lands in a
// hidden temporary file first, so a broken upload leaves no half file.
func (s *Server) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, ok := s.filePath(w, q.Get("dir"))
	if !ok {
		return
	}
	name := q.Get("name")
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		writeErr(w, 400, "文件名无效")
		return
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		if err != nil {
			fileErr(w, err)
		} else {
			writeErr(w, 400, "不是文件夹")
		}
		return
	}
	tmp, err := os.CreateTemp(dir, ".rymesh-upload-*")
	if err != nil {
		fileErr(w, err)
		return
	}
	n, err := io.Copy(tmp, r.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		writeErr(w, 400, "上传中断: "+err.Error())
		return
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	final, err := freeName(dir, name, func(target string) error {
		// Claim the name with an exclusive create, then move the data over
		// the empty placeholder: check and claim are one step, and it works
		// on filesystems without hard links.
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		f.Close()
		if err := os.Rename(tmp.Name(), target); err != nil {
			os.Remove(target)
			return err
		}
		return nil
	})
	if err != nil {
		os.Remove(tmp.Name())
		fileErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"name": filepath.Base(final), "path": final, "size": n})
}

// freeName calls place with dir/name, or "name (1).ext", "name (2).ext"…
// until one is not taken. place must fail with fs.ErrExist on a taken name.
func freeName(dir, name string, place func(string) error) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" { // ".bashrc"
		stem, ext = name, ""
	}
	for i := 0; i < 1000; i++ {
		target := filepath.Join(dir, name)
		if i > 0 {
			target = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}
		err := place(target)
		if err == nil {
			return target, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("同名文件过多")
}
