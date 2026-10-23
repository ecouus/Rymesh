package console

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

	"golang.org/x/sys/unix"
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
		raw = cfg.DefaultDir()
	}
	// Only "~" and "~/…": "~bob" is another user's home, not ours plus "bob".
	if raw == "~" || strings.HasPrefix(raw, "~/") {
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
	if !validName(name) {
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
	// A known size that cannot fit is refused before any of it is written,
	// rather than filling the disk and failing at the end.
	if free, ok := freeSpace(dir); ok && r.ContentLength > 0 && uint64(r.ContentLength) > free {
		writeErr(w, http.StatusInsufficientStorage, fmt.Sprintf("磁盘空间不足：需要 %s，只剩 %s",
			humanBytes(uint64(r.ContentLength)), humanBytes(free)))
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
		writeErr(w, 400, fmt.Sprintf("上传中断: %v", err))
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

// validName reports whether name is usable as one path element.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// handleFilesRename gives a file or folder a new name in the same folder. It
// refuses to replace something that already has that name.
func (s *Server) handleFilesRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	p, ok := s.filePath(w, req.Path)
	if !ok {
		return
	}
	if !validName(req.Name) {
		writeErr(w, 400, "文件名无效")
		return
	}
	if _, err := os.Lstat(p); err != nil {
		fileErr(w, err)
		return
	}
	target := filepath.Join(filepath.Dir(p), req.Name)
	if target == p {
		writeJSON(w, 200, map[string]any{"path": p})
		return
	}
	if _, err := os.Lstat(target); err == nil {
		writeErr(w, 409, "已经有同名的文件或文件夹")
		return
	}
	if err := os.Rename(p, target); err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": target})
}

// handleFilesDelete removes a file, a link (not what it points at) or a
// folder with everything in it. The root, top-level system folders and the
// home folder are refused: one slip there would wreck the machine.
func (s *Server) handleFilesDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	p, ok := s.filePath(w, req.Path)
	if !ok {
		return
	}
	home, _ := os.UserHomeDir()
	if filepath.Dir(p) == "/" || (home != "" && p == filepath.Clean(home)) {
		writeErr(w, 403, "不能删除根目录、顶层系统目录或主目录")
		return
	}
	if _, err := os.Lstat(p); err != nil {
		fileErr(w, err)
		return
	}
	if err := os.RemoveAll(p); err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
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

// freeSpace is the space an unprivileged write can still use in dir.
func freeSpace(dir string) (uint64, bool) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n)/unit, 0
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %cB", v, "KMGT"[i])
}
