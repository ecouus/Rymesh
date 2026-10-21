package main

// AgentProfile describes one permission/sandbox mode that maps directly onto a
// flag supported by the installed CLI. IDs are what the API stores on a task.
type AgentProfile struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Detail    string `json:"detail"`
	Dangerous bool   `json:"dangerous,omitempty"`
}

var codexProfiles = []AgentProfile{
	{ID: "read-only", Label: "只读沙箱", Detail: "可读取文件，不能写入或修改系统"},
	{ID: "workspace-write", Label: "工作区可写", Detail: "可在工作目录内写入，工作目录外仍受限"},
	{ID: "danger-full-access", Label: "危险：完全访问", Detail: "关闭沙箱，可读写整台服务器", Dangerous: true},
}

var claudeProfiles = []AgentProfile{
	{ID: "manual", Label: "逐次审批", Detail: "需要授权的操作在网页请求批准"},
	{ID: "acceptEdits", Label: "自动接受编辑", Detail: "文件编辑自动通过，其它操作仍请求批准"},
	{ID: "plan", Label: "计划模式", Detail: "只做只读探索与规划，不执行修改"},
	{ID: "auto", Label: "自动", Detail: "由 Claude Code 的 auto 模式自行判定权限"},
	{ID: "dontAsk", Label: "不询问", Detail: "需要询问的操作直接拒绝，不弹出审批"},
	{ID: "bypassPermissions", Label: "危险：绕过所有权限", Detail: "跳过所有权限检查，可执行任意命令", Dangerous: true},
}

func profilesFor(agent string) []AgentProfile {
	switch agent {
	case "codex":
		return codexProfiles
	case "claude":
		return claudeProfiles
	}
	return nil
}

func defaultProfileFor(agent string) string {
	switch agent {
	case "codex":
		return "read-only"
	case "claude":
		return "manual"
	}
	return "read-only"
}

func validProfile(agent, id string) bool {
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
