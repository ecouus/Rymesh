package importer

import (
	"encoding/json"
	"fmt"

	"rymesh/internal/store"
	"rymesh/internal/util"
)

// snapshot is a whole conversation read back from an agent's own store
// (Hermes' session export, Codex' app-server), as opposed to a transcript file
// the importer follows line by line. It is replaced as a whole when it
// changes.
type snapshot struct {
	agent, nativeID, cwd, title string
	created, updated            string
	// stamp changes whenever the conversation does; an unchanged one is
	// skipped without touching the store.
	stamp  string
	events []store.ImportedEvent
	// prevIDs are native ids the conversation had before (Hermes continues a
	// compressed conversation in a new session).
	prevIDs []string
	meta    map[string]string // extra session meta keys
	// offset is the agent's own position after this snapshot (Codex: the
	// number of turns read), for appending later turns.
	offset int64
}

func (im *Importer) storeSnapshot(sn snapshot, rep *ImportReport) error {
	existing, err := im.store.FindNativeSession(sn.agent, sn.nativeID)
	if err != nil && !store.IsNoRowsErr(err) {
		return err
	}
	for _, id := range sn.prevIDs {
		if existing != nil {
			break
		}
		if prev, err := im.store.FindNativeSession(sn.agent, id); err == nil {
			existing = prev
		}
	}
	if existing != nil && existing.Source == "web" {
		rep.Skipped++ // started in the console: its turns come from the task logs
		return nil
	}
	if len(sn.events) == 0 {
		rep.Skipped++
		return nil
	}
	title := sn.title
	if existing != nil {
		st := store.ParseImportState(existing.Meta)
		if st.Path == sn.stamp && existing.NativeID == sn.nativeID {
			rep.Skipped++
			return nil
		}
		// Turns sent from the console are in the task logs and in the agent's
		// own history; replacing the snapshot would show them twice.
		if has, _ := im.store.HasNonImportEvents(existing.ID, importTaskID(existing.ID)); has {
			rep.Skipped++
			return nil
		}
		// A title the user changed in the console stays; one that is still
		// what the last import set follows the agent's.
		var m struct {
			Title string `json:"import_title"`
		}
		_ = json.Unmarshal([]byte(existing.Meta), &m)
		if m.Title != "" && existing.Title != m.Title {
			title = ""
		}
		if existing.Cwd != sn.cwd {
			if err := im.store.UpdateImportedSession(existing.ID, firstNonEmpty(title, existing.Title), sn.cwd, sn.updated); err != nil {
				return err
			}
		}
		if existing.NativeID != sn.nativeID {
			if err := im.store.SetSessionNativeID(existing.ID, sn.nativeID); err != nil {
				return err
			}
		}
		rep.Updated++
	} else {
		sess := &store.Session{
			Agent: sn.agent, NativeID: sn.nativeID, Cwd: sn.cwd, Title: sn.title,
			Source: "imported", CreatedAt: sn.created, UpdatedAt: sn.updated,
			LastMessageAt: sn.updated, SnapshotImported: true,
		}
		if err := im.store.CreateSession(sess); err != nil {
			return err
		}
		existing = sess
		rep.Imported++
	}
	events := sn.events
	if limit := im.historyLimit; limit > 0 && len(events) > limit {
		notice := store.ImportedEvent{TS: sn.created, Kind: "notice", Payload: map[string]any{
			"text": fmt.Sprintf("更早的 %d 条历史未在控制台展开。", len(events)-limit)}}
		events = append([]store.ImportedEvent{notice}, events[len(events)-limit:]...)
	}
	// One transaction: a failure part-way must not leave half a conversation.
	st := store.ImportState{Path: sn.stamp, Offset: sn.offset, Updated: util.LaterTS(sn.updated, existing.LastMessageAt)}
	seq, err := im.store.ReplaceImportedEvents(existing.ID, importTaskID(existing.ID), events, st.Updated, title, st)
	if err != nil {
		return err
	}
	st.Seq = seq
	if err := im.store.SetImportState(existing.ID, st); err != nil {
		return err
	}
	if err := im.store.MarkSnapshotImported(existing.ID); err != nil {
		return err
	}
	meta := map[string]string{"import_title": sn.title}
	if title == "" {
		delete(meta, "import_title") // keep comparing against what was last set
	}
	for k, v := range sn.meta {
		meta[k] = v
	}
	return im.store.SetSessionMetaKeys(existing.ID, meta)
}
