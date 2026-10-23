package importer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"rymesh/internal/agent"
	"rymesh/internal/store"
	"rymesh/internal/util"
)

// Hermes keeps its history in a SQLite database per profile, not in files the
// importer could read line by line. The CLI exports it itself
// (`hermes sessions export --format jsonl -`), with --redact so a secret
// pasted into a chat stays out of the console's copy.
//
// Only sessions Hermes created over ACP can be continued: its ACP adapter
// restores nothing else (gateway chats from WeChat, Telegram or the desktop
// app, terminal sessions). Those are imported read-only.

// DismissedKey marks a conversation the user deleted in the console whose
// history lives in the agent's own database (Hermes, Codex threads without a
// rollout file). The agent keeps it (a Hermes one may be a bot's live chat),
// so the rescan has to know not to bring it back.
func DismissedKey(agent, profile, nativeID string) string {
	return agent + "_dismissed:" + profile + ":" + nativeID
}

// hermesProfiles lists the default profile ("") and the named ones under
// ~/.hermes/profiles, with the database file whose stamp says whether the
// profile's history changed.
func (im *Importer) hermesProfiles() map[string]string {
	root := filepath.Join(im.home, ".hermes")
	out := map[string]string{}
	if _, err := os.Stat(filepath.Join(root, "state.db")); err == nil {
		out[""] = filepath.Join(root, "state.db")
	}
	entries, _ := os.ReadDir(filepath.Join(root, "profiles"))
	for _, e := range entries {
		// The name becomes a `hermes -p` argument; one that reads as a flag
		// is not a profile Hermes would have made.
		if strings.HasPrefix(e.Name(), "-") || strings.ContainsAny(e.Name(), " \t\n") {
			continue
		}
		db := filepath.Join(root, "profiles", e.Name(), "state.db")
		if _, err := os.Stat(db); e.IsDir() && err == nil {
			out[e.Name()] = db
		}
	}
	return out
}

func (im *Importer) importHermes(rep *ImportReport) {
	if agent.Agents == nil {
		return
	}
	info, ok := agent.Agents.Get("hermes")
	if !ok || !info.Available || info.Binary == "" {
		return
	}
	for profile, db := range im.hermesProfiles() {
		// The WAL file changes with every write; state.db itself only on
		// checkpoints.
		k, _ := statKey(db)
		if w, ok := statKey(db + "-wal"); ok {
			k.size, k.mtime = k.size+w.size, max(k.mtime, w.mtime)
		}
		key := "hermes:" + profile
		im.stampMu.Lock()
		old, seen := im.stamps[key]
		im.stampMu.Unlock()
		if seen && old == k {
			rep.Skipped++
			continue
		}
		rep.HermesProfiles++
		if err := im.importHermesProfile(info.Binary, profile, rep); err != nil {
			rep.Errors++
			if len(rep.ErrorSamples) < 5 {
				rep.ErrorSamples = append(rep.ErrorSamples, "hermes "+profileName(profile)+": "+err.Error())
			}
			continue
		}
		im.remember(key, k)
	}
}

// profileName names a Hermes profile for messages.
func profileName(p string) string {
	if p == "" {
		return "default"
	}
	return p
}

// hermesSession is the part of an exported session the console uses. The
// export carries much more (system prompt, model config); it is not kept.
type hermesSession struct {
	ID       string          `json:"id"`
	Source   string          `json:"source"`
	ChatType string          `json:"chat_type"`
	Parent   string          `json:"parent_session_id"`
	Title    string          `json:"title"`
	Cwd      string          `json:"cwd"`
	Started  float64         `json:"started_at"`
	Last     float64         `json:"last_activity_at"`
	Model    json.RawMessage `json:"model_config"`
	Messages []hermesMessage `json:"messages"`
}

type hermesMessage struct {
	Role       string  `json:"role"`
	Content    string  `json:"content"`
	ToolCallID string  `json:"tool_call_id"`
	ToolCalls  []any   `json:"tool_calls"`
	ToolName   string  `json:"tool_name"`
	Timestamp  float64 `json:"timestamp"`
	Reasoning  string  `json:"reasoning"`
	Active     *int    `json:"active"`
	Compacted  int     `json:"compacted"`
}

func (im *Importer) importHermesProfile(bin, profile string, rep *ImportReport) error {
	args := []string{"sessions", "export", "--format", "jsonl", "--redact", "-"}
	if profile != "" {
		args = append([]string{"-p", profile}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var sessions []*hermesSession
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 256<<20)
	for sc.Scan() {
		var hs hermesSession
		if json.Unmarshal(sc.Bytes(), &hs) == nil && hs.ID != "" {
			hs.Model = nil
			sessions = append(sessions, &hs)
		}
	}
	scanErr := sc.Err()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("sessions export: %w", err)
	}
	if scanErr != nil {
		return scanErr
	}
	for _, chain := range hermesChains(sessions) {
		if err := im.importHermesChain(profile, chain, rep); err != nil {
			rep.Errors++
			if len(rep.ErrorSamples) < 5 {
				rep.ErrorSamples = append(rep.ErrorSamples, "hermes "+chain[len(chain)-1].ID+": "+err.Error())
			}
		}
	}
	return nil
}

// hermesChains groups sessions into conversations. When Hermes compresses a
// long conversation it continues in a new session whose parent is the old
// one, from the same source; those read as one conversation, ending in the
// session that is resumed. Cron runs and delegated subagents are not
// conversations of their own and are left out.
func hermesChains(sessions []*hermesSession) [][]*hermesSession {
	byID := map[string]*hermesSession{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	skip := func(s *hermesSession) bool {
		return s.Source == "cron" || s.Source == "subagent"
	}
	continued := map[string]bool{} // has a compression child
	parentOf := func(s *hermesSession) *hermesSession {
		if p := byID[s.Parent]; p != nil && p.Source == s.Source {
			return p
		}
		return nil
	}
	for _, s := range sessions {
		if p := parentOf(s); p != nil && !skip(s) {
			continued[p.ID] = true
		}
	}
	var chains [][]*hermesSession
	for _, s := range sessions {
		if skip(s) || continued[s.ID] {
			continue
		}
		chain := []*hermesSession{s}
		seen := map[string]bool{s.ID: true}
		for p := parentOf(s); p != nil && !seen[p.ID]; p = parentOf(p) {
			seen[p.ID] = true
			chain = append([]*hermesSession{p}, chain...)
		}
		chains = append(chains, chain)
	}
	sort.Slice(chains, func(i, j int) bool { return chains[i][0].Started < chains[j][0].Started })
	return chains
}

var hermesGroupRe = regexp.MustCompile(`^\[Group chat: "([^"]{1,80})"\]`)

func hermesTS(f float64) string {
	if f <= 0 || math.IsNaN(f) {
		return ""
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC().Format("2006-01-02T15:04:05.000Z")
}

// hermesSourceLabels names where a read-only conversation came from.
var hermesSourceLabels = map[string]string{
	"weixin": "微信", "telegram": "Telegram", "discord": "Discord", "slack": "Slack",
	"whatsapp": "WhatsApp", "signal": "Signal", "email": "邮件", "desktop": "Hermes 桌面版",
	"cli": "Hermes 终端", "tui": "Hermes 终端", "oneshot": "单次命令",
}

func hermesReadOnly(s *hermesSession, profile string) string {
	if s.Source == "acp" {
		return ""
	}
	from := hermesSourceLabels[s.Source]
	if from == "" {
		from = firstNonEmpty(s.Source, "其他入口")
	}
	if profile != "" {
		from = fmt.Sprintf("%s（档案 %s）", from, profile)
	}
	return fmt.Sprintf("这段对话来自%s。Hermes 只能接着聊通过 ACP 创建的对话，这里只能查看。", from)
}

// hermesEvents converts a conversation's messages into console events.
func hermesEvents(chain []*hermesSession) ([]store.ImportedEvent, string) {
	var out []store.ImportedEvent
	firstUser := ""
	for _, s := range chain {
		for _, m := range s.Messages {
			if m.Active != nil && *m.Active == 0 && m.Compacted == 0 {
				continue // rewound
			}
			ts := hermesTS(m.Timestamp)
			switch m.Role {
			case "user":
				if t := strings.TrimSpace(m.Content); t != "" {
					if firstUser == "" {
						firstUser = t
					}
					out = append(out, store.ImportedEvent{TS: ts, Kind: "user", Payload: map[string]any{"text": t}})
				}
			case "assistant":
				if t := strings.TrimSpace(m.Reasoning); t != "" {
					out = append(out, store.ImportedEvent{TS: ts, Kind: "thinking", Payload: map[string]any{"text": t}})
				}
				if t := strings.TrimSpace(m.Content); t != "" {
					out = append(out, store.ImportedEvent{TS: ts, Kind: "assistant", Payload: map[string]any{"text": t}})
				}
				for _, c := range m.ToolCalls {
					call, _ := c.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					var input any = util.Str(fn["arguments"])
					var parsed map[string]any
					if json.Unmarshal([]byte(util.Str(fn["arguments"])), &parsed) == nil {
						input = parsed
					}
					out = append(out, store.ImportedEvent{TS: ts, Kind: "tool", Payload: map[string]any{
						"id":   firstNonEmpty(util.Str(call["call_id"]), util.Str(call["id"])),
						"name": firstNonEmpty(util.Str(fn["name"]), "tool"), "status": "completed", "input": input}})
				}
			case "tool":
				out = append(out, store.ImportedEvent{TS: ts, Kind: "tool_result",
					Payload: map[string]any{"id": m.ToolCallID, "output": m.Content}})
			}
		}
	}
	return out, firstUser
}

func (im *Importer) importHermesChain(profile string, chain []*hermesSession, rep *ImportReport) error {
	last := chain[len(chain)-1]
	rep.HermesSessions++
	if im.store.GetMeta(DismissedKey("hermes", profile, last.ID)) != "" {
		rep.Skipped++
		return nil
	}
	events, firstUser := hermesEvents(chain)
	cwd := chain[0].Cwd
	if st, err := os.Stat(cwd); cwd == "" || err != nil || !st.IsDir() {
		cwd = im.home // gateway chats have no working directory
	}
	if im.excluded(cwd) {
		rep.Excluded++
		return nil
	}
	title := strings.TrimSpace(last.Title)
	// Desktop group chats are titled by internal room ids; each bot's turn
	// opens with `[Group chat: "<room>"] You are @<bot>, ...`.
	if m := hermesGroupRe.FindStringSubmatch(firstUser); m != nil && (title == "" || strings.HasPrefix(title, "Group: ")) {
		title = fmt.Sprintf("%s · 群聊（%s）", m[1], profileName(profile))
	}
	if title == "" {
		title = store.TruncateTitle(firstUser, 72)
	}
	if title == "" {
		title = fmt.Sprintf("Hermes 会话 %s", shortID(last.ID))
	}
	created := firstTS(events)
	updated := created
	if len(events) > 0 {
		created = firstNonEmpty(hermesTS(chain[0].Started), created)
		updated = events[len(events)-1].TS
	}
	var prev []string
	for _, s := range chain {
		updated = util.LaterTS(updated, hermesTS(s.Last))
		if s != last {
			prev = append(prev, s.ID)
		}
	}
	return im.storeSnapshot(snapshot{
		agent: "hermes", nativeID: last.ID, cwd: cwd, title: title, created: created, updated: updated,
		stamp: fmt.Sprintf("hermes:%s/%d", last.ID, len(events)), events: events, prevIDs: prev,
		meta: map[string]string{"native_profile": profile, "read_only": hermesReadOnly(last, profile)},
	}, rep)
}
