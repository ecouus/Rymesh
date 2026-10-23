package ingest

import (
	"testing"
)

func TestNormalizeCodexRPC(t *testing.T) {
	ev := func(method string, params map[string]any) []normEvent {
		return normalizeCodex(map[string]any{"method": method, "params": params})
	}
	out := ev("item/completed", map[string]any{"item": map[string]any{"type": "commandExecution", "id": "c1",
		"command": "ls", "status": "completed", "aggregatedOutput": "a.txt", "exitCode": 0}})
	if len(out) != 1 || out[0].Kind != "tool" || out[0].Payload.(map[string]any)["output"] != "a.txt" {
		t.Fatalf("command = %+v", out)
	}
	if out = ev("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "hi"}}); out[0].Kind != "assistant" {
		t.Fatalf("message = %+v", out)
	}
	if out = ev("item/completed", map[string]any{"item": map[string]any{"type": "plan", "text": "# p"}}); out[0].Kind != "plan" {
		t.Fatalf("plan = %+v", out)
	}
	if out = ev("item/started", map[string]any{"item": map[string]any{"type": "agentMessage", "text": ""}}); len(out) != 0 {
		t.Fatalf("started message should be silent: %+v", out)
	}
	// Exec-format events from older task logs still normalize.
	if out = normalizeCodex(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "old"}}); out[0].Kind != "assistant" {
		t.Fatalf("legacy = %+v", out)
	}
}

func TestNormalizeACP(t *testing.T) {
	got := normalizeEvent("hermes", map[string]any{"acp": "assistant", "payload": map[string]any{"text": "hi"}}, "")
	if len(got) != 1 || got[0].Kind != "assistant" || got[0].Payload.(map[string]any)["text"] != "hi" {
		t.Fatalf("assistant = %+v", got)
	}
	got = normalizeEvent("hermes", map[string]any{"acp": "usage", "payload": map[string]any{
		"usage": map[string]any{"inputTokens": 5.0, "outputTokens": 2.0}}}, "")
	u := got[0].Payload.(map[string]any)["usage"].(map[string]any)
	if got[0].Kind != "usage" || u["input_tokens"] != 5.0 || u["output_tokens"] != 2.0 {
		t.Fatalf("usage = %+v", got)
	}
	if got := normalizeEvent("hermes", map[string]any{"acp": "commands", "payload": map[string]any{}}, ""); len(got) != 0 {
		t.Fatalf("commands should not be a transcript event: %+v", got)
	}
}
