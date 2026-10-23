package store

import (
	"testing"
)

func TestSessionSettingsKeepImportState(t *testing.T) {
	st, _ := OpenStore(t.TempDir())
	defer st.Close()
	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "t"}
	_ = st.CreateSession(sess)
	_ = st.SetImportState(sess.ID, ImportState{Path: "/x.jsonl", Offset: 42})
	if _, err := st.UpdateSessionSettings(sess.ID, func(s *SessionSettings) { s.Model = "opus" }); err != nil {
		t.Fatal(err)
	}
	_ = st.SetImportState(sess.ID, ImportState{Path: "/x.jsonl", Offset: 99})
	if got := st.GetSessionSettings(sess.ID); got.Model != "opus" {
		t.Fatalf("settings lost after import update: %+v", got)
	}
	if s := ParseImportState(st.GetSessionMeta(sess.ID)); s.Offset != 99 {
		t.Fatalf("import state lost: %+v", s)
	}
}
