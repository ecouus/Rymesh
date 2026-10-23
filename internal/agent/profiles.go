package agent

// AgentProfile describes one permission/sandbox mode that maps directly onto a
// flag supported by the installed CLI. IDs are what the API stores on a task.
type AgentProfile struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Detail    string `json:"detail"`
	Dangerous bool   `json:"dangerous,omitempty"`
}

// Codex modes mirror the CLI's: the sandbox decides what runs without asking,
// and anything beyond it is sent to the page for approval (codexModes).
var CodexProfiles = []AgentProfile{
	{ID: "read-only", Label: "只读", Detail: "可读取文件；写入或联网等操作逐次请求批准"},
	{ID: "workspace-write", Label: "自动", Detail: "可在工作目录内读写；超出范围的操作请求批准"},
	{ID: "plan", Label: "计划模式", Detail: "只读探索并提问澄清，产出计划后再决定是否执行"},
	{ID: "danger-full-access", Label: "危险：完全访问", Detail: "关闭沙箱且不再请求批准，可读写整台机器", Dangerous: true},
}

var ClaudeProfiles = []AgentProfile{
	{ID: "manual", Label: "逐次审批", Detail: "需要授权的操作在网页请求批准"},
	{ID: "acceptEdits", Label: "自动接受编辑", Detail: "文件编辑自动通过，其它操作仍请求批准"},
	{ID: "plan", Label: "计划模式", Detail: "只做只读探索与规划，不执行修改"},
	{ID: "auto", Label: "自动", Detail: "由 Claude Code 的 auto 模式自行判定权限"},
	{ID: "dontAsk", Label: "不询问", Detail: "需要询问的操作直接拒绝，不弹出审批"},
	{ID: "bypassPermissions", Label: "危险：绕过所有权限", Detail: "跳过所有权限检查，可执行任意命令", Dangerous: true},
}

// Hermes profiles are its ACP session modes.
var HermesProfiles = []AgentProfile{
	{ID: "default", Label: "逐次审批", Detail: "编辑文件等操作在网页请求批准"},
	{ID: "accept_edits", Label: "自动接受编辑", Detail: "工作目录与临时目录内的编辑自动通过，敏感路径仍请求批准"},
	{ID: "dont_ask", Label: "不询问编辑", Detail: "本会话的文件编辑都自动通过（敏感路径除外）"},
}

func profilesFor(agent string) []AgentProfile {
	switch agent {
	case "codex":
		return CodexProfiles
	case "claude":
		return ClaudeProfiles
	case "hermes":
		return HermesProfiles
	}
	return nil
}

func DefaultProfileFor(agent string) string {
	switch agent {
	case "codex":
		return "read-only"
	case "claude":
		return "manual"
	case "hermes":
		return "default"
	}
	return "read-only"
}

func ValidProfile(agent, id string) bool {
	for _, p := range profilesFor(agent) {
		if p.ID == id {
			return true
		}
	}
	return false
}

func profileLabel(agent, id string) string {
	for _, p := range profilesFor(agent) {
		if p.ID == id {
			return p.Label
		}
	}
	return id
}
