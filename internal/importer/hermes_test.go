package importer

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rymesh/internal/agent"
	"rymesh/internal/store"
)

// fakeHermes installs a stand-in `hermes` that prints the given export for
// each profile ("" is the default one) and logs its arguments.
func fakeHermes(t *testing.T, home string, exports map[string][]map[string]any) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\nprofile=default\n[ \"$1\" = -p ] && profile=$2\ncat " + dir + "/$profile.jsonl\n"
	for profile, sessions := range exports {
		name := profile
		db := filepath.Join(home, ".hermes", "profiles", profile, "state.db")
		if profile == "" {
			name, db = "default", filepath.Join(home, ".hermes", "state.db")
		}
		var b strings.Builder
		for _, s := range sessions {
			line, _ := json.Marshal(s)
			b.Write(append(line, '\n'))
		}
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(db, []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "hermes")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	old := agent.Agents
	agent.Agents = agent.FixedAgentSet(agent.AgentInfo{Name: "hermes", Binary: bin, Available: true})
	t.Cleanup(func() { agent.Agents = old })
	return calls
}

func hermesMsg(role, content string, ts float64, extra map[string]any) map[string]any {
	m := map[string]any{"role": role, "content": content, "timestamp": ts, "active": 1}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// Hermes history comes from `hermes sessions export`: ACP sessions can be
// continued, gateway chats are read-only and keep their profile, a
// compressed conversation is one entry ending in its newest session, cron
// runs are left out, and an unchanged database is not exported again.
func TestImportHermes(t *testing.T) {
	home := t.TempDir()
	calls := fakeHermes(t, home, map[string][]map[string]any{
		"": {{"id": "acp-1", "source": "acp", "cwd": home, "title": "fix it", "started_at": 1792000000.0,
			"messages": []any{
				hermesMsg("user", "please fix", 1792000001, nil),
				hermesMsg("assistant", "", 1792000002, map[string]any{"tool_calls": []any{map[string]any{
					"call_id": "c1", "function": map[string]any{"name": "terminal", "arguments": `{"command":"ls"}`}}}}),
				hermesMsg("tool", `{"output":"a.txt"}`, 1792000003, map[string]any{"tool_call_id": "c1"}),
				hermesMsg("assistant", "fixed", 1792000004, map[string]any{"reasoning": "look first"}),
				hermesMsg("assistant", "rewound", 1792000005, map[string]any{"active": 0}),
			}}},
		"bot-a": {
			{"id": "wx-1", "source": "weixin", "started_at": 1791999000.0, "messages": []any{
				hermesMsg("user", "你好", 1791999001, nil), hermesMsg("assistant", "我是 Nano Bot", 1791999002, nil)}},
			{"id": "wx-2", "source": "weixin", "parent_session_id": "wx-1", "started_at": 1791999100.0, "messages": []any{
				hermesMsg("user", "提醒我拿快递", 1791999101, nil)}},
			{"id": "grp-1", "source": "desktop", "title": "Group: rm-1 · tm-2", "started_at": 1791999150.0, "messages": []any{
				hermesMsg("user", `[Group chat: "Team-Room"] You are @bot-a, one participant`, 1791999151, nil)}},
			{"id": "cron-1", "source": "cron", "started_at": 1791999200.0, "messages": []any{
				hermesMsg("user", "reminder", 1791999201, nil)}},
		},
	})
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	if rep := im.ImportAll(); rep.Imported != 3 || rep.Errors != 0 {
		t.Fatalf("first scan = %+v", rep)
	}
	acp, err := st.FindNativeSession("hermes", "acp-1")
	if err != nil || acp.ReadOnly != "" || acp.NativeProfile != "" || acp.Title != "fix it" {
		t.Fatalf("acp session = %+v %v", acp, err)
	}
	evs, _ := st.RecentEvents(acp.ID, 100)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "user,tool,tool_result,thinking,assistant" {
		t.Fatalf("acp events = %s", got)
	}
	wx, err := st.FindNativeSession("hermes", "wx-2")
	if err != nil || wx.NativeProfile != "bot-a" || !strings.Contains(wx.ReadOnly, "微信") || wx.Cwd != home {
		t.Fatalf("weixin session = %+v %v", wx, err)
	}
	if evs, _ := st.RecentEvents(wx.ID, 100); len(evs) != 3 {
		t.Fatalf("weixin events = %d, want both sessions of the chain", len(evs))
	}
	if g, err := st.FindNativeSession("hermes", "grp-1"); err != nil || g.Title != "Team-Room · 群聊（bot-a）" {
		t.Fatalf("group chat = %+v %v", g, err)
	}
	if _, err := st.FindNativeSession("hermes", "cron-1"); err == nil {
		t.Fatal("cron run imported")
	}

	before, _ := os.ReadFile(calls)
	if rep := im.ImportAll(); rep.Imported != 0 || rep.Updated != 0 {
		t.Fatalf("idle rescan = %+v", rep)
	}
	if after, _ := os.ReadFile(calls); string(after) != string(before) {
		t.Fatalf("unchanged profiles exported again: %s", after)
	}
	if !strings.Contains(string(before), "-p bot-a sessions export --format jsonl --redact -") {
		t.Fatalf("calls = %s", before)
	}

	// Deleted in the console: Hermes keeps the chat, the rescan leaves it out.
	if err := st.SetMeta(DismissedKey("hermes", "bot-a", "wx-2"), "x"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSessionRow(wx.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".hermes", "profiles", "bot-a", "state.db"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rep := im.ImportAll(); rep.Imported != 0 {
		t.Fatalf("rescan after delete = %+v", rep)
	}
}
