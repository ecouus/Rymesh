package importer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"rymesh/internal/agent"
	"rymesh/internal/ingest"
	"rymesh/internal/store"
	"rymesh/internal/util"
)

// Newer Codex versions keep threads in a SQLite store and migrate old rollout
// files into it, so most conversations no longer have a transcript file. The
// CLI still serves them: `codex app-server` lists threads (thread/list) and
// returns their turns with every item (thread/turns/list). Threads that still
// have a rollout file are left to the file importer, which follows them line
// by line.

// codexStoreKey stamps Codex' thread databases; an unchanged stamp means no
// thread changed since the last pass.
func (im *Importer) codexStoreKey() (fileKey, bool) {
	var k fileKey
	found := false
	for _, pat := range []string{"state_*.sqlite", "thread_history_*.sqlite"} {
		matches, _ := filepath.Glob(filepath.Join(im.home, ".codex", pat))
		for _, db := range matches {
			for _, f := range []string{db, db + "-wal"} {
				if s, ok := statKey(f); ok {
					found = true
					k.size += s.size
					k.mtime = max(k.mtime, s.mtime)
				}
			}
		}
	}
	return k, found
}

func (im *Importer) importCodexThreads(rep *ImportReport) {
	if agent.Agents == nil {
		return
	}
	info, ok := agent.Agents.Get("codex")
	if !ok || !info.Available || info.Binary == "" {
		return
	}
	k, found := im.codexStoreKey()
	if !found {
		return // a Codex that keeps only rollout files
	}
	const key = "codex:threads"
	im.stampMu.Lock()
	old, seen := im.stamps[key]
	im.stampMu.Unlock()
	if seen && old == k {
		return
	}
	if err := im.importCodexThreadList(info.Binary, rep); err != nil {
		rep.Errors++
		if len(rep.ErrorSamples) < 5 {
			rep.ErrorSamples = append(rep.ErrorSamples, "codex app-server: "+err.Error())
		}
		return
	}
	im.remember(key, k)
}

// codexThread is the part of an app-server Thread the importer uses.
type codexThread struct {
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Preview   string `json:"preview"`
	Path      string `json:"path"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

type codexTurn struct {
	StartedAt   int64            `json:"startedAt"`
	CompletedAt int64            `json:"completedAt"`
	Items       []map[string]any `json:"items"`
	Error       *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (im *Importer) importCodexThreadList(bin string, rep *ImportReport) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := startCodexRPC(ctx, bin)
	if err != nil {
		return err
	}
	defer c.close()
	var threads []codexThread
	cursor := ""
	for {
		// Every provider: left out, Codex lists only threads of the provider
		// configured now, and one switched provider hides the rest.
		params := map[string]any{"limit": 100, "useStateDbOnly": true, "modelProviders": []string{},
			"sourceKinds": []string{"cli", "vscode", "exec", "appServer"}}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []codexThread `json:"data"`
			NextCursor string        `json:"nextCursor"`
		}
		if err := c.call("thread/list", params, &page); err != nil {
			return err
		}
		threads = append(threads, page.Data...)
		if page.NextCursor == "" || len(page.Data) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	rep.CodexThreads = len(threads)
	for _, t := range threads {
		if err := im.importCodexThread(c, t, rep); err != nil {
			rep.Errors++
			if len(rep.ErrorSamples) < 5 {
				rep.ErrorSamples = append(rep.ErrorSamples, "codex "+t.ID+": "+err.Error())
			}
		}
	}
	return nil
}

func (im *Importer) importCodexThread(c *codexRPC, t codexThread, rep *ImportReport) error {
	if t.ID == "" || (t.Path != "" && util.FileExists(t.Path)) {
		return nil // the file importer has it
	}
	if st, err := os.Stat(t.Cwd); t.Cwd == "" || err != nil || !st.IsDir() {
		rep.Skipped++ // its directory is gone; it could not run again here
		return nil
	}
	if im.excluded(t.Cwd) {
		rep.Excluded++
		return nil
	}
	if im.store.GetMeta(DismissedKey("codex", "", t.ID)) != "" {
		rep.Skipped++ // deleted in the console; Codex keeps its own copy
		return nil
	}
	stamp := fmt.Sprintf("codexdb:%s/%d", t.ID, t.UpdatedAt)
	// Decide from the row alone when nothing changed, before reading turns.
	existing, err := im.store.FindNativeSession("codex", t.ID)
	if err != nil && !store.IsNoRowsErr(err) {
		return err
	}
	if existing != nil && store.ParseImportState(existing.Meta).Path == stamp {
		rep.Skipped++
		return nil
	}
	var turns []codexTurn
	cursor := ""
	for {
		params := map[string]any{"threadId": t.ID, "itemsView": "full", "sortDirection": "asc", "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []codexTurn `json:"data"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := c.call("thread/turns/list", params, &page); err != nil {
			// Codex cannot read some migrated threads back ("missing source
			// rollout"); there is nothing to show for those.
			if strings.Contains(err.Error(), "missing source rollout") {
				rep.Skipped++
				return nil
			}
			return err
		}
		turns = append(turns, page.Data...)
		if page.NextCursor == "" || len(page.Data) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if existing != nil {
		if has, _ := im.store.HasNonImportEvents(existing.ID, importTaskID(existing.ID)); has || existing.Source == "web" {
			return im.appendCodexTurns(existing, turns, stamp, rep)
		}
	}
	var events []store.ImportedEvent
	firstUser := ""
	for _, turn := range turns {
		events = append(events, codexTurnEvents(turn, &firstUser)...)
	}
	title := strings.TrimSpace(t.Name)
	if title == "" {
		title = store.TruncateTitle(firstNonEmpty(firstUser, t.Preview), 72)
	}
	if title == "" {
		title = fmt.Sprintf("Codex 会话 %s", shortID(t.ID))
	}
	created := firstNonEmpty(unixTS(t.CreatedAt), firstTS(events))
	updated := firstNonEmpty(unixTS(t.UpdatedAt), created)
	return im.storeSnapshot(snapshot{agent: "codex", nativeID: t.ID, cwd: t.Cwd, title: title,
		created: created, updated: updated, stamp: stamp, events: events, offset: int64(len(turns))}, rep)
}

// appendCodexTurns brings a conversation the console has run turns in up to
// date: only turns after the ones already read are added, and those the
// console ran itself (already shown from its task log) are left out.
// Replacing the history would renumber it after the console's turns.
func (im *Importer) appendCodexTurns(sess *store.Session, turns []codexTurn, stamp string, rep *ImportReport) error {
	st := store.ParseImportState(sess.Meta)
	from := int(st.Offset)
	if from > len(turns) {
		from = len(turns)
	}
	windows, _ := im.store.ConsoleTaskWindows(sess.ID)
	var events []store.ImportedEvent
	firstUser := ""
	for _, turn := range turns[from:] {
		if consoleTurn(turn, windows) {
			continue
		}
		events = append(events, codexTurnEvents(turn, &firstUser)...)
	}
	updated := util.LaterTS(st.Updated, sess.LastMessageAt)
	for _, e := range events {
		updated = util.LaterTS(updated, e.TS)
	}
	taskID := importTaskID(sess.ID)
	seq, err := im.store.LastImportedSeq(sess.ID, taskID)
	if err != nil {
		return err
	}
	next := store.ImportState{Path: stamp, Offset: int64(len(turns)), Updated: updated}
	if len(events) == 0 {
		next.Seq = seq
		rep.Skipped++
		return im.store.SetImportState(sess.ID, next)
	}
	nextSeq, err := im.store.AppendImportedEvents(sess.ID, taskID, seq, events, updated, "", next)
	if err != nil {
		return err
	}
	next.Seq = nextSeq
	rep.Updated++
	return im.store.SetImportState(sess.ID, next)
}

// consoleTurn reports whether a turn ran inside one of the console's task
// windows. Codex stamps turns in whole seconds, so the window is widened a
// little on both sides.
func consoleTurn(turn codexTurn, windows []store.TaskWindow) bool {
	if turn.StartedAt <= 0 {
		return false
	}
	at := time.Unix(turn.StartedAt, 0)
	for _, w := range windows {
		start, err := time.Parse(time.RFC3339Nano, w.Start)
		if err != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339Nano, w.End)
		if err != nil {
			end = time.Now().Add(time.Minute) // still running
		}
		if !at.Before(start.Add(-2*time.Second)) && !at.After(end.Add(2*time.Second)) {
			return true
		}
	}
	return false
}

// codexTurnEvents converts one turn. Items carry no time of their own: the
// user's message takes the turn's start, everything else its end.
func codexTurnEvents(turn codexTurn, firstUser *string) []store.ImportedEvent {
	start := unixTS(turn.StartedAt)
	end := firstNonEmpty(unixTS(turn.CompletedAt), start)
	var out []store.ImportedEvent
	for _, item := range turn.Items {
		if util.Str(item["type"]) == "userMessage" {
			var parts []string
			list, _ := item["content"].([]any)
			for _, x := range list {
				if m, _ := x.(map[string]any); util.Str(m["type"]) == "text" {
					parts = append(parts, util.Str(m["text"]))
				}
			}
			txt := strings.TrimSpace(strings.Join(parts, "\n"))
			if txt == "" || internalUserText(txt) {
				continue
			}
			if *firstUser == "" {
				*firstUser = txt
			}
			out = append(out, store.ImportedEvent{TS: start, Kind: "user", Payload: map[string]any{"text": txt}})
			continue
		}
		for _, e := range ingest.CodexItemEvents(item) {
			e.TS = end
			out = append(out, e)
		}
	}
	if turn.Error != nil && turn.Error.Message != "" {
		out = append(out, store.ImportedEvent{TS: end, Kind: "error", Payload: map[string]any{"text": turn.Error.Message}})
	}
	return out
}

func unixTS(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// codexRPC is a minimal client for `codex app-server` on stdio: one request at
// a time, notifications ignored.
type codexRPC struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Scanner
	nextID int
}

func startCodexRPC(ctx context.Context, bin string) (*codexRPC, error) {
	cmd := exec.CommandContext(ctx, bin, "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 256<<20)
	c := &codexRPC{cmd: cmd, stdin: stdin, out: sc}
	if err := c.call("initialize", map[string]any{
		"clientInfo": map[string]any{"name": "rymesh", "version": util.Version}}, nil); err != nil {
		c.close()
		return nil, err
	}
	if err := c.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *codexRPC) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *codexRPC) call(method string, params any, result any) error {
	c.nextID++
	id := c.nextID
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for c.out.Scan() {
		var m struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(c.out.Bytes(), &m) != nil || m.ID == nil || *m.ID != id || m.Method != "" {
			continue // notifications, or a request we do not serve
		}
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
	if err := c.out.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s: codex app-server exited", method)
}

func (c *codexRPC) close() {
	_ = c.stdin.Close()
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
}
