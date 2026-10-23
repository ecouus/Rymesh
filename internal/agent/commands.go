package agent

import (
	"strings"
)

// SlashCommand splits "/model sonnet" into ("model", "sonnet").
func SlashCommand(text string) (name, arg string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") || strings.HasPrefix(t, "//") {
		return "", "", false
	}
	name, arg, _ = strings.Cut(t[1:], " ")
	if name == "" || strings.ContainsAny(name, "/\n") {
		return "", "", false
	}
	return strings.ToLower(name), strings.TrimSpace(arg), true
}

// CodexCommands are the Codex CLI commands the console implements.
var CodexCommands = []CommandInfo{
	{Name: "model", Description: "选择模型和推理强度", Args: "[模型] [强度]"},
	{Name: "effort", Description: "设置推理强度", Args: "<low|medium|high|xhigh|max|ultra>"},
	{Name: "compact", Description: "压缩对话上下文，节省上下文窗口"},
	{Name: "review", Description: "审查当前未提交的改动（可附加说明）", Args: "[说明]"},
	{Name: "status", Description: "显示当前会话的模型、权限与目录"},
	{Name: "new", Description: "开始新的对话"},
	{Name: "clear", Description: "开始新的对话（清空上下文）"},
	{Name: "rename", Description: "重命名对话", Args: "<名称>"},
}

// Descriptions for Claude's built-in commands; skills and custom commands
// from the CLI's own list show by name.
var ClaudeCommandText = map[string]string{
	"model": "切换模型", "effort": "设置推理强度", "compact": "压缩对话上下文",
	"clear": "开始新的对话（清空上下文）", "new": "开始新的对话", "context": "查看上下文占用",
	"cost": "查看用量", "usage": "查看用量", "init": "生成 CLAUDE.md 项目说明",
	"review": "审查代码改动", "rename": "重命名对话", "mcp": "查看 MCP 服务器",
	"agents": "查看子代理", "config": "查看配置", "memory": "查看记忆", "status": "查看状态",
	"output-style": "设置输出风格", "security-review": "安全审查", "pr-comments": "查看 PR 评论",
	"todos": "查看待办", "release-notes": "查看更新说明", "doctor": "诊断安装",
}

type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Args        string `json:"args,omitempty"`
}

const ClaudeCommandsMeta = "slash_commands.claude"

// HermesCommandsMeta keeps the commands Hermes last announced over ACP
// (available_commands_update), as [{name, description}].
const HermesCommandsMeta = "slash_commands.hermes"

// claudeSettingFrom recognises a /model or /effort the CLI confirmed. Only a
// confirmation is saved, so a name the CLI rejects never sticks.
func claudeSettingFrom(prompt, result string) (map[string]any, bool) {
	name, arg, ok := SlashCommand(prompt)
	if !ok || arg == "" {
		return nil, false
	}
	switch {
	case name == "model" && strings.HasPrefix(result, "Set model to"):
		if arg == "default" {
			arg = ""
		}
		return map[string]any{"model": arg}, true
	case name == "effort" && strings.HasPrefix(result, "Set effort level to"):
		if arg == "auto" {
			arg = ""
		}
		return map[string]any{"effort": arg}, true
	}
	return nil, false
}
