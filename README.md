# Agent Console

通过 EasyTier 内网、在手机浏览器中管理服务器上的 **Codex CLI** 与 **Claude Code**。
Agent 任务由服务器独立托管：关闭浏览器、锁屏、断网或重启 Web 服务都不会终止正在执行的 Agent。

- 实际访问地址：**http://10.0.0.6:8899**
- 服务：`agentconsole.service`（systemd，已 enable，开机自启）
- 登录密码：默认 `admin`，首次登录强制修改（`agentconsole password` 可查看）

> **原则：本项目只是远程控制台。** 所有模型请求都由所选机器上的官方 CLI（`claude` / `codex`）
> 用那台机器自己的登录发出；本程序不调用任何模型 API、不转发模型流量、不读取或传输凭据。
> 请只用它控制你自己的机器，不要把控制台开放给他人使用你的账号（那属于账号共享，违反服务条款）。
> 开发约束见 [CLAUDE.md](CLAUDE.md)。

> 本 README 面向长期运维。完整测试记录见 [TEST_REPORT.md](TEST_REPORT.md)。

---

## 1. 功能概览

| 能力 | 状态 |
|---|---|
| Codex CLI 接入（新建 / 原生 resume） | ✅ `codex-cli 0.163.0` |
| Claude Code 接入（新建 / 原生 resume） | ✅ `2.1.303` |
| 多会话、多 Agent 并发（默认 4，可配 1–32） | ✅ |
| 同一会话串行调度（新消息自动排队） | ✅ |
| 后台独立执行，与浏览器/SSE 解耦 | ✅ |
| 服务端事件日志，SSE 断点续传 | ✅ |
| 工具调用可视化（名称/参数/状态/输出/错误） | ✅ |
| 与 CLI 一致的权限审批：允许一次 / 本会话允许 / 始终允许 / 拒绝（可附理由）/ 拒绝并停止（可跨断线） | ✅ Claude + Codex |
| Agent 提问（单选、多选、自填），计划模式的计划审批 | ✅ Claude（AskUserQuestion / ExitPlanMode）+ Codex（计划模式 request_user_input） |
| 原生历史导入（只读扫描，不改原文件；跳过空会话与子代理转录） | ✅ |
| 权限模式与 CLI 原生选项一致（按 Agent 切换） | ✅ |
| 重命名 / 删除对话（侧边栏三点菜单，删除含磁盘文件二次确认） | ✅ |
| 任务取消、手动重跑、状态恢复 | ✅ |
| 移动端 UI / Markdown / 代码高亮 / 深色模式 | ✅ |
| PWA manifest | ✅（HTTP 私网下 Service Worker 通常不可注册） |
| 基本认证 / CSRF / 登录锁定 / CSP / 首次登录改密 | ✅ |

---

## 2. 架构

```text
手机浏览器 (EasyTier)
      │ HTTP + SSE
      ▼
agentconsole serve  (systemd, 127.0.0.1 + 10.0.0.6:8899)
      │  SQLite (会话/任务/事件/审批)
      │  文件 IPC (events.jsonl, state.json, control/*.json)
      ▼
agentconsole run-task  (setsid，独立于 Web 服务)
      │
      ├── codex app-server             (JSON-RPC：审批与提问是服务端请求)
      └── claude -p --input-format stream-json --permission-prompt-tool stdio
                                       (stream-json：审批与提问是 can_use_tool 控制请求)
```

关键设计：

1. **Web 只做控制与展示。** 创建任务时，Web 进程把 `task.json` 写入数据目录，并以
   `Setsid` 启动一个独立的 `run-task` 进程；Web 进程重启不会影响它。
2. **事件先落盘，再入库。** runner 把 Agent stdout 逐行写入 `events.jsonl`（带序号），
   Web 侧 Ingestor 从持久化 offset 增量读取并写入 SQLite，唯一键保证重复读取幂等。
3. **SSE 游标恢复。** 前端断线后使用 `Last-Event-ID` / `after` 续传；服务端以 SQLite 为准，
   不会因内存缓冲区丢失事件。
4. **原生恢复。** Codex 使用 app-server 的 `thread/resume`；Claude 使用
   `claude -p --resume <uuid>`。控制台历史只是展示层，真正上下文由 CLI 原生会话延续。
5. **原生历史增量同步。** 导入的会话在打开时（会话详情 + 首页事件）会同步原生转录的
   新增部分，而不是只在启动时拍一次快照。每个会话在 `sessions.meta` 里记录
   `import_path / import_offset / import_size / import_seq` 字节游标：
   - 文件大小未变：只做一次 `stat`，不读文件。
   - 文件变长：从游标处只读完整行；末尾未写完的半行留到下次。
   - 文件变短或换了文件：全量重解析并替换导入事件。
   - 控制台任务窗口（`tasks.started_at` 起）内的原生记录会跳过，避免同一轮对话显示两次；
     仍有控制台事件进行中的会话不会被重排。
   - 用户改过的标题不会被同步覆盖，只有自动生成的标题才会升级。
   这保证 CLI 里继续的对话、甚至刚结束的回答，打开页面即可看到，无需重启服务。
6. **权限桥。** Claude 以
   `--permission-prompt-tool mcp__agentconsole__approve` 启动一个 stdio MCP 服务；
   该服务把审批请求写成文件，Web 写回 `resp-*.json`，因此等待状态可跨浏览器断线与服务重启。
7. **故障分类。**
   - 浏览器/网络断开：无影响，SSE 重连补齐。
   - Web 服务重启：runner 存活（systemd `KillMode=process`），重启后重新挂接。
   - Agent 崩溃：runner 读到非零退出/信号，任务记为 `failed`，保留日志，可手动重跑。
   - 整机重启：进程必然终止。启动时按 `boot_id` + runner PID 判定为 `interrupted`，
     不自动重放任何指令；用户可手动「重新运行」或发送新消息继续原生会话。
8. **孤儿进程清理。** 若 runner 被杀而 Agent 子进程仍在，Ingestor 会先校验进程身份
   （进程组 + cmdline），再终止该进程组，避免任务显示“中断”但仍在产生副作用。

### 目录结构

```text
/srv/rymesh/
├── main.go           命令入口（serve/install/status/doctor/import/…）
├── server.go         HTTP API、调度器、会话/任务/审批路由
├── runner.go         独立任务进程、Agent 命令构造、MCP 审批服务
├── events.go         事件入库、归一化、断线恢复、孤儿清理
├── store.go           SQLite schema 与查询
├── store_extra.go     任务/事件附加查询
├── importer.go       Codex/Claude 原生历史导入
├── auth.go           登录、Cookie、CSRF、锁定、安全头
├── agents.go         Agent 探测（版本/认证/能力）
├── config.go         配置、PBKDF2、监听地址、工作目录校验
├── sse.go            SSE 流
├── web/              前端（无构建步骤）
│   ├── index.html
│   ├── app.js
│   ├── markdown.js   Markdown + 轻量语法高亮
│   ├── styles.css
│   ├── manifest.webmanifest, sw.js, icon*
└── agentconsole_test.go
```

---

## 3. Agent 接入矩阵

| | Codex CLI | Claude Code |
|---|---|---|
| 路径 | `/usr/local/bin/codex` | `/root/.local/bin/claude` |
| 版本 | `codex-cli 0.163.0` | `2.1.303 (Claude Code)` |
| 认证 | 复用 `~/.codex/auth.json`（未读取/未上传） | 复用现有 Claude 登录/环境 |
| 新建 | `codex app-server` → `initialize` → `thread/start {cwd, sandbox, approvalPolicy}` → `turn/start {input, collaborationMode}` | `claude -p --input-format stream-json --output-format stream-json --verbose --permission-mode MODE --permission-prompt-tool stdio --session-id UUID`，首条消息经 stdin 发送 |
| 原生恢复 | `thread/resume {threadId}` | 同上，改用 `--resume UUID` |
| 实时输出 | JSON-RPC 通知（item/started、item/completed、turn/completed） | stream-json |
| 交互 | 服务端请求：`item/commandExecution/requestApproval`、`item/fileChange/requestApproval`（选项来自 `availableDecisions`）、`item/permissions/requestApproval`、`item/tool/requestUserInput` | `control_request` / `can_use_tool`：普通工具（选项由 `permission_suggestions` 生成）、`AskUserQuestion`、`ExitPlanMode` |
| 工具调用 | ✅ | ✅ |
| 权限模式 | 只读（read-only + on-request）/ 自动（workspace-write + on-request）/ 计划模式（read-only + 协作模式 plan）/ 危险：完全访问（danger-full-access + never） | `manual`（即 default）/ `acceptEdits` / `plan` / `auto` / `dontAsk` / `bypassPermissions`（后者追加 `--dangerously-skip-permissions`） |
| 已知警告 | — | 当前环境模型别名会打印 `unrecognized_model` 到 stderr（不影响执行，UI 中折叠显示） |

工作目录必须位于 `allowed_roots`（默认 `/srv/project`、`/home`、`/root`、`/tmp`），
会解析符号链接，防止逃逸。不同会话可并行操作同一目录；创建会话页的“检查目录”会提示
该目录已有运行任务，建议使用独立目录或 git worktree。

---

## 4. 部署（已完成）

当前部署信息：

```text
服务名      agentconsole.service
二进制      /usr/local/bin/agentconsole
配置        /etc/agentconsole/config.json
密码文件    /etc/agentconsole/password.txt (0600，仅存初始默认密码；改密后自动删除)
数据        /var/lib/agentconsole
数据库      /var/lib/agentconsole/agentconsole.db (WAL)
任务日志    /var/lib/agentconsole/tasks/<task-id>/events.jsonl
应用日志    /var/log/agentconsole/agentconsole.log
unit        /etc/systemd/system/agentconsole.service
监听        127.0.0.1:8899, 10.0.0.6:8899
```

从零部署（在一台已装好两个 CLI 的 Debian/Linux 上）：

```bash
cd /srv/rymesh
go build -o agentconsole ./cmd/agentconsole
install -m 0755 agentconsole /usr/local/bin/agentconsole
agentconsole install          # 生成配置/密码、写 unit、enable、启动
agentconsole status
agentconsole doctor
```

`agentconsole install` 的关键行为：

- 若 `/etc/agentconsole/config.json` 不存在，生成随机 32 字节 Cookie 签名密钥，
  并将登录密码设为默认 `admin`（`must_change_password=true`，首次登录必须修改）。
  `--reset-password` 会把密码重置回 `admin` 并要求下次登录修改。
- 自动探测 `tun0` 的 IPv4 并只监听 `127.0.0.1` 与该地址；拒绝在未开启
  `allow_public_bind` 时绑定 `0.0.0.0`。
- 单元文件使用 `KillMode=process`，`Restart=always`，`HOME=/root`，
  `PATH` 包含 `/root/.local/bin`。

配置文件主要字段见第 9 节。

---

## 5. 手机使用教程

1. 在手机上打开 EasyTier，确认已加入与服务端相同的虚拟网络（服务端为 `10.0.0.6`）。
2. 浏览器访问 **http://10.0.0.6:8899**。
3. 输入密码登录。默认密码为 `admin`，登录后必须立即修改（不限制密码长度，非空即可）；
   连续输错 5 次会锁定 60 秒。
4. 打开页面默认是一个空白新对话：左上角下拉选择 Agent，输入消息即可开始。
   当前工作目录只显示在顶栏副标题（如 `Codex CLI · /srv/project`），新对话时
   点击该副标题即可修改，输入框下方不再重复显示目录。输入框下方的「权限」随
   Agent 切换，直接对应 CLI 的原生模式：
   - Codex：`只读沙箱`（read-only）、`工作区可写`（workspace-write）、
     `危险：完全访问`（danger-full-access，关闭沙箱）。
   - Claude Code：`逐次审批`（manual）、`自动接受编辑`（acceptEdits）、
     `计划模式`（plan）、`自动`（auto）、`不询问`（dontAsk）、
     `危险：绕过所有权限`（bypassPermissions）。
   选择按 Agent 分别记忆；标记「危险」的模式会在切换时弹出提示，任务卡片会记录实际模式。
5. 左上角 **☰** 打开对话侧边栏：搜索/选择历史对话、新建对话。每个对话右侧的
   **⋯** 打开操作菜单，可「重命名对话」或「删除对话」。
   底部的「任务中心」查看所有任务、队列位置与耗时，可取消运行中任务，
   或对失败/中断/取消的任务手动「重新运行」。
6. 「删除对话」会提示：控制台记录将被移除，且该 Agent 的原生会话文件会同步移动
   到服务端回收目录。确认后还需再确认一次；不需要手动输入会话 ID。
7. 需要审批时（Claude），右上角会弹出待审批通知，顶栏同时显示待审批数量；
   点击通知会进入对应会话并定位到「需要批准」卡片，可允许/拒绝或填写拒绝原因。
   该请求由服务器持久化，断线或刷新后仍可处理。
8. 关闭浏览器 / 锁屏 / 切网：任务继续。重新打开页面默认回到空白新对话，
   从侧边栏进入历史对话后会补齐断线期间的输出。
9. 对话样式：你的消息在右侧浅蓝色气泡中，Agent 回复通栏显示、不使用气泡；
   用户问题与最终回答之间只显示一个「执行过程 · N 步」折叠项，点击后展开
   全部思考、工具调用、终端命令与工具结果。需要审批的卡片始终可见。
10. 系统设置按钮位于右上角 **⚙**，可查看 Agent 版本与能力、服务绑定、
    数据/日志路径、修改密码、切换主题。终端里开的会话会自动出现在列表中。页面没有底部标签栏。
11. 添加到主屏幕：浏览器菜单中“添加到主屏幕”。注意：在 HTTP 私网地址下，
    大多数浏览器不会注册 Service Worker，因此可能只是普通书签，无法完全离线；
    这不影响后台任务（任务始终在服务器执行）。

### 同一会话的调度策略

- 同一会话同一时间只运行一个任务；运行中再发消息会进入 FIFO 队列，界面显示队列位置。
  排队的判据只有「该会话上已有 running/waiting_input 任务」，**排队中的任务不算占用会话**。
- 不同会话、不同 Agent 可并发，受 `max_concurrent` 限制（默认 4）。判定互不影响：
  A 会话运行中不会阻挡 B 会话启动。
- 只有确实有任务在前时界面才显示「排队中 · 前面还有 N 个」；空队列或受全局并发上限
  阻塞时显示「启动中 / 正在启动…」，不会凭空说排在某个任务后面。
- 取消：先 SIGINT/SIGTERM，超时后 SIGKILL；等待审批的任务取消会同时使审批过期。

---

## 6. 故障与恢复语义

| 场景 | 行为 |
|---|---|
| 浏览器关闭 / 锁屏 / 切网 | Agent 不中断；事件持续写入服务器；重连按游标补齐 |
| Web 服务重启 | runner 与 Agent 存活；重启后 Ingestor 重新挂接；任务继续 |
| Agent 进程崩溃 | 任务 `failed`，保留已有日志，记录信号/退出码；需人工重跑 |
| runner 被杀 / 服务异常 | 约 1–2 秒内标记 `interrupted`，终止孤儿 Agent 进程组 |
| 整机重启 | 所有进程终止；启动后按 boot_id 标记 `interrupted`；**不自动重放** |
| 审批等待中断线 | 请求与等待状态在 SQLite/文件中，重连后继续；超过 TTL 自动过期 |

任务状态：`pending` / `running` / `waiting_input` / `completed` / `failed` /
`cancelled` / `interrupted`。

**不会自动重试**任何 `running`/`interrupted` 任务。重跑与“继续原生会话”是两件事：

- 「重新运行」= 用同一提示词创建新任务（可能有副作用，界面会二次确认）。
- 在历史会话里发送新消息 = 继续该原生会话，不重放旧指令。

---


## 7. 运维命令

```bash
# 状态 / 自检
systemctl status agentconsole
systemctl is-enabled agentconsole
agentconsole status
agentconsole doctor

# 启动 / 停止 / 重启 Web 服务
systemctl start agentconsole
systemctl stop agentconsole
systemctl restart agentconsole

# 注意：KillMode=process，stop/restart 不会杀死正在运行的 Agent runner。
# 如需强制终止所有 runner（会影响正在执行的任务）：
systemctl kill -s SIGKILL --kill-whom=all agentconsole

# 日志
journalctl -u agentconsole -f
tail -f /var/log/agentconsole/agentconsole.log

# 密码
agentconsole password                       # 查看当前密码
agentconsole password --set NEW             # 直接设置，并要求下次登录修改
agentconsole password --set NEW --force-change
agentconsole password --generate            # 生成随机密码
agentconsole password --set admin --force-change   # 恢复默认密码并要求首次登录修改

# 手动导入原生历史（服务运行时也可；会跳过 import_exclude_roots）
# 一般不需要：import_on_start=true 时服务每 30 秒自动同步一次，只解析有变化的文件
agentconsole import

# 列出/取消任务
agentconsole tasks
agentconsole cancel <task-id>
```

### 更新

```bash
cd /srv/rymesh
go test ./...
go build -o agentconsole ./cmd/agentconsole
install -m 0755 agentconsole /usr/local/bin/agentconsole
systemctl restart agentconsole        # runner 不受影响
```

### 备份

```bash
systemctl stop agentconsole
tar czf /root/agentconsole-backup-$(date +%F-%H%M).tgz \
  /etc/agentconsole /var/lib/agentconsole /var/log/agentconsole
systemctl start agentconsole
chmod 600 /root/agentconsole-backup-*.tgz
```

备份包含密码哈希与 Cookie 签名密钥，请按敏感文件保管。恢复时解包到对应路径，
确保 `/etc/agentconsole` 为 `0700`、`config.json`/`password.txt` 为 `0600`，然后
`systemctl start agentconsole`。

### 故障排查

| 现象 | 检查 |
|---|---|
| 手机打不开 | `ip -4 addr show tun0`；`ss -ltnp \| grep 8899` 应只有 127.0.0.1 与 10.0.0.6；EasyTier peer 是否在线 |
| 502/连接被拒 | `systemctl status agentconsole`；`curl http://127.0.0.1:8899/api/health` |
| 登录 429 | 连续失败 5 次锁定 60 秒；重启服务可立即清除内存中的锁定 |
| Agent 不可用 | 以 root 执行 `codex --version`、`claude --version`；unit 的 `HOME=/root` 与 PATH 是否正确 |
| 审批不出现 | 任务是否 `waiting_input`；查看任务事件中的 `permission` 记录；Claude 版本是否支持 `--permission-prompt-tool` |
| 任务显示 interrupted | 查看错误信息：runner 丢失或服务器重启；用「重新运行」或发送新消息继续会话 |
| 端口占用 | `ss -ltnp \| grep 8899`；修改 `/etc/agentconsole/config.json` 的 `port` 后重启 |
| 数据库异常 | 备份后检查 `/var/lib/agentconsole/agentconsole.db`；服务使用单连接 WAL，不要同时启动多个实例 |
| 会话打开慢 | 文本响应默认 gzip；首屏只取最近 200 条，工具输出折叠且按需渲染；可「加载更早」（每页 500 条） |

---

## 8. 安全模型

- **监听**：只绑定 loopback 与 EasyTier 网卡；`allow_public_bind=false`，
  即使配置了 `0.0.0.0` 也会拒绝启动。当前 `ss` 确认没有公网/以太网监听。
- **认证**：PBKDF2-HMAC-SHA256（120k 次）；HMAC-SHA256 签名的 HttpOnly、
  SameSite=Strict Cookie（默认 30 天）；每个变更请求要求 `X-CSRF-Token`。
- **登录限制**：每 IP 5 次失败锁定 60 秒；不信任 `X-Forwarded-For`，防止绕过。
- **首次登录**：默认密码 `admin` 仅用于局域网初始访问；`must_change_password=true` 时，
  除改密与登出外所有 API 都返回 403，前端只显示不可跳过的改密表单。密码非空即可，
  不设长度下限。
- **授权**：所有 `/api/*`（除 health/session/login）要求认证；工作目录受 allowed roots 限制；
  Agent 名称白名单；请求体上限。
- **不泄密**：API 不返回密码哈希、Cookie 密钥、Agent Token/API Key；设置页仅显示版本、
  路径、认证方式摘要。
- **审批**：默认模式（Claude `manual`、Codex `read-only`）下危险操作必须网页批准，不自动批准。
  `危险：完全访问` / `危险：绕过所有权限` 是用户逐次显式选择的模式，会跳过限制；
  切换时前端提示，任务记录保留当时的实际模式，便于审计。
- **高危操作**：删除对话需要两次界面确认（不要求输入会话 ID）；原生会话文件仅移动到
  `/var/lib/agentconsole/trash/`，不会直接删除。运行中或排队中的任务会阻止删除。
- **HTTP**：控制台本身是 HTTP，依赖 EasyTier 隧道加密传输；如需额外 TLS，
  可在前面加反向代理并只监听 loopback。

---

## 9. 配置

`/etc/agentconsole/config.json`（0600）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `port` | 8899 | 监听端口 |
| `listen_addrs` | 空 | 显式地址列表；空 = loopback + EasyTier 网卡 |
| `easytier_interface` | `tun0` | EasyTier 网卡名 |
| `allow_public_bind` | false | 允许绑定 0.0.0.0（不建议） |
| `default_cwd` | `/srv/project` | 新会话默认目录 |
| `allowed_roots` | `/srv/project`,`/home`,`/root`,`/tmp` | 允许的工作目录根 |
| `max_concurrent` | 4 | 并发任务上限（1–32） |
| `data_dir` | `/var/lib/agentconsole` | SQLite、任务日志、回收站 |
| `log_dir` | `/var/log/agentconsole` | 应用日志 |
| `password_hash` | 默认 `admin` 的哈希 | PBKDF2-HMAC-SHA256（120k 次） |
| `must_change_password` | true（新装） | 为 true 时除改密/登出外所有 API 返回 403 |
| `session_ttl_hours` | 720 | 登录 Cookie 有效期 |
| `permission_ttl_minutes` | 60 | 审批请求有效期 |
| `task_log_max_bytes` | 64 MiB | 单任务事件文件上限（退出/状态记录始终保留） |
| `import_on_start` | true | 启动时只读导入原生历史，并每 30 秒自动同步新增/变化的会话 |
| `import_history_limit` | 0 | 控制台保存的历史事件上限；0 = 全部保存（原生恢复不受影响） |
| `import_exclude_roots` | `[]` | 导入时跳过的目录；支持 `*` 通配符。当前部署为 `["/tmp/ac-*", "/tmp/cc-probe-main", "/tmp/agenttest-*"]`，防止测试会话进入生产库 |
修改配置后 `systemctl restart agentconsole`。
`max_concurrent` 与 `default_cwd` 也可在设置页修改。

### 设置页：Agent 的更新与重启

这里的“更新”和“重启”针对 **Codex CLI / Claude Code 本身**，不是 Agent Console：

- **检查更新**：对 `@openai/codex` 与 `@anthropic-ai/claude-code` 执行
  `npm view <包> version`，与 `codex --version` / `claude --version` 对比。
- **更新**：按安装方式执行
  - npm 安装（二进制位于 `node_modules` 下）：`npm install -g @openai/codex@latest`
    或 `npm install -g @anthropic-ai/claude-code@latest`；
  - 原生安装的 Claude Code：`claude update`。
  更新后自动重新检测版本、认证与模型。
- **重启 Agent**：Console 不常驻 Agent 进程，每条消息都会新建一个 CLI 进程并读取最新的
  `~/.codex/config.toml` / `~/.claude/settings.json`。因此“重启”会：
  1. 重新检测该 Agent 的版本、认证与模型；
  2. 结束它 **正在运行/等待审批** 的任务，并用相同内容重新排队（新进程读取新配置）；
  3. 排队中的任务不受影响——它们启动时本来就会使用新配置。
  没有运行中的任务时，页面会提示无需重启，直接发送新消息即可。

---

## 10. 多设备（主控 + 节点）

每台机器都安装一份 agentconsole，作为**节点**运行自己的 Agent、数据库与原生历史同步。
其中一台在配置里列出其它节点，即成为**主控**：手机只访问主控，页面通过
`/n/<设备>/api/...` 由主控经 EasyTier 转发到对应节点（含 SSE 实时流）。Agent 始终在
所属机器上运行，主控、网络或浏览器断开都不影响它。

```bash
# 在新设备（节点）上：取得令牌（首次自动生成），可选设置显示名
agentconsole node token --config <节点配置>
agentconsole node name Mac --config <节点配置>

# 在主控上：添加设备并重启
agentconsole node add mac --name Mac --url http://10.0.0.2:8899 --token <令牌>
systemctl restart agentconsole
agentconsole node list        # 在线状态与令牌校验
```

- 节点只接受主控的 `Authorization: Bearer <node_token>`，或节点自己的登录 Cookie；
  节点应只监听 127.0.0.1 与 EasyTier IP。
- 登录、改密、退出始终作用于主控，不会转发给节点。节点令牌错误显示为 502，不会把用户登出。
- 侧边栏顶部切换设备；设置页的 Agent / 通用 / 服务分组属于当前设备。
- 其它设备上的待审批请求会汇总到通知里，点击直接切到该设备的会话。
- macOS 节点：`go build` 后用 `agentconsole serve --config ~/.agentconsole/config.json` 运行
  （无 systemd，开机标识取 `kern.boottime`）。

## 11. 客户端即主控：Mac 与 Android（SSH 直连，免组网）

主控运行在用户手里的设备上，通过 SSH 直连各台服务器；服务器上的节点只监听 `127.0.0.1`，
除 SSH 外不暴露任何端口，也不需要 EasyTier 之类的组网。

```text
Mac：agentconsole client（127.0.0.1）   Android：App 内嵌 Go 核心 + WebView（127.0.0.1 随机端口）
   ├─ 本机节点（Mac 上可直接跑 Agent）       └─ 只做主控（hub_only）
   └─ /n/<设备>/api/… ──SSH direct-tcpip──▶ 服务器 127.0.0.1:8899（agentconsole node）
```

**Mac App**

```bash
make macos       # → macos/dist/AgentConsole.app（Swift 外壳 + 通用二进制 Go 控制台，ad-hoc 签名）
```

把 `AgentConsole.app` 拖进“应用程序”。从网上下载的副本首次打开会被 Gatekeeper 拦下：
系统设置 → 隐私与安全性 → “仍要打开”（或 `xattr -dr com.apple.quarantine /Applications/AgentConsole.app`）。
App 提供独立窗口、菜单栏图标（待处理数量）、Dock 角标、审批/提问的系统通知（点击直达卡片）。
控制台只在 App 运行时运行（普通进程，不开机自启、不常驻）；退出 App 会停止控制台，
但正在运行的 Agent 任务是独立进程，继续执行，下次打开 App 时补上它们的输出。
App 内置 Linux amd64/arm64 节点程序，可直接部署服务器；若服务器上已有系统级安装
（`agentconsole install`：`/etc/agentconsole` + systemd），部署会接管并原地升级它（旧程序保存为 `.prev`），
而不是另起一个节点。

不想用 App 也可以只用命令行：`agentconsole client`（首次自动配置，免密打开浏览器）、
`agentconsole client open`、`agentconsole client stop`。

设置 → 设备 → 添加服务器：可从 `~/.ssh/config` 一键填入，密钥可选密钥库里的、`~/.ssh` 里的、ssh-agent，
或当场生成。首次连接核对并固定主机指纹（若 `known_hosts` 已有记录则自动采用），指纹变化一律拒绝。
服务器还不认识这把密钥时，可以输入一次服务器密码自动把公钥写入 `~/.ssh/authorized_keys`
（同 `ssh-copy-id`；只在主机指纹已固定后发送，密码不保存），或复制命令手动添加。随后一键部署：客户端上传对应平台
的程序（`make dist` 产物），在服务器上以 SSH 用户身份执行 `agentconsole node setup`
（安装到 `~/.agentconsole`，root 用 systemd、普通用户用 `systemctl --user`、macOS 用 launchd），
已有节点则沿用其令牌原地升级，因此 Mac 与手机可以共用同一台服务器。

**密钥库与迁移。** 客户端的设备列表（地址、固定的主机指纹、节点令牌）和 SSH 私钥保存在自己的
SQLite 数据库里（`data/agentconsole.db`，权限 0600），不依赖 `~/.ssh`。设置 → 密钥 可生成、重命名、
删除密钥，也可以把 `~/.ssh` 里的密钥导入（有密码短语的输入一次，之后不再需要）。
设置 → 导出设备 把选中的服务器连同密钥打包成 `.acbundle` 文件，用随机生成的 16 位解密码
（scrypt + AES-256-GCM）加密；在另一台客户端（例如手机）上 设置 → 导入设备，选文件、输入解密码即可直接使用。
解密码只显示一次，请和文件分开传递。旧版本 `config.json` 里的设备和 `data/keys/` 里的密钥会在首次启动时
自动迁入数据库（原配置备份为 `config.json.pre-vault`）。密钥库只保存 SSH 与节点凭据，不读取、不保存任何
Codex / Claude 账号凭据。

**Android**

```bash
export ANDROID_HOME=~/Library/Android/sdk ANDROID_NDK_HOME=$ANDROID_HOME/ndk/<版本> \
       JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
go install golang.org/x/mobile/cmd/gomobile@latest golang.org/x/mobile/cmd/gobind@latest
make android     # → android/app/build/outputs/apk/debug/app-debug.apk
```

可以在 App 内生成密钥并添加服务器，也可以从电脑导出设备后在手机上导入（见上文“密钥库与迁移”）。
APK 内置 Linux amd64/arm64 节点程序，可直接部署。界面跟随系统深浅色，返回手势先关闭弹层。
App 退到后台时连接会断开，服务器上的 Agent 照常运行；回到前台自动重连。

## 12. 已知限制

1. **交互请求的存续。** 待处理的审批 / 提问由 runner 保持（控制目录里的 `req-*.json`），
   网页服务重启、浏览器关闭都不影响；超过 `permission_ttl_minutes` 未处理时，权限请求按拒绝、
   提问按"未回答"答复 Agent。Codex 的 MCP 表单请求（elicitation）暂不支持，会自动拒绝。
2. **PWA 安装/离线**在 `http://<私网 IP>` 下通常不可用（Service Worker 需要安全上下文）。
   manifest 与图标已提供；后台执行不依赖 PWA。
3. **原生历史导入**默认完整保存所有事件（`import_history_limit=0`），不修改任何原生文件。
   会话打开时会增量同步原生转录的新增内容（见第 5 节第 5 条）；同步只读取文件，不回写原生文件。
   前端首屏只显示最近 200 条，可「加载更早的消息」（每页 500 条）。子代理转录（`subagents/`）不是独立
   对话，会被跳过；只有本地命令、`/resume` 或空 rollout 的转录不会显示为会话。
   标题优先使用 Claude Code 生成的 `ai-title`。
   可用 `import_exclude_roots` 排除测试目录（当前为 `["/tmp/ac-*", "/tmp/cc-probe-main", "/tmp/agenttest-*"]`），被排除的会话不会进入控制台。
4. **真机与物理网络切换**未在当前环境验证（见 TEST_REPORT）。服务器端 EasyTier 地址已验证，
   其他 EasyTier 设备/手机的实际访问需要现场确认。
5. **整机重启未实际执行**（会中断当前管理会话）。开机自启已配置，boot_id 判定逻辑有单元测试，
   runner 丢失路径已实测；不声称重启后进程继续运行。
6. **一个会话一个活动任务**；同一会话并发消息会排队，这是刻意设计，避免上下文错乱。
7. 仅提供 Codex/Claude 适配器；新增 Agent 可在 `agents.go` 探测与 `runner.go`
   的 `buildAgentCommand` 中扩展，事件归一化在 `events.go`。
