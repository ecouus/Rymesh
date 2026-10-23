package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowedCWD(t *testing.T) {
	// allowedCWD resolves symlinks; on macOS the temp dir lives behind one
	// (/var -> /private/var), so compare against the resolved root.
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DefaultCWD: root, AllowedRoots: []string{root}}

	got, err := cfg.AllowedCWD(filepath.Join(root, "sub"))
	if err != nil || got != filepath.Join(root, "sub") {
		t.Fatalf("inside root: got %q err %v", got, err)
	}
	if _, err := cfg.AllowedCWD(outside); err == nil {
		t.Fatal("outside root accepted")
	}
	if _, err := cfg.AllowedCWD("/etc"); err == nil {
		t.Fatal("/etc accepted")
	}
	// A symlink inside the root pointing outside must be rejected.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := cfg.AllowedCWD(link); err == nil {
			t.Fatal("symlink escape accepted")
		}
	}
	if _, err := cfg.AllowedCWD(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
	if _, err := cfg.AllowedCWD("relative/path"); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestConfigVersionIsEnforced(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Version = 2
	err := cfg.Normalize()
	if err == nil || !strings.Contains(err.Error(), "unsupported config version") {
		t.Fatalf("expected version error, got %v", err)
	}
}

func TestImportExcludeRootsNormalize(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Secret = "c2VjcmV0"
	cfg.ImportExcludeRoots = []string{"/tmp/ac-*", "/tmp/ac-*", "/tmp/ac-*/"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ImportExcludeRoots) != 1 || cfg.ImportExcludeRoots[0] != "/tmp/ac-*" {
		t.Fatalf("normalized excludes = %v", cfg.ImportExcludeRoots)
	}
	cfg.ImportExcludeRoots = []string{"relative/path"}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("relative exclude root accepted")
	}
	cfg.ImportExcludeRoots = []string{"/"}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("root directory accepted as exclude root")
	}
}

func TestIsExcludedCWD(t *testing.T) {
	roots := []string{"/tmp/ac-*"}
	for _, cwd := range []string{"/tmp/ac-e2e", "/tmp/ac-e2e/work", "/tmp/ac-prod/work"} {
		if !IsExcludedCWD(cwd, roots) {
			t.Fatalf("%q should be excluded", cwd)
		}
	}
	for _, cwd := range []string{"/tmp/accounting", "/tmp/ac", "/tmp/acct"} {
		if IsExcludedCWD(cwd, roots) {
			t.Fatalf("%q should not be excluded", cwd)
		}
	}
	if !IsExcludedCWD("/tmp/ac-e2e/work", []string{"/tmp/ac-e2e"}) {
		t.Fatal("plain root should cover descendants")
	}
	if IsExcludedCWD("/tmp/ac-e2e2", []string{"/tmp/ac-e2e"}) {
		t.Fatal("plain root must match path components only")
	}
}

// Signing session cookies with an empty secret uses a publicly known HMAC key,
// so a config without one must be rejected instead of starting insecurely.
func TestConfigRejectsEmptySecret(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Secret = ""
	if err := cfg.Normalize(); err == nil {
		t.Fatal("empty secret accepted")
	}
	cfg.Secret = "   "
	if err := cfg.Normalize(); err == nil {
		t.Fatal("blank secret accepted")
	}
	cfg.Secret = "c2VjcmV0"
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
}

func TestDefaultCWDIsHomeAndSurvivesMissingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := DefaultConfig()
	if c.DefaultCWD != home || c.AllowedRoots[0] != home {
		t.Fatalf("default cwd %q roots %v", c.DefaultCWD, c.AllowedRoots)
	}
	// A configured directory that does not exist on this machine falls back
	// to home instead of breaking new conversations.
	c.DefaultCWD = filepath.Join(home, "gone")
	if got := c.DefaultDir(); got != home {
		t.Fatalf("fallback = %q", got)
	}
	real, _ := filepath.EvalSymlinks(home)
	if got, err := c.AllowedCWD(""); err != nil || got != real {
		t.Fatalf("allowedCWD(\"\") = %q, %v", got, err)
	}
}
