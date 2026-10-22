package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The subprocess emulates the official CLI's stdio protocol, not a provider API.
func TestModelCLIHelper(t *testing.T) {
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		return
	}
	dir, agent := args[0], args[1]
	f, err := os.OpenFile(filepath.Join(dir, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintln(f, agent)
	_ = f.Close()
	var fixture struct {
		Result json.RawMessage `json:"result"`
		Page   json.RawMessage `json:"page"`
		Error  string          `json:"error"`
		Stall  bool            `json:"stall"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	read := func() map[string]any {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if agent == "codex" {
		if !reflect.DeepEqual(args[2:], []string{"app-server"}) {
			t.Fatalf("args: %v", args)
		}
		if m := read(); m["method"] != "initialize" {
			t.Fatalf("initialize: %v", m)
		}
		if fixture.Stall {
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
		_ = enc.Encode(map[string]any{"id": 1, "result": map[string]any{}})
		if m := read(); m["method"] != "initialized" {
			t.Fatalf("initialized: %v", m)
		}
		if m := read(); m["method"] != "model/list" {
			t.Fatalf("model/list: %v", m)
		}
		if fixture.Error != "" {
			_ = enc.Encode(map[string]any{"id": 2, "error": map[string]any{"message": fixture.Error}})
		} else {
			_ = enc.Encode(map[string]any{"id": 2, "result": fixture.Result})
			if fixture.Page != nil {
				m := read()
				params, _ := m["params"].(map[string]any)
				if m["method"] != "model/list" || params["cursor"] != "page-2" {
					t.Fatalf("page: %v", m)
				}
				_ = enc.Encode(map[string]any{"id": 2, "result": fixture.Page})
			}
		}
	} else {
		want := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence",
			"--strict-mcp-config", "--settings", `{"disableAllHooks":true}`}
		if !reflect.DeepEqual(args[2:], want) {
			t.Fatalf("args: %v", args)
		}
		m := read()
		request, _ := m["request"].(map[string]any)
		if m["type"] != "control_request" || request["subtype"] != "initialize" {
			t.Fatalf("initialize: %v", m)
		}
		if fixture.Stall {
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
		// Ignore notifications and responses for another request.
		_ = enc.Encode(map[string]any{"type": "system", "subtype": "init"})
		_ = enc.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "unrelated"}})
		response := map[string]any{"subtype": "success", "request_id": m["request_id"], "response": fixture.Result}
		if fixture.Error != "" {
			response["subtype"], response["error"] = "error", fixture.Error
		}
		_ = enc.Encode(map[string]any{"type": "control_response", "response": response})
	}
	// Stay alive until the caller closes input; cleanup must not wait indefinitely.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func modelCLIFixture(t *testing.T, agent string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	bin := filepath.Join(dir, "agent")
	script := "#!/bin/sh\nexec " + quote(os.Args[0]) + " -test.run=^TestModelCLIHelper$ -- " + quote(dir) + " " + quote(agent) + " \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func writeModelFixture(t *testing.T, dir, fixture string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
}

func modelFixture(agent, id string) string {
	if agent == "codex" {
		return fmt.Sprintf(`{"result":{"data":[{"id":%q,"displayName":"Node model","supportedReasoningEfforts":[{"reasoningEffort":"high"}]},{"id":"hidden","hidden":true}]}}`, id)
	}
	return fmt.Sprintf(`{"result":{"models":[{"value":"default","displayName":"CLI default","supportedEffortLevels":["low"]},{"value":%q,"displayName":"Node model","description":"From CLI","supportedEffortLevels":["high"]}]}}`, id)
}

func TestModelListsQuerySelectedNodeEveryTime(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			bin, dir := modelCLIFixture(t, agent)
			cfg := testConfig(t)
			cfg.NodeToken = "node-secret"
			st, err := openStore(cfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			node := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
			node.agents = &AgentSet{at: time.Now(), agents: []AgentInfo{{Name: agent, Binary: bin, Available: true}}}
			ns := httptest.NewServer(node.Handler())
			defer ns.Close()
			hubCfg := testConfig(t)
			hubCfg.Nodes = []NodeConfig{{ID: "selected", Name: "Selected", URL: ns.URL, Token: "node-secret"}}
			hubStore, err := openStore(hubCfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer hubStore.Close()
			hub := newServer(hubCfg, "", hubStore, log.New(io.Discard, "", 0), nil)
			// There is deliberately no CLI available on the hub.
			hub.agents = &AgentSet{at: time.Now(), agents: []AgentInfo{}}
			query := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", "/n/selected/api/agents/"+agent+"/models", nil)
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: hub.auth.token()})
				w := httptest.NewRecorder()
				hub.Handler().ServeHTTP(w, r)
				return w
			}
			for _, id := range []string{"first-model", "changed-model"} {
				writeModelFixture(t, dir, modelFixture(agent, id))
				w := query()
				if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("response: %d %s %v", w.Code, w.Body, w.Header())
				}
				var result struct {
					Models []modelChoice `json:"models"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				choices := result.Models
				if agent == "claude" {
					if len(choices) != 2 || choices[0].ID != "" || !choices[0].Default || !reflect.DeepEqual(choices[0].Efforts, []string{"low"}) {
						t.Fatalf("Claude default: %+v", choices)
					}
					choices = choices[1:]
				}
				if len(choices) != 1 || choices[0].ID != id || !reflect.DeepEqual(choices[0].Efforts, []string{"high"}) {
					t.Fatalf("models: %+v", choices)
				}
			}
			b, _ := os.ReadFile(filepath.Join(dir, "starts"))
			if strings.Count(string(b), agent+"\n") != 2 {
				t.Fatalf("CLI not restarted: %q", b)
			}
			// A CLI failure after success must never serve the previous list.
			writeModelFixture(t, dir, `{"error":"CLI model discovery rejected"}`)
			if w := query(); w.Code != 502 || !strings.Contains(w.Body.String(), "CLI model discovery rejected") || strings.Contains(w.Body.String(), "changed-model") {
				t.Fatalf("failure fallback: %d %s", w.Code, w.Body)
			}
			writeModelFixture(t, dir, `{"result":{}}`)
			if w := query(); w.Code != 502 {
				t.Fatalf("missing models: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestCodexModelListPagination(t *testing.T) {
	bin, dir := modelCLIFixture(t, "codex")
	writeModelFixture(t, dir, `{"result":{"data":[{"id":"a"}],"nextCursor":"page-2"},"page":{"data":[{"id":"b"}]}}`)
	models, err := codexModelList(context.Background(), bin)
	if err != nil || len(models) != 2 || models[0].ID != "a" || models[1].ID != "b" {
		t.Fatalf("pages: %+v %v", models, err)
	}
}

func TestModelListCancellation(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			bin, dir := modelCLIFixture(t, agent)
			writeModelFixture(t, dir, `{"stall":true}`)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			query := codexModelList
			if agent == "claude" {
				query = claudeModelList
			}
			_, err := query(ctx, bin)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
				t.Fatalf("cancellation: %v after %s", err, time.Since(start))
			}
		})
	}
}

func TestRealCLIModelLists(t *testing.T) {
	if os.Getenv("RYMESH_TEST_REAL_MODEL_CLIS") != "1" {
		t.Skip("opt-in check using locally installed official CLIs")
	}
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			bin, err := exec.LookPath(agent)
			if err != nil {
				t.Fatal(err)
			}
			query := codexModelList
			if agent == "claude" {
				query = claudeModelList
			}
			models, err := query(context.Background(), bin)
			if err != nil || len(models) == 0 {
				t.Fatalf("%s models: %+v %v", agent, models, err)
			}
			t.Logf("%s returned %d models", agent, len(models))
		})
	}
}
