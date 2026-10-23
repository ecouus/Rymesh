package store

import (
	"encoding/json"
	"strings"
)

// SessionSettings are per-conversation choices applied to every message.
type SessionSettings struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

func (s *Store) GetSessionSettings(id string) SessionSettings {
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(s.GetSessionMeta(id)), &m)
	var st SessionSettings
	_ = json.Unmarshal(m["settings"], &st)
	return st
}

// UpdateSessionSettings changes the settings stored in the session's meta,
// next to (never over) the import cursor that shares the document.
func (s *Store) UpdateSessionSettings(id string, fn func(*SessionSettings)) (SessionSettings, error) {
	m := map[string]any{}
	if cur := strings.TrimSpace(s.GetSessionMeta(id)); cur != "" {
		_ = json.Unmarshal([]byte(cur), &m)
	}
	st := s.GetSessionSettings(id)
	fn(&st)
	m["settings"] = st
	b, err := json.Marshal(m)
	if err != nil {
		return st, err
	}
	_, err = s.DB.Exec(`UPDATE sessions SET meta=? WHERE id=?`, string(b), id)
	return st, err
}
