# Rymesh

在手机或 Mac 上遥控服务器里的 **Codex CLI**、**Claude Code** 和 **Hermes Agent**：
看它们在做什么、批准它们要做的事、接着和它们聊。

Agent 在服务器上独立运行：关掉 App、锁屏、断网，任务都会继续，回来时补上错过的输出。

<p>
  <a href="https://github.com/ecouus/Rymesh/releases/latest"><b>⬇ 下载最新版本</b></a>
  &nbsp;·&nbsp; macOS（Apple 芯片 / Intel） &nbsp;·&nbsp; Android
</p>

> **Rymesh 只是遥控器。** 所有模型请求都由服务器上的官方 CLI，用那台服务器自己的登录发出。
> Rymesh 不调用任何模型 API、不转发模型流量、不读取或上传 CLI 的登录凭据。
> 请只用它控制你自己的机器，不要拿来让别人使用你的账号（那属于账号共享，违反各家的服务条款）。

---

## 功能

- **多台服务器、一个界面**：Mac 和手机通过 SSH 直连各台服务器，服务器上只需要开放 SSH，
  不用组网、不用公网端口。侧边栏顶部切换设备。Mac 本身也可以作为一台“服务器”供手机使用。
- **三种 Agent**：Codex CLI、Claude Code、Hermes Agent，按服务器上实际安装的显示。
- **后台托管**：每个任务是服务器上的独立进程，和 App、网页、网络连接都无关；整机重启后
  任务会标记为“已中断”，不会被自动重放。
- **和 CLI 一致的审批**：命令、文件修改、权限申请会弹出卡片，可以允许一次、本次对话都允许、
  拒绝并说明理由；Agent 提问（单选 / 多选 / 自填）和计划审批也在页面上完成。
  等待中的审批跨断线、跨重启保留，其它设备上的待审批会汇总到通知里。
- **权限模式与模型**：直接对应各 CLI 自己的模式（只读、自动接受编辑、计划模式……）
  和模型列表，按 Agent 分别记住。
- **原生历史**：自动导入你在终端或桌面版里和这些 Agent 聊过的对话，之后的新内容也会同步过来；
  导入只读，不改原始记录。打开任何一段都可以直接接着聊（个别来源只能查看，见下文）。
- **斜杠命令**：输入 `/` 列出该 Agent 支持的命令，原样交给 CLI 执行。
- **终端与文件**：每台服务器都可以打开终端、浏览和上传下载文件。
- **Mac App**：独立窗口、菜单栏图标和 Dock 角标显示待处理数量，审批和提问有系统通知，点击直达。
- **Android App**：内置完整控制台，不依赖电脑；界面跟随系统深浅色。

## 下载与安装

到 [Releases](https://github.com/ecouus/Rymesh/releases/latest) 下载对应平台的文件：

| 平台 | 文件 | 要求 |
|---|---|---|
| macOS | `Rymesh-macOS.zip` | macOS 13 及以上，Apple 芯片和 Intel 通用 |
| Android | `Rymesh-Android.apk` | Android 8.0 及以上，64 位 ARM |
| Linux 节点程序 | `rymesh_linux_amd64`、`rymesh_linux_arm64` | 一般不用单独下载：App 部署服务器时会自动上传 |

**macOS**：解压后把 `Rymesh.app` 拖进“应用程序”。App 没有经过 Apple 公证，首次打开会被拦下：
到 系统设置 → 隐私与安全性，点“仍要打开”；或在终端执行
`xattr -dr com.apple.quarantine /Applications/Rymesh.app`。

**Android**：下载 APK 后直接安装，按系统提示允许“安装未知来源应用”。

## 快速开始

1. **在服务器上装好 Agent 并登录**，和平时在终端里用一样：
   `codex`（登录 ChatGPT 账号或配置好 API）、`claude`（登录 Claude 账号）、
   `hermes setup`（Hermes Agent）。装哪个都行，Rymesh 只用已经能正常工作的。
2. **打开 App → 侧边栏底部“设置” → 设备 → 添加服务器**。
   可以从 `~/.ssh/config` 一键填入；密钥可以用已有的、ssh-agent 里的，或在 App 里当场生成。
   首次连接会让你核对并固定主机指纹，之后指纹变化一律拒绝连接。
   服务器还不认这把密钥时，可以输入一次服务器密码自动把公钥加进去（密码不保存）。
3. **点“部署”**。App 把节点程序上传到服务器并安装到 `~/.rymesh`
   （root 用 systemd，普通用户用 `systemctl --user`，macOS 用 launchd）。
   已经部署过的服务器会原地升级，并保留原来的配对，所以 Mac 和手机可以共用同一台服务器。
4. **开始用**：选 Agent、选工作目录，输入消息。

想在电脑和手机之间同步设备列表：设置 → 导出设备，得到一个用随机解密码加密的 `.rymesh` 文件；
在另一台设备上 设置 → 导入设备。解密码只显示一次，请和文件分开传递。

### 把 Mac 作为手机的服务器

Mac 没有公网 IP 也可以：只要手机能连到 Mac 的 SSH（同一 Wi-Fi、Tailscale、EasyTier，
或把 22 端口转发出去）。

1. Mac 上：系统设置 → 通用 → 共享 → 打开“远程登录”，并保持 Rymesh App 运行。
2. 手机上：添加服务器，填 Mac 的地址和用户名，核对指纹后部署。部署发现对方是运行中的 Rymesh 时
   不会安装任何东西，只向它取一个配对令牌。Mac 睡眠时手机连不上它。

## 支持的 Agent

| | Codex CLI | Claude Code | Hermes Agent |
|---|---|---|---|
| 接入方式 | `codex app-server`（JSON-RPC） | `claude -p --input-format stream-json` | `hermes acp`（Agent Client Protocol） |
| 审批与提问 | ✅ | ✅ | ✅ 文件编辑等审批 |
| 原生历史导入 | ✅ 会话文件，以及新版 Codex 存在数据库里的对话（经 app-server 读取） | ✅ 会话文件 | ✅ 经 `hermes sessions export --redact` 读取，含各档案 |
| 接着聊历史对话 | ✅ | ✅ | 仅限通过 ACP 创建的对话 |

Hermes 只能接续通过 ACP 创建的对话，这是 Hermes 本身的限制。来自微信、Telegram、桌面版群聊、
终端的 Hermes 对话会以只读方式显示，并注明来源。

每个对话固定在某台服务器、某个 Agent 上。同一对话同一时间只跑一个任务，
运行中再发的消息会排队；不同对话可以并发（默认最多 4 个）。

## 工作原理

```text
Mac App / Android App（内置主控，只监听本机）
      │  SSH（固定主机指纹）
      ▼
服务器：rymesh 节点（只监听 127.0.0.1）
      │  SQLite：对话、任务、事件、审批
      ▼
rymesh run-task（每个任务一个独立进程，和网页服务分离）
      ├── codex app-server
      ├── claude -p --input-format stream-json --permission-prompt-tool stdio
      └── hermes acp
```

- **网页服务只负责控制和展示。** 每个任务由单独启动的进程执行，网页服务重启、升级都不影响它。
- **事件先落盘再入库。** 任务进程把 Agent 的输出逐行写进日志，再增量写入数据库；
  页面断线后按序号续传，不会丢。
- **接续用 CLI 自己的会话。** 控制台里的历史只是展示，真正的上下文由 CLI 的原生会话延续。
- **导入的历史是一份副本**，存在控制台自己的数据库里，原始记录始终由各个 CLI 自己保管。
  在控制台删除对话时，副本会删掉；Codex、Claude 的原始文件会挪进回收目录
  （`<数据目录>/trash/`），30 天后自动清理；存在 Agent 自己数据库里的记录不会被动。

## 安全

- 服务器上的节点只监听 `127.0.0.1`，除 SSH 外不开放任何端口。App 到服务器的流量走 SSH，
  主机指纹固定。
- 没有密码登录：App 用一次性票据登录本机控制台，主控访问节点用每个节点独立的令牌。
  在浏览器里直接打开节点地址只会看到“请在应用中打开”。
- SSH 私钥和设备列表加密保存在客户端自己的数据库里（macOS 用钥匙串、Android 用 Keystore 保管密钥）。
- 不读取、不保存任何 Codex / Claude / Hermes 的账号凭据；导入 Hermes 历史时会让 Hermes 先把聊天里的密钥打码。
- 默认权限模式下，危险操作必须在页面上批准。“危险”类模式需要逐次显式选择，任务记录会保留当时的模式。

## 从源码构建

需要 Go 1.26+。

```bash
make build      # 命令行程序 ./rymesh
make test       # go vet + go test -race
make dist       # 各平台程序（含 App 内置的 Linux 节点程序）→ dist/
make macos      # Mac App → macos/dist.noindex/Rymesh.app（需要 Xcode 命令行工具）
make android    # Android APK → android/app/build/outputs/apk/release/app-release.apk
```

Android 需要 Android SDK / NDK 和 gomobile：

```bash
export ANDROID_HOME=~/Library/Android/sdk ANDROID_NDK_HOME=$ANDROID_HOME/ndk/<版本> \
       JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
go install golang.org/x/mobile/cmd/gomobile@latest golang.org/x/mobile/cmd/gobind@latest
```

不想用 App 时，电脑上也可以只用命令行：`rymesh client` 启动本机控制台并在浏览器中打开。

### 目录结构

```text
cmd/rymesh/      程序入口
*.go             HTTP 接口、调度、多设备中转与部署、终端与文件
internal/
  agent/         调用官方 CLI：探测、命令构造、stdio 协议、审批、任务进程
  importer/      导入并同步 CLI 的原生历史
  ingest/        任务事件入库与归一化
  store/         SQLite：对话、任务、事件、审批、加密的密钥库
  config/ sshconn/ sys/ util/
web/             前端（无构建步骤）
macos/           Mac App（Swift）
android/         Android App
mobile/          Android 用的 gomobile 绑定
```

## 命令行与配置

部署后的节点一般不需要手动维护。需要时可以在服务器上使用：

```bash
rymesh status           # 运行状态
rymesh doctor           # 自检：Agent、目录、监听
rymesh tasks            # 列出任务
rymesh cancel <任务ID>  # 取消任务
rymesh logs <任务ID>    # 任务日志
rymesh import           # 立即导入原生历史（默认每 30 秒自动同步）
```

配置文件在 `~/.rymesh/config.json`（以 root 用 `rymesh install` 安装时为 `/etc/rymesh/config.json`），常用字段：

| 字段 | 默认 | 说明 |
|---|---|---|
| `default_cwd` | 家目录 | 新对话的默认工作目录 |
| `allowed_roots` | 家目录 | 允许作为工作目录的根目录（目录之外的对话不能运行） |
| `max_concurrent` | 4 | 同时运行的任务上限（1–32） |
| `permission_ttl_minutes` | 60 | 审批请求的有效期，过期按拒绝处理 |
| `import_on_start` | true | 启动时导入原生历史，并每 30 秒同步 |
| `import_history_limit` | 0 | 每段对话在控制台里保留的事件上限；0 为全部 |
| `import_exclude_roots` | `[]` | 不导入的目录，支持 `*` 通配符 |

`default_cwd` 和 `max_concurrent` 也可以在设置页修改。

## 已知限制

- Hermes 只能接续通过 ACP 创建的对话，其它来源的 Hermes 对话只读。
- Codex 的 MCP 表单请求（elicitation）暂不支持，会自动拒绝。
- Codex 恢复一段很久没动的对话时，会把它的“更新时间”改成当前时间，这是 Codex 自己的行为。
- 整机重启会终止正在运行的任务；重启后它们显示为“已中断”，可以手动重跑或接着发消息。

## 参与开发

开发约束（尤其是“只做遥控器”的原则）见 [CLAUDE.md](CLAUDE.md)。
`principle_test.go` 会在源码里出现模型服务商 API 地址或凭据变量时让构建失败。

## 许可证

Copyright © 2026 ecouus

Rymesh 以 [GNU Affero 通用公共许可证 第 3 版（AGPL-3.0）](LICENSE) 发布：
可以自由使用、修改和分发，但分发修改后的版本，或把修改后的版本作为网络服务提供给他人使用时，
必须以同样的许可证公开完整源码。
