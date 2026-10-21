package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// handleStream is the live link between the browser and the persisted event log.
// The event log is the source of truth: a disconnect, page reload or service
// restart only changes where the cursor resumes.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetSession(id); err != nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	cursor := int64(0)
	explicit := false
	if v := r.URL.Query().Get("after"); v != "" {
		cursor, _ = strconv.ParseInt(v, 10, 64)
		explicit = true
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		if n > cursor {
			cursor = n
		}
		explicit = true
	}
	if !explicit {
		cursor, _ = s.store.LastEventSeq(id)
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	select {
	case s.streamSem <- struct{}{}:
		defer func() { <-s.streamSem }()
	default:
		writeErr(w, http.StatusServiceUnavailable, "实时连接数已达上限，请稍后重试")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprintf(w, "retry: 2000\n\n")
	// Report the session's true latest seq, not the cursor: the page uses it as
	// a safety net to fetch anything that landed between its first page and the
	// stream connecting. Reporting the cursor made that check impossible.
	latest := cursor
	if n, err := s.store.LastEventSeq(id); err == nil && n > latest {
		latest = n
	}
	fmt.Fprintf(w, "event: hello\ndata: {\"session_id\":%q,\"last_seq\":%d}\n\n", id, latest)
	fl.Flush()

	lastTaskFP := ""
	lastBeat := time.Now()
	taskTick := time.Now()
	idle := 0
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		evs, err := s.store.EventsAfter(id, cursor, 400)
		if err != nil {
			// A transient store error must not silently end the live link: the
			// browser would keep a dead connection until its own timeout. Tell
			// it to reconnect instead.
			s.logger.Printf("stream %s: %v", id, err)
			fmt.Fprintf(w, "event: error\ndata: {\"error\":\"event stream interrupted\"}\n\n")
			fl.Flush()
			return
		}
		for _, e := range evs {
			b, _ := json.Marshal(map[string]any{
				"seq": e.Seq, "task_id": e.TaskID, "kind": e.Kind,
				"payload": clipPayload(e.Kind, e.Seq, e.Payload), "ts": e.TS,
			})
			fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", e.Seq, b)
			cursor = e.Seq
		}
		if len(evs) > 0 {
			fl.Flush()
		}
		if time.Since(taskTick) > 1200*time.Millisecond {
			taskTick = time.Now()
			tasks, _ := s.store.TasksForSession(id, 50)
			var active []*Task
			for _, t := range tasks {
				switch t.Status {
				case StatusRunning, StatusWaitingInput, StatusPending:
					// The page renders "前面还有 N 个" from these fields. Without
					// them the live push would blank the values the poll just set.
					if pos, ahead, err := s.store.QueueWait(t.ID); err == nil {
						t.QueuePosition, t.QueueAhead = pos, ahead
					}
					active = append(active, t)
				}
			}
			perms, _ := s.store.PendingPermissions(id)
			payload := map[string]any{"tasks": active, "permissions": perms,
				"session_status": activeStatus(active)}
			b, _ := json.Marshal(payload)
			fp := string(b)
			if fp != lastTaskFP {
				lastTaskFP = fp
				fmt.Fprintf(w, "event: tasks\ndata: %s\n\n", b)
				fl.Flush()
				lastBeat = time.Now()
			}
		}
		if time.Since(lastBeat) > 20*time.Second {
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
			lastBeat = time.Now()
		}
		// Poll quickly while events are flowing and back off when idle. Every
		// open stream shares the single DB connection, so a page left open on a
		// finished conversation should not cost four queries a second forever.
		if len(evs) > 0 {
			idle = 0
		} else if idle < 6 {
			idle++
		}
		delay := 350 * time.Millisecond
		switch idle {
		case 3:
			delay = 700 * time.Millisecond
		case 4:
			delay = 1400 * time.Millisecond
		case 5:
			delay = 2500 * time.Millisecond
		case 6:
			delay = 3 * time.Second
		}
		time.Sleep(delay)
	}
}

func activeStatus(tasks []*Task) string {
	waiting, running, pending := 0, 0, 0
	for _, t := range tasks {
		switch t.Status {
		case StatusWaitingInput:
			waiting++
		case StatusRunning:
			running++
		case StatusPending:
			pending++
		}
	}
	if waiting > 0 {
		return StatusWaitingInput
	}
	if running > 0 {
		return StatusRunning
	}
	if pending > 0 {
		return StatusPending
	}
	return "idle"
}

// Thinking text and tool output are most of a long transcript's bytes but stay
// collapsed on the page. Event lists carry a short preview of those fields; the
// page fetches the full event (handleEvent) when a block is expanded. Opening
// the largest session used to transfer 1–2.5 MB of mostly unseen text.
const (
	clipFieldBytes   = 2048
	clipPreviewRunes = 300
)

var clipFields = map[string][]string{
	"thinking":    {"text"},
	"tool_result": {"output"},
	"tool":        {"output"},
}

// clipPayload replaces oversized fields with a marker the page understands:
// {"__clipped": true, "seq", "field", "bytes", "preview"}.
func clipPayload(kind string, seq int64, payload json.RawMessage) json.RawMessage {
	fields := clipFields[kind]
	if len(fields) == 0 || len(payload) <= clipFieldBytes {
		return payload
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return payload
	}
	changed := false
	for _, f := range fields {
		raw, ok := m[f]
		if !ok || len(raw) <= clipFieldBytes {
			continue
		}
		marker, err := json.Marshal(map[string]any{
			"__clipped": true, "seq": seq, "field": f, "bytes": len(raw), "preview": clipPreview(raw),
		})
		if err != nil {
			continue
		}
		m[f] = marker
		changed = true
	}
	if !changed {
		return payload
	}
	b, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return b
}

func clipPreview(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	n := 0
	for i := range s {
		if n == clipPreviewRunes {
			return s[:i]
		}
		n++
	}
	return s
}

// clipEvents clips the payloads of events about to be listed, in place.
func clipEvents(evs []*Event) []*Event {
	for _, e := range evs {
		e.Payload = clipPayload(e.Kind, e.Seq, e.Payload)
	}
	return evs
}
