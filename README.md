# Rymesh

**English** · [简体中文](README.zh-CN.md)

A remote control for **Codex CLI**, **Claude Code** and **Hermes Agent** on your servers, from your phone or Mac:
see what they are doing, approve what they want to do, and keep talking to them.

Agents run on the server on their own: close the app, lock the phone or lose the network and the task
carries on; when you come back you see what you missed.

<p>
  <a href="https://github.com/ecouus/Rymesh/releases/latest"><b>⬇ Download the latest release</b></a>
  &nbsp;·&nbsp; macOS (Apple silicon / Intel) &nbsp;·&nbsp; Android
</p>

> **Rymesh is only a remote control.** Every model request is sent by the official CLI on the server,
> with that server's own login. Rymesh calls no model API, relays no model traffic, and never reads or
> uploads the CLIs' credentials. Use it to control your own machines only, not to let other people use
> your account (that is account sharing, which the providers' terms forbid).

> The app's interface is currently in **Chinese**. Menu names below are given in English with the
> Chinese label in brackets.

---

## Features

- **Many servers, one app**: the Mac and the phone reach each server directly over SSH. The server only
  needs SSH open: no VPN, no public port. Switch servers at the top of the sidebar. A Mac can itself
  serve as a "server" for the phone.
- **Three agents**: Codex CLI, Claude Code and Hermes Agent, as installed on each server.
- **Runs in the background**: every task is its own process on the server, independent of the app, the
  page and the connection. After a reboot, tasks are marked interrupted and never replayed on their own.
- **Approvals as in the CLI**: commands, file edits and permission requests show up as cards: allow once,
  allow for this conversation, or deny with a reason. Agent questions (single choice, multiple choice,
  free text) and plan approval are answered on the page too. Pending approvals survive disconnects and
  restarts, and those waiting on other servers are collected in the notifications.
- **Permission modes and models**: the CLIs' own modes (read-only, accept edits, plan mode…) and model
  lists, remembered per agent.
- **Native history**: conversations you had with these agents in a terminal or their desktop apps are
  imported, and later turns keep syncing in. Importing only reads; the originals are never changed.
  Any conversation can be continued (a few sources are read-only, see below).
- **Slash commands**: type `/` for the agent's commands; they are passed to the CLI as they are.
- **Terminal and files**: open a terminal on any server, browse files, upload and download.
- **Mac app**: its own window, a menu bar icon and a Dock badge with the number of pending items, and
  system notifications for approvals and questions that open the card.
- **Android app**: the whole console runs inside the app, no computer needed; follows the system's
  light or dark mode.

## Download and install

Get the files for your platform from [Releases](https://github.com/ecouus/Rymesh/releases/latest):

| Platform | File | Requirements |
|---|---|---|
| macOS | `Rymesh-macOS.zip` | macOS 13 or later, Apple silicon and Intel |
| Android | `Rymesh-Android.apk` | Android 8.0 or later, 64-bit ARM |
| Linux node | `rymesh_linux_amd64`, `rymesh_linux_arm64` | Usually not needed: the app uploads it when it sets up a server |

**macOS**: unzip and drag `Rymesh.app` into Applications. The app is not notarized by Apple, so the first
launch is blocked: open System Settings → Privacy & Security and click "Open Anyway", or run
`xattr -dr com.apple.quarantine /Applications/Rymesh.app` in a terminal.

**Android**: install the APK and allow "install unknown apps" when asked.

`SHA256SUMS` in each release lists the files' checksums.

## Getting started

1. **Install the agents on the server and log in**, as you would for terminal use:
   `codex` (sign in with ChatGPT or set up an API key), `claude` (sign in to Claude),
   `hermes setup` (Hermes Agent). Any of them will do; Rymesh uses whichever already work.
2. **In the app: sidebar → Settings (设置) at the bottom → Add server (添加服务器)**.
   Fill it in from `~/.ssh/config` with one tap if you like, and pick how to log in (登录方式):
   - **Key (密钥)**: an existing key, one in ssh-agent, or one generated in the app. If the server does
     not know the key yet, enter the server password once and the app installs the public key
     (the password is used for that one connection and not kept).
   - **Password (密码)**: for servers that only accept passwords. The password is kept encrypted in the
     app and used for every connection.

   On first contact you confirm the server's host key fingerprint; from then on a changed fingerprint is
   refused.
3. **Tap Deploy node (部署节点)**. The app uploads the node program and installs it in `~/.rymesh` (systemd for
   root, `systemctl --user` for other users, launchd on macOS). A server that already has it is upgraded
   in place and keeps its pairing, so a Mac and a phone can share a server.
4. **Start**: pick an agent and a working directory, and send a message.

**Moving your servers from the computer to the phone**: on the phone, Settings → Import devices
(导入设备) → Scan QR code (扫描二维码); the phone shows a 16-character pairing code. On the computer,
Settings → Export devices (导出设备) → Show QR code (显示二维码), type in that pairing code, then scan the
QR code that appears with the phone. The QR code is sealed with the phone's pairing code and the
computer's screen never shows any secret, so a photo or recording of that screen opens nothing; the code
hides itself after two minutes. You can also export a `.rymesh` file and import it on the other device
with its decryption code; send the code separately from the file.

### Using a Mac as the phone's server

The Mac needs no public IP, only an SSH connection the phone can reach (same Wi-Fi, Tailscale, EasyTier,
or port 22 forwarded).

1. On the Mac: System Settings → General → Sharing → turn on Remote Login, and keep the Rymesh app
   running.
2. On the phone: add the Mac as a server with its address and user name, confirm the fingerprint and
   deploy. When deploy finds Rymesh running there it installs nothing and only asks it for a pairing
   token. The phone cannot reach the Mac while it sleeps.

## Supported agents

| | Codex CLI | Claude Code | Hermes Agent |
|---|---|---|---|
| Driven through | `codex app-server` (JSON-RPC) | `claude -p --input-format stream-json` | `hermes acp` (Agent Client Protocol) |
| Approvals and questions | ✅ | ✅ | ✅ file edits and similar |
| Native history import | ✅ session files, and threads newer Codex versions keep in their database (read through the app-server) | ✅ session files | ✅ through `hermes sessions export --redact`, all profiles |
| Continue past conversations | ✅ | ✅ | Only those created over ACP |

Hermes can only continue conversations created over ACP; that is a limit of Hermes itself. Hermes
conversations from WeChat, Telegram, desktop group chats or the terminal are shown read-only, with their
source.

Each conversation belongs to one server and one agent. A conversation runs one task at a time; messages
sent meanwhile are queued. Different conversations run in parallel (up to 4 by default).

## How it works

```text
Mac app / Android app (built-in hub, listens on this device only)
      │  SSH (pinned host key)
      ▼
Server: rymesh node (listens on 127.0.0.1 only)
      │  SQLite: conversations, tasks, events, approvals
      ▼
rymesh run-task (one process per task, separate from the web service)
      ├── codex app-server
      ├── claude -p --input-format stream-json --permission-prompt-tool stdio
      └── hermes acp
```

- **The web service only controls and shows.** Each task runs in a process of its own; restarting or
  upgrading the web service does not touch it.
- **Events reach disk before the database.** The task process writes the agent's output line by line
  to a log, which is then read into the database; a page that reconnects resumes from the last event it
  saw.
- **Continuing uses the CLI's own session.** The console's history is for display; the real context is
  carried by the CLI's native session.
- **Imported history is a copy** in the console's own database; the originals stay with each CLI.
  Deleting a conversation in the console deletes the copy; Codex and Claude session files are moved to
  a trash folder (`<data dir>/trash/`) and removed after 30 days; records in an agent's own database are
  left alone.

## Security

- The node on a server listens on `127.0.0.1` only and opens no port besides SSH. Traffic from the app
  to a server goes over SSH with a pinned host key.
- The console has no password login: the apps sign in to their local console with a one-time ticket,
  and the hub reaches each node with that node's own token. Opening a node's address in a browser only
  says to open it from the app.
- SSH private keys, server passwords and the device list are kept in the client's own database; keys
  and passwords are encrypted (the encryption key is held by the macOS keychain or the Android
  Keystore), and no page or API returns them.
- No Codex / Claude / Hermes account credentials are ever read or stored; when importing Hermes history,
  Hermes is asked to redact secrets in the chats first.
- In the default permission modes, risky actions must be approved on the page. "Dangerous" modes must be
  chosen explicitly each time, and every task records the mode it ran in.

## Building from source

Requires Go 1.26 or later.

```bash
make build      # the command-line program, ./rymesh
make test       # go vet + go test -race
make dist       # programs for every platform (including the Linux nodes the apps carry) → dist/
make macos      # the Mac app → macos/dist.noindex/Rymesh.app (needs the Xcode command line tools)
make android    # the Android APK → dist/Rymesh-Android.apk
make release    # every release file and its checksum → dist/release/
```

Android also needs the Android SDK and NDK, and gomobile:

```bash
export ANDROID_HOME=~/Library/Android/sdk ANDROID_NDK_HOME=$ANDROID_HOME/ndk/<version> \
       JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
go install golang.org/x/mobile/cmd/gomobile@latest golang.org/x/mobile/cmd/gobind@latest
```

Without a release key configured, the APK is signed with your local debug key.

Without the apps, a computer can use the command line alone: `rymesh client` starts the local console
and opens it in the browser.

### Layout

```text
cmd/rymesh/      entry point
*.go             HTTP API, scheduling, multi-device relay and deployment, terminal and files
internal/
  agent/         driving the official CLIs: detection, commands, stdio protocols, approvals, task processes
  importer/      importing and syncing the CLIs' native history
  ingest/        storing and normalizing task events
  store/         SQLite: conversations, tasks, events, approvals, the encrypted vault
  config/ sshconn/ sys/ util/
web/             the front end (no build step)
macos/           the Mac app (Swift)
android/         the Android app
mobile/          gomobile bindings for Android
```

## Command line and configuration

A deployed node normally needs no attention. When it does, on the server:

```bash
rymesh status           # is it running
rymesh doctor           # self-check: agents, directories, listening addresses
rymesh tasks            # list tasks
rymesh cancel <task-id> # cancel a task
rymesh logs <task-id>   # a task's log
rymesh import           # import native history now (it syncs every 30 seconds anyway)
```

The configuration is `~/.rymesh/config.json` (`/etc/rymesh/config.json` for a root install made with
`rymesh install`). Common fields:

| Field | Default | Meaning |
|---|---|---|
| `default_cwd` | home directory | Working directory of new conversations |
| `allowed_roots` | home directory | Directories conversations may run in |
| `max_concurrent` | 4 | Tasks running at once (1–32) |
| `permission_ttl_minutes` | 60 | How long an approval request stays open; unanswered ones are denied |
| `import_on_start` | true | Import native history at start and sync every 30 seconds |
| `import_history_limit` | 0 | Events kept per conversation in the console; 0 keeps all |
| `import_exclude_roots` | `[]` | Directories not to import, `*` wildcards allowed |

`default_cwd` and `max_concurrent` can also be changed in Settings.

## Known limitations

- The interface is in Chinese only for now.
- Hermes can only continue conversations created over ACP; its other conversations are read-only.
- Codex MCP form requests (elicitation) are not supported yet and are declined.
- When Codex resumes a long-idle thread it sets the thread's "updated" time to now; that is Codex's own
  behaviour.
- Rebooting a server ends its running tasks; afterwards they show as interrupted and can be rerun or
  continued with a new message.

## Contributing

The project's rules, above all "only a remote control", are in [CLAUDE.md](CLAUDE.md).
`principle_test.go` fails the build if a model provider's API host or a credential variable appears in
the source.

## License

Copyright © 2026 ecouus

Rymesh is released under the [GNU Affero General Public License v3.0 (AGPL-3.0)](LICENSE): you may use,
modify and share it freely, but if you distribute a modified version, or offer one to others as a
network service, you must publish its complete source under the same license.
