package console

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoDirectModelAPI guards the project's core rule (CLAUDE.md): model
// requests are sent only by the official CLIs on each machine, never by this
// program. Provider API hosts, endpoints or credential variables in the
// source mean someone started turning it into a proxy.
func TestNoDirectModelAPI(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)(api\.anthropic\.com|api\.openai\.com|chatgpt\.com/backend-api|/v1/messages\b|/v1/chat/completions|/v1/responses\b|x-api-key)`)
	// Credential variables may be named only to tell whether a login exists
	// (agents.go); using their values anywhere else is not allowed.
	credential := regexp.MustCompile(`(ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|OPENAI_API_KEY)`)
	allowedCredentialFiles := map[string]bool{"agents.go": true}
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", ".build", "build", ".gradle", "libs", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(p)
		if ext != ".go" && ext != ".js" && ext != ".swift" && ext != ".java" && ext != ".html" {
			return nil
		}
		if strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if m := forbidden.Find(b); m != nil {
			t.Errorf("%s mentions %q: model requests must come from the CLI on the node, not this program (see CLAUDE.md)", p, m)
		}
		if m := credential.Find(b); m != nil && !allowedCredentialFiles[filepath.Base(p)] {
			t.Errorf("%s uses credential variable %q: the console must not read or pass CLI credentials (see CLAUDE.md)", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
