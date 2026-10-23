/* Rymesh SPA — vanilla JS, no build step. */
(function () {
  "use strict";

  var $ = function (sel, root) { return (root || document).querySelector(sel); };
  var $$ = function (sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); };
  var esc = window.MD.esc;
  var md = window.MD.render;
  // T: the page's own text; TR: a finished message from a server (i18n.js).
  var T = window.I18N.T;
  var TR = window.I18N.TR;

  var LS = {
    get: function (k, d) { try { var v = localStorage.getItem("rymesh." + k); return v === null ? d : v; } catch (e) { return d; } },
    set: function (k, v) { try { localStorage.setItem("rymesh." + k, v); } catch (e) { /* private mode */ } },
    del: function (k) { try { localStorage.removeItem("rymesh." + k); } catch (e) {} }
  };

  var S = {
    csrf: "",
    booted: false,
    agents: [],
    config: {},
    counts: {},
    imports: {},
    sessions: [],
    profiles: {},
    tasks: [],
    activeTasks: [],
    perms: [],
    session: null,
    sessionTasks: [],
    sessionPerms: [],
    sessionActive: [],
    eventSeqs: Object.create(null),
    toolNodes: Object.create(null),
    lastSeq: 0,
    lastEv: null, // the newest event seen, for the activity line
    oldestSeq: 0,
    hasOlder: false,
    es: null,
    route: "home",
    draftAgent: LS.get("draftAgent", ""),
    draftCwd: LS.get("draftCwd", ""),
    sessionQuery: "",
    agentFilter: LS.get("agentFilter", "all"),
    pollTimer: null,
    conn: "off",
    sending: false,
    sidebarOpen: false,
    openToken: 0,
    focusPermission: "",
    settingsReturn: "",
    settingsCache: {},
    settingsPainted: "",
    approvalKeys: "",
    approvalTimer: null,
    approvalStackOpen: false,
    // Transcript paging is by conversation turns: a page always starts on a
    // user message, so a turn is never split across two pages. pageEvents and
    // olderEvents cap each page by raw event count as a safety net, because one
    // turn can be hundreds of events long and would otherwise make the first
    // paint heavy again.
    pageTurns: 10,
    olderTurns: 10,
    pageEvents: 400,
    olderEvents: 400,
    theme: LS.get("theme", "auto"),
    // The language chosen in Settings ("" = the system's); set at start-up.
    langPref: undefined,
    // Multi-device: the device whose agents the page shows ("local" = this
    // console's own machine), the device list, and approvals waiting on the
    // other devices.
    node: LS.get("node", "local"),
    nodeName: "",
    nodes: [],
    remotePerms: [],
    // Slash commands and model choice (slash.go): caches per device+agent, the
    // open conversation's settings, and the settings for a new conversation.
    commands: {},
    models: {},
    sessionSettings: {},
    draftSettings: {},
    slashItems: [],
    slashIdx: 0,
    permDecisions: Object.create(null),
    loadSeq: 0,
    loadPending: Object.create(null)
  };

  // ---------------------------------------------------------------- utilities

  function rid() {
    // crypto.randomUUID needs a secure context; HTTP-over-EasyTier may not be one.
    try {
      if (window.crypto && crypto.getRandomValues) {
        var b = new Uint8Array(16);
        crypto.getRandomValues(b);
        return Array.prototype.map.call(b, function (x) { return ("0" + x.toString(16)).slice(-2); }).join("");
      }
    } catch (e) {}
    return "r" + Date.now().toString(36) + Math.random().toString(36).slice(2, 10);
  }

  function rel(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    if (isNaN(d)) return "";
    var s = (Date.now() - d.getTime()) / 1000;
    if (s < 45) return T("刚刚");
    if (s < 3600) return T("%d 分钟前", Math.floor(s / 60));
    if (s < 86400) return T("%d 小时前", Math.floor(s / 3600));
    if (s < 7 * 86400) return T("%d 天前", Math.floor(s / 86400));
    return d.getFullYear() + "/" + (d.getMonth() + 1) + "/" + d.getDate();
  }

  function clock(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    if (isNaN(d)) return "";
    return d.toLocaleString(window.I18N.lang() === "en" ? "en-US" : "zh-CN",
      { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
  }

  function dur(start, end) {
    if (!start) return "";
    var a = new Date(start).getTime();
    var b = end ? new Date(end).getTime() : Date.now();
    var s = Math.max(0, Math.round((b - a) / 1000));
    if (s < 60) return T("%d 秒", s);
    if (s < 3600) return T("%d 分 %d 秒", Math.floor(s / 60), s % 60);
    return T("%d 小时 %d 分", Math.floor(s / 3600), Math.floor((s % 3600) / 60));
  }

  function pretty(v) {
    if (v === null || v === undefined || v === "") return "";
    if (typeof v === "string") {
      var t = v.trim();
      if ((t[0] === "{" && t[t.length - 1] === "}") || (t[0] === "[" && t[t.length - 1] === "]")) {
        try { return JSON.stringify(JSON.parse(t), null, 2); } catch (e) {}
      }
      return v;
    }
    try { return JSON.stringify(v, null, 2); } catch (e) { return String(v); }
  }

  function truncate(s, n) {
    s = String(s || "").replace(/\s+/g, " ").trim();
    return s.length > n ? s.slice(0, n - 1) + "…" : s;
  }

  function agentMeta(name) {
    for (var i = 0; i < S.agents.length; i++) if (S.agents[i].name === name) return S.agents[i];
    return { name: name, label: name, available: false };
  }

  // A small monochrome glyph identifies the agent in lists, so rows do not
  // need to spell out "Claude Code" / "Codex CLI" every time.
  var AGENT_ICONS = {
    claude: '<path d="M12 4v16M4 12h16M6.3 6.3l11.4 11.4M17.7 6.3 6.3 17.7"/>',
    codex: '<path d="m6 8 4 4-4 4M13 16h5"/>',
    hermes: '<path d="M7 5v14M17 5v14M7 12h10"/>'
  };
  function agentIcon(name) {
    var a = agentMeta(name);
    return '<svg class="ico agent-ico" viewBox="0 0 24 24" aria-label="' + esc(a.label || name) + '">' +
      (AGENT_ICONS[name] || '<circle cx="12" cy="12" r="5"/>') + "</svg>";
  }

  function greeting() {
    var h = new Date().getHours();
    if (h >= 5 && h < 11) return T("早上好");
    if (h >= 11 && h < 13) return T("中午好");
    if (h >= 13 && h < 18) return T("下午好");
    if (h >= 18 && h < 23) return T("晚上好");
    return T("夜深了");
  }

  function dirName(p) {
    return String(p || "").replace(/\/+$/, "").split("/").pop() || p || "";
  }

  function statusLabel(st, ahead) {
    // "排队中" is only true when something is actually ahead of the task.
    if (st === "pending") return ahead > 0 ? T("排队中") : T("启动中");
    var label = ({
      running: "运行中", waiting_input: "等待审批",
      completed: "已完成", failed: "失败", cancelled: "已取消", interrupted: "已中断", idle: "空闲"
    })[st];
    return label ? T(label) : st || "";
  }

  var TOOL_LABELS = {
    Bash: "终端", shell: "终端", Read: "读取", Write: "写入", Edit: "编辑",
    Glob: "查找文件", Grep: "搜索", WebFetch: "抓取网页", WebSearch: "搜索网页",
    Task: "子任务", TodoWrite: "待办", file_change: "文件变更", mcp_tool_call: "MCP",
    NotebookEdit: "编辑笔记本", MultiEdit: "批量编辑", AskUserQuestion: "提问",
    web_search: "搜索网页", permissions: "额外权限", request_user_input: "提问", ExitPlanMode: "计划"
  };

  function toolLabel(name) { return TOOL_LABELS[name] ? T(TOOL_LABELS[name]) : name || T("工具"); }

  function toolSummary(p) {
    var input = p.input;
    if (input && typeof input === "object") {
      if (input.command) return String(input.command);
      if (input.file_path) return String(input.file_path);
      if (input.pattern) return String(input.pattern);
      if (input.query) return String(input.query);
      if (input.url) return String(input.url);
      if (input.description) return String(input.description);
    }
    return truncate(pretty(input), 90);
  }

  // ---------------------------------------------------------------------- API

  // Requests for the selected device go through this console's relay
  // (/n/<device>/api/...). opts.hub keeps a request on this console itself
  // (login, password, device list); opts.node targets a specific device.
  function nodePath(path, node) {
    node = node === undefined ? S.node : node;
    return node && node !== "local" ? "/n/" + encodeURIComponent(node) + path : path;
  }

  // A hub-only console (the phone app) has no local machine: its first server
  // is already "another device", so the switcher always shows.
  function multiNode() { return S.nodes.length > 1 || (S.hubOnly && S.nodes.length > 0); }
  function noDevice() { return S.hubOnly && S.node === "local"; }

  function nodeLabel(id) {
    for (var i = 0; i < S.nodes.length; i++) if (S.nodes[i].id === id) return T(S.nodes[i].name);
    return id === "local" ? T("本机") : id;
  }

  function api(path, opts) {
    opts = opts || {};
    if (!opts.hub) path = nodePath(path, opts.node);
    var headers = {};
    if (opts.body !== undefined) headers["Content-Type"] = "application/json";
    if (opts.method && opts.method !== "GET" && opts.method !== "HEAD") {
      headers["X-CSRF-Token"] = S.csrf;
    }
    return fetch(path, {
      method: opts.method || "GET",
      headers: headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      credentials: "same-origin",
      cache: "no-store"
    }).then(function (res) {
      var ct = res.headers.get("content-type") || "";
      return (ct.indexOf("application/json") >= 0 ? res.json() : res.text()).then(function (data) {
        if (res.status === 401) {
          showLogin();
          throw new Error(T("登录已过期"));
        }
        if (!res.ok) {
          var msg = data && data.error ? data.error : "HTTP " + res.status;
          // The server's own text stays in err.raw for checks on it.
          var err = new Error(TR(msg));
          err.raw = msg;
          err.status = res.status;
          throw err;
        }
        return data;
      });
    });
  }

  // -------------------------------------------------------------------- toast

  var toastTimer = null;
  // Text from a server is translated here too, so callers can pass an
  // error or warning as it came.
  function toast(msg, isErr) {
    var t = $("#toast");
    t.textContent = TR(msg);
    // Resetting the class also cancels a fade in progress: a toast already
    // showing takes the new text and a fresh timer; only a hidden one plays
    // its entrance again.
    t.className = "toast" + (isErr ? " err" : "");
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      t.classList.add("out");
      toastTimer = setTimeout(function () { t.hidden = true; t.classList.remove("out"); }, 250);
    }, isErr ? 4200 : 2500);
  }

  // -------------------------------------------------------------------- sheet

  // The page scrolls unless a sheet is open, or a slide-over sidebar covers it.
  // The docked sidebar on wide screens never blocks scrolling.
  function syncScrollLock() {
    var lock = !$("#sheet").hidden || (!$("#sidebar").hidden && !wide.matches);
    document.body.style.overflow = lock ? "hidden" : "";
  }

  function openSheet(title, bodyHTML) {
    $("#sheet-title").textContent = title;
    // Replace the body node so listeners from a previous sheet cannot fire
    // against a newer sheet's controls.
    var old = $("#sheet-body");
    var fresh = document.createElement("div");
    fresh.id = "sheet-body";
    fresh.className = "sheet-body";
    fresh.innerHTML = bodyHTML;
    old.replaceWith(fresh);
    $("#sheet").hidden = false;
    syncScrollLock();
    return fresh;
  }
  function closeSheet() {
    $("#sheet").hidden = true;
    syncScrollLock();
  }
  $("#sheet").addEventListener("click", function (e) {
    if (e.target.closest("[data-close]")) closeSheet();
  });

  function confirmSheet(title, html, okLabel, danger) {
    return new Promise(function (resolve) {
      var body = openSheet(title, html +
        '<div class="row" style="margin-top:16px;gap:8px">' +
        '<button class="btn ghost grow" data-act="no">' + T("取消") + "</button>" +
        '<button class="btn ' + (danger ? "danger" : "primary") + ' grow" data-act="yes">' +
        esc(okLabel || T("确认")) + "</button></div>");
      body.addEventListener("click", function (e) {
        var b = e.target.closest("[data-act]");
        if (!b) return;
        closeSheet();
        resolve(b.getAttribute("data-act") === "yes");
      });
    });
  }

  // --------------------------------------------------------------------- auth

  // There is no password login: the apps and `rymesh client open` sign in
  // with a one-time ticket, hubs reach nodes with a token. Without a session
  // the page can only say where to open it from.
  function showLogin() {
    closeStream();
    S.booted = false;
    closeSidebar(true);
    closeSheet();
    $("#approval-stack").hidden = true;
    $("#app").hidden = true;
    $("#login").hidden = false;
    if (S.pollTimer) { clearInterval(S.pollTimer); S.pollTimer = null; }
  }

  function showApp() {
    $("#login").hidden = true;
    $("#app").hidden = false;
  }

  // --------------------------------------------------------------------- boot

  // Boot data of each device is kept (in the hub's database) so the next
  // start can draw the last known state at once; reaching a distant server
  // over SSH takes seconds.
  function snapshotPath() { return "/api/nodes/" + encodeURIComponent(S.node) + "/snapshot"; }

  function applyBoot(d, cached) {
    S.booted = true;
    S.nodeName = d.node_name || "";
    if (!cached) S.csrf = d.csrf || S.csrf;
    S.agents = d.agents || [];
    S.profiles = trProfiles(d.profiles || {});
    S.config = d.config || {};
    S.counts = d.counts || {};
    S.imports = d.import || {};
    S.sessions = d.sessions || [];
    S.tasks = d.tasks || [];
    S.perms = d.permissions || [];
    S.activeTasks = S.tasks.filter(isActiveTask);
    ensureDraftAgent();
    if (!S.draftCwd) S.draftCwd = S.config.default_cwd || "";
    // Warm the settings page so it opens without a round trip.
    if (!cached && !S.settingsCache[S.node]) {
      var node = S.node;
      api("/api/settings").then(function (s) { S.settingsCache[node] = s; }).catch(function () {});
    }
  }

  // Permission modes come from the device in its own wording.
  function trProfiles(all) {
    var out = {};
    Object.keys(all).forEach(function (agent) {
      out[agent] = (all[agent] || []).map(function (p) {
        var c = {};
        for (var k in p) c[k] = p[k];
        c.label = TR(p.label);
        c.detail = TR(p.detail);
        return c;
      });
    });
    return out;
  }

  // Connection to the current device at start-up: "connecting", "failed"
  // or "ok". The welcome page shows it where recent conversations go.
  S.link = { state: "ok" };
  var linkTimer = null;
  function setLink(state, error) {
    S.link = { state: state, error: error || "", since: S.link.state === state ? S.link.since : Date.now() };
    clearInterval(linkTimer);
    if (state === "connecting") {
      linkTimer = setInterval(function () {
        var el = $("#conn-elapsed");
        if (el) el.textContent = T("%d 秒", Math.round((Date.now() - S.link.since) / 1000));
      }, 1000);
    }
    if (S.route === "home" && S.booted) renderHome();
  }

  function connCard() {
    var n = S.nodes.filter(function (x) { return x.id === S.node; })[0] || {};
    var name = esc(nodeLabel(S.node)), target = esc(n.target || "");
    if (S.link.state === "failed") {
      return '<div class="conn-card failed"><div class="conn-title">' + T("无法连接 %s", name) + "</div>" +
        '<div class="conn-sub">' + esc(TR(S.link.error) || T("连接失败")) + "</div>" +
        '<button class="btn sm" type="button" data-reboot="1">' + T("重试") + "</button></div>";
    }
    var secs = Math.round((Date.now() - (S.link.since || Date.now())) / 1000);
    return '<div class="conn-card"><div class="conn-anim" aria-hidden="true">' +
      '<svg class="ico" viewBox="0 0 24 24"><rect x="7" y="3" width="10" height="18" rx="2.5"/><path d="M11 18h2"/></svg>' +
      '<span class="conn-dots"><i></i><i></i><i></i></span>' +
      '<svg class="ico" viewBox="0 0 24 24"><rect x="4" y="4" width="16" height="7" rx="1.5"/><rect x="4" y="13" width="16" height="7" rx="1.5"/><path d="M8 7.5h.01M8 16.5h.01"/></svg>' +
      "</div>" +
      '<div class="conn-title">' + T("正在连接 %s", name) + "</div>" +
      '<div class="conn-sub">' + T("通过 SSH") + (target ? " · " + target : "") + ' · <span id="conn-elapsed">' + T("%d 秒", secs) + "</span></div></div>";
  }

  function boot() {
    // The device list decides which device to load (a hub-only console has no
    // local one), so it comes first. The fast listing answers from the hub's
    // own records without waiting for SSH; live status follows.
    var shown = false;
    return loadNodes(true).then(function () {
      if (S.hubOnly && !S.nodes.some(function (n) { return n.id === S.node; })) {
        var first = S.nodes.filter(function (n) { return n.online; })[0] || S.nodes[0];
        S.node = first ? first.id : "local";
        LS.set("node", S.node);
        if (first) useNode(S.node);
        S.draftCwd = ""; // a directory on the previous device means nothing here
        LS.del("draftCwd");
        renderNodeSwitch(); // it was drawn before the device was chosen
      }
      if (S.node === "local") return api("/api/bootstrap");
      setLink("connecting");
      return api(snapshotPath(), { hub: true }).catch(function () { return null; }).then(function (cached) {
        // The welcome page comes up at once, from the last snapshot if there
        // is one; the connection shows in place of recent conversations.
        showApp();
        updateThemeColor();
        applyBoot(cached || {}, true);
        route(true);
        dockSidebar();
        shown = true;
        return api("/api/bootstrap");
      });
    }).then(function (d) {
      var where = S.route, sid = S.session && S.session.id;
      applyBoot(d, false);
      S.link = { state: "ok" };
      clearInterval(linkTimer);
      if (S.node !== "local") {
        api(snapshotPath(), { method: "PUT", hub: true, body: { node_name: d.node_name, agents: d.agents, profiles: d.profiles,
          config: d.config, counts: d.counts, sessions: (d.sessions || []).slice(0, 100) } }).catch(function () {});
      }
      showApp();
      updateThemeColor();
      renderApprovalNotifications();
      // An open conversation already loaded itself; just refresh around it.
      if (shown && where === "chat" && sid) renderSessionList();
      else route(true);
      dockSidebar();
      startPolling();
      if (S.node !== "local" || S.nodes.length > 1) loadNodes(); // live status
    }).catch(function (err) {
      if (err && err.status === 401) { setLink("ok"); showLogin(); return; }
      // The welcome page says what is wrong and offers to retry; the device
      // stays selected so the user can retry it or switch away.
      if (S.node !== "local" && shown) {
        setLink("failed", (err && err.message) || "");
        if (S.route !== "home") toast(T("无法连接 %s", nodeLabel(S.node)), true);
        return;
      }
      setLink("ok");
      // An unreachable device must not lock the user out: fall back to this
      // console's own machine.
      if (S.node !== "local") {
        toast((err && err.message) || T("设备不可用"), true);
        S.node = "local";
        LS.set("node", "local");
        return boot();
      }
      showLogin(); toast(err.message || T("无法连接服务"), true);
    });
  }
  // Tell the hub which device is in use (it remembers it, and a retry
  // skips any reconnect backoff).
  function useNode(id) {
    return api("/api/nodes/" + encodeURIComponent(id) + "/use", { method: "POST", hub: true, body: {} }).catch(function () {});
  }

  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-reboot]");
    if (!b) return;
    b.disabled = true;
    setLink("connecting");
    useNode(S.node).then(boot);
  });

  var nodesLoaded = false;
  function loadNodes(fast) {
    return api("/api/nodes" + (fast ? "?fast=1" : ""), { hub: true }).then(function (d) {
      S.nodes = d.nodes || [];
      S.hubOnly = !!d.hub_only;
      // The hub's record of the device in use outlives the page's storage;
      // it only decides at start-up (a switch in progress is newer).
      var startup = !nodesLoaded;
      nodesLoaded = true;
      if (startup && d.current && d.current !== S.node &&
          (d.current === "local" ? !S.hubOnly : S.nodes.some(function (n) { return n.id === d.current; }))) {
        S.node = d.current;
        LS.set("node", S.node);
      }
      if (S.node !== "local" && !S.nodes.some(function (n) { return n.id === S.node; })) {
        S.node = "local";
        LS.set("node", "local");
      }
      renderNodeSwitch();
    }).catch(function () {});
  }

  // Switching device reloads everything from that device. hash is where to
  // land (an approval on another device opens its session directly).
  function switchNode(id, hash) {
    if (id === S.node && !hash) return;
    closeStream();
    closeSheet();
    S.booted = false; // route() waits for the new device's bootstrap
    S.node = id;
    LS.set("node", id);
    useNode(id);
    S.draftCwd = "";
    LS.del("draftCwd");
    S.session = null;
    S.sessions = [];
    S.agentFilter = "all";
    location.hash = hash || "#/";
    boot();
  }

  function renderNodeSwitch() {
    var t = $("#sidebar-title");
    if (!t) return;
    if (!multiNode()) { t.textContent = T("对话"); return; }
    var cur = S.nodes.filter(function (n) { return n.id === S.node; })[0];
    t.innerHTML = '<button type="button" id="node-switch" class="node-switch" aria-haspopup="listbox" aria-expanded="false">' +
      '<span class="node-dot' + (cur && !cur.online ? " off" : "") + '"></span>' +
      esc(cur ? T(cur.name) : nodeLabel(S.node)) +
      '<svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 10 5 5 5-5"/></svg></button>';
  }

  function nodeRows() {
    return S.nodes.map(function (n) {
      // An offline device can still be chosen: its welcome page says why it
      // cannot be reached and offers a retry.
      var state = n.online ? (n.local ? T("本机") : n.target || "") : n.checking ? T("连接中…") : (TR(n.error) || T("离线"));
      return '<button type="button" class="menu-item node-item' + (n.id === S.node ? " on" : "") +
        (n.online || n.checking ? "" : " offline") + '" data-node="' + esc(n.id) + '">' +
        '<span class="node-dot' + (n.online ? "" : " off") + '"></span>' +
        '<span class="menu-text"><span class="menu-label">' + esc(T(n.name)) + "</span>" +
        '<span class="menu-detail">' + esc(state) + "</span></span>" +
        '<svg class="ico sm menu-check" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg></button>';
    }).join("");
  }

  function settingsDeviceRows() {
    return S.nodes.map(function (n) {
      var sub = n.local ? T("本机") : (n.target || n.url || "");
      return '<button type="button" class="g-row link" ' + (n.local ? 'data-node="local"' : 'data-device="' + esc(n.id) + '"') + ">" +
        '<span class="node-dot' + (n.online ? "" : " off") + '"></span>' +
        '<span class="g-label">' + esc(T(n.name)) + ' <span class="muted small">' + esc(sub) + "</span></span>" +
        '<span class="g-value">' + esc(n.online ? (n.id === S.node ? T("当前") : (n.version || "")) : (TR(n.error) || T("离线"))) + "</span></button>";
    }).join("");
  }

  // ------------------------------------------------------------ native bridge

  // The Android app injects RymeshNative; the Mac app listens on the
  // "ac" message handler. In a plain browser both are absent and the web
  // fallbacks apply.
  var androidNative = window.RymeshNative || null;
  var macNative = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.ac || null;

  // writeClipboard resolves to whether the text really reached the clipboard.
  function writeClipboard(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).then(function () { return true; }, function () { return fallbackCopy(text); });
    }
    return Promise.resolve(fallbackCopy(text));
  }
  function fallbackCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
    document.body.appendChild(ta); ta.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    ta.remove();
    return ok;
  }

  function copyText(text, msg) {
    writeClipboard(text).then(function (ok) { toast(ok ? msg || T("已复制") : T("复制失败"), !ok); });
  }

  function canShare() { return !!(androidNative || navigator.share); }
  function shareText(text, title) {
    if (androidNative) { androidNative.share(text, title || ""); return; }
    if (navigator.share) { navigator.share({ title: title || "", text: text }).catch(function () {}); return; }
    copyText(text);
  }

  // saveTextFile hands a text file to the user: the system save dialog in the
  // apps, a download in a browser.
  function saveTextFile(name, text) {
    if (androidNative) { androidNative.saveFile(name, text); return; }
    if (macNative) { macNative.postMessage({ op: "save", name: name, text: text }); return; }
    var a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([text], { type: "application/octet-stream" }));
    a.download = name;
    document.body.appendChild(a);
    a.click();
    setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
  }

  // ------------------------------------------------------------------ overlay

  // A second layer above the sheet for pickers and short prompts, so choosing
  // something inside a form does not throw the form away.
  function openOverlay(title, html) {
    var el = document.createElement("div");
    el.className = "overlay";
    el.innerHTML = '<div class="overlay-backdrop" data-ov-close="1"></div>' +
      '<div class="overlay-panel" role="dialog" aria-modal="true">' +
      (title ? '<div class="overlay-head">' + esc(title) + "</div>" : "") +
      '<div class="overlay-body">' + html + "</div></div>";
    document.body.appendChild(el);
    var closed = false, onClose = null;
    function close() {
      if (closed) return;
      closed = true;
      el.remove();
      overlays.splice(overlays.indexOf(handle), 1);
      if (onClose) onClose();
    }
    var handle = { el: el, body: el.querySelector(".overlay-body"), close: close,
      onClose: function (fn) { onClose = fn; } };
    overlays.push(handle);
    el.addEventListener("click", function (e) { if (e.target.closest("[data-ov-close]")) close(); });
    return handle;
  }
  var overlays = [];

  // pickList shows choices in a bottom panel and resolves with the chosen
  // value, or null if dismissed. Items: {value, label, sub, tag, selected,
  // action} — an item with action is drawn as a command row.
  function pickList(title, items) {
    return new Promise(function (resolve) {
      var ov = openOverlay(title, '<div class="pick-list">' + items.map(function (it, i) {
        if (it.section) return '<div class="pick-section">' + esc(it.section) + "</div>";
        return '<button type="button" class="pick-row' + (it.action ? " action" : "") + (it.danger ? " danger" : "") + (it.selected ? " on" : "") +
          '" data-pick="' + i + '"' + (it.disabled ? " disabled" : "") + ">" +
          '<span class="pick-text"><span class="pick-label">' + esc(it.label) +
          (it.tag ? ' <span class="pick-tag">' + esc(it.tag) + "</span>" : "") + "</span>" +
          (it.sub ? '<span class="pick-sub">' + esc(it.sub) + "</span>" : "") + "</span>" +
          (it.selected ? '<svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg>' : "") +
          "</button>";
      }).join("") + "</div>");
      var chosen = null;
      ov.onClose(function () { resolve(chosen); });
      ov.body.addEventListener("click", function (e) {
        var b = e.target.closest("[data-pick]");
        if (!b) return;
        chosen = items[+b.getAttribute("data-pick")].value;
        ov.close();
      });
    });
  }

  // askText asks for one line of text; resolves with it, or null.
  function askText(title, o) {
    o = o || {};
    return new Promise(function (resolve) {
      var ov = openOverlay(title,
        (o.note ? '<p class="small muted" style="margin:0 0 12px">' + o.note + "</p>" : "") +
        '<form class="ask-form"><label class="field"><span>' + esc(o.label || "") + "</span>" +
        '<input name="v" type="' + (o.type || "text") + '" value="' + esc(o.value || "") + '" placeholder="' + esc(o.placeholder || "") +
        '" autocomplete="off" autocapitalize="off" spellcheck="false"></label>' +
        '<div class="row" style="gap:8px"><button type="button" class="btn ghost grow" data-ov-close="1">' + T("取消") + "</button>" +
        '<button type="submit" class="btn primary grow">' + esc(o.ok || T("确定")) + "</button></div></form>");
      var out = null;
      ov.onClose(function () { resolve(out); });
      var input = ov.body.querySelector("input");
      setTimeout(function () { input.focus(); input.select(); }, 60);
      ov.body.querySelector("form").addEventListener("submit", function (e) {
        e.preventDefault();
        out = input.value;
        ov.close();
      });
    });
  }

  // ------------------------------------------------------------ device setup

  // A POST whose answer is an event stream (deploy progress).
  function postStream(path, onEvent) {
    return fetch(path, { method: "POST", credentials: "same-origin", cache: "no-store",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": S.csrf }, body: "{}" }).then(function (res) {
      if (!res.ok || !res.body) return res.json().then(function (d) { throw new Error(d.error || "HTTP " + res.status); });
      var reader = res.body.getReader(), dec = new TextDecoder(), buf = "";
      function pump() {
        return reader.read().then(function (r) {
          if (r.done) return;
          buf += dec.decode(r.value, { stream: true });
          var parts = buf.split("\n\n");
          buf = parts.pop();
          parts.forEach(function (chunk) {
            var ev = "message", data = "";
            chunk.split("\n").forEach(function (l) {
              if (l.indexOf("event: ") === 0) ev = l.slice(7);
              else if (l.indexOf("data: ") === 0) data += l.slice(6);
            });
            try { onEvent(ev, JSON.parse(data)); } catch (e) {}
          });
          return pump();
        });
      }
      return pump();
    });
  }

  function shortFp(fp) { return String(fp || "").replace(/^SHA256:/, "").slice(0, 12); }

  // keyChoices turns /api/keys into picker items. Values: "id:<vault id>",
  // "path:<file>", "agent", "new".
  function keyChoices(d, current, withNew) {
    var items = [];
    (d.keys || []).forEach(function (k) {
      items.push({ value: "id:" + k.id, label: k.name, sub: k.type.replace("ssh-", "") + " · " + shortFp(k.fingerprint) +
        (k.used_by.length ? " · " + T("用于 %s", k.used_by.join(T("、"))) : ""), selected: current === "id:" + k.id });
    });
    var files = (d.detected || []).filter(function (k) { return !k.imported; });
    if (files.length) items.push({ section: T("~/.ssh 中的密钥") });
    files.forEach(function (k) {
      items.push({ value: "path:" + k.path, label: k.name, tag: k.encrypted ? T("有密码短语") : "",
        sub: shortFp(k.fingerprint) + (k.encrypted ? " · " + T("使用前需导入") : ""), selected: current === "path:" + k.path });
    });
    if (d.agent) items.push({ value: "agent", label: "ssh-agent", sub: T("使用 ssh-agent 里已加载的密钥"), selected: current === "agent" });
    if (withNew) items.push({ value: "new", label: T("生成新密钥…"), action: true });
    return items;
  }

  function keyLabel(d, v) {
    if (!v) return "";
    if (v === "agent") return "ssh-agent";
    var k = (d.keys || []).filter(function (x) { return "id:" + x.id === v; })[0];
    if (k) return k.name;
    var f = (d.detected || []).filter(function (x) { return "path:" + x.path === v; })[0];
    return f ? T("%s（~/.ssh）", f.name) : "";
  }

  function keyValueOf(dev) {
    if (dev.key_id) return "id:" + dev.key_id;
    if (dev.key_path) return "path:" + dev.key_path;
    return dev.use_agent ? "agent" : "";
  }

  function keyFields(v) {
    return { key_id: v.indexOf("id:") === 0 ? v.slice(3) : "", key_path: v.indexOf("path:") === 0 ? v.slice(5) : "",
      use_agent: v === "agent" };
  }

  // generateKeyFlow asks for a name and creates a key in the vault.
  function generateKeyFlow() {
    var def = (S.nodeName ? T(S.nodeName) : T("设备")) + "-" + new Date().toISOString().slice(5, 10).replace("-", "");
    return askText(T("生成新密钥"), { label: T("名称（便于在服务器上认出它）"), value: def, ok: T("生成"),
      note: T("生成一把 ed25519 密钥，私钥保存在本应用的数据库里。") }).then(function (name) {
      if (name === null) return null;
      return api("/api/keys/generate", { method: "POST", body: { name: name.trim() }, hub: true }).then(function (d) {
        toast(T("已生成密钥「%s」", d.key.name));
        return d.key;
      });
    }).catch(function (e) { toast(e.message, true); return null; });
  }

  // importKeyFlow copies a ~/.ssh key into the vault, asking for its
  // passphrase if it has one.
  function importKeyFlow(k) {
    function go(pass) {
      return api("/api/keys/import", { method: "POST", hub: true, body: { path: k.path, passphrase: pass || "" } })
        .then(function (d) { toast(T("已导入「%s」", d.key.name)); return d.key; });
    }
    var first = k.encrypted ? askText(T("导入 %s", k.name), { label: T("密码短语"), type: "password", ok: T("导入"),
      note: T("只用这一次：解开后由本应用保存，以后不再需要密码短语。") }).then(function (p) { return p === null ? null : go(p); }) : go("");
    return first.catch(function (e) { toast(e.message, true); return null; });
  }

  // pubBox shows a public key with copy and share; compact leaves out the
  // key text (the add-server form, for a key the user already knows).
  function pubBox(pub, fresh, compact) {
    if (compact) {
      return '<div class="key-card compact"><span class="small muted grow">' + T("公钥") + "</span>" +
        '<button type="button" class="btn sm" data-copy-text="' + esc(pub) + '">' + T("复制") + "</button>" +
        (canShare() ? '<button type="button" class="btn sm" data-share-text="' + esc(pub) + '">' + T("分享…") + "</button>" : "") + "</div>";
    }
    return '<div class="key-card' + (fresh ? " fresh" : "") + '">' +
      (fresh ? '<div class="key-card-head">' + T("新密钥已生成") + "</div>" : "") +
      '<div class="small muted">' + T("公钥（可以公开）：") + '</div><pre class="mono">' + esc(pub) + "</pre>" +
      '<div class="row" style="gap:8px"><button type="button" class="btn sm grow" data-copy-text="' + esc(pub) + '">' + T("复制公钥") + "</button>" +
      (canShare() ? '<button type="button" class="btn sm grow" data-share-text="' + esc(pub) + '">' + T("分享…") + "</button>" : "") +
      "</div>" + (fresh ? '<div class="small muted" style="margin-top:8px">' + T("连接时可以用服务器密码自动安装它，或者手动加入服务器的 <code>~/.ssh/authorized_keys</code>。") + "</div>" : "") +
      "</div>";
  }

  document.addEventListener("click", function (e) {
    var c = e.target.closest("[data-copy-text]");
    if (c) { copyText(c.getAttribute("data-copy-text")); return; }
    var s = e.target.closest("[data-share-text]");
    if (s) shareText(s.getAttribute("data-share-text"), T("公钥"));
  });

  function addDeviceSheet() {
    var body = openSheet(T("添加服务器"),
      '<div class="stack device-form">' +
      '<button type="button" class="g-row link pick-btn" id="dev-import" hidden><span class="g-label">' + T("从 ~/.ssh/config 填入") + '</span><span class="g-value"></span></button>' +
      '<label class="field"><span>' + T("名称") + '</span><input id="dev-name" placeholder="' + esc(T("例如 生产服务器")) + '" maxlength="60"></label>' +
      '<div class="row"><label class="field grow"><span>' + T("主机") + '</span><input id="dev-host" class="mono" placeholder="' + esc(T("IP 或域名")) + '" autocapitalize="off" spellcheck="false"></label>' +
      '<label class="field" style="width:84px"><span>' + T("SSH 端口") + '</span><input id="dev-port" class="mono" inputmode="numeric" value="22"></label></div>' +
      '<label class="field"><span>' + T("用户") + '</span><input id="dev-user" class="mono" value="root" autocapitalize="off" spellcheck="false"></label>' +
      '<div class="field"><span>' + T("登录方式") + '</span><span class="seg auth-seg" role="radiogroup">' +
      '<button type="button" class="on" data-auth="key" role="radio" aria-checked="true">' + T("密钥") + "</button>" +
      '<button type="button" data-auth="password" role="radio" aria-checked="false">' + T("密码") + "</button></span></div>" +
      '<div class="field" id="dev-key-field"><span>' + T("认证密钥") + '</span><button type="button" class="pick-btn input-like" id="dev-key"><span class="t"></span>' +
      '<svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 10 5 5 5-5"/></svg></button></div>' +
      '<div id="dev-pub"></div>' +
      '<label class="field" id="dev-pw-field" hidden><span>' + T("服务器登录密码") + "</span>" +
      '<input id="dev-pw" type="password" autocomplete="new-password" autocapitalize="off" spellcheck="false"></label>' +
      '<p class="small muted" id="dev-pw-note" hidden style="margin:0">' +
      T("适用于只允许密码登录的服务器。密码加密保存在本应用里，每次连接时使用，网页和导出文件之外的地方都看不到它。服务器支持密钥时，用密钥更安全。") + "</p>" +
      '<details class="fold"><summary>' + T("高级") + '</summary><label class="field" style="margin-top:8px"><span>' + T("节点端口（服务器本机回环端口）") + "</span>" +
      '<input id="dev-rport" class="mono" inputmode="numeric" value="8899"></label></details>' +
      '<button id="dev-save" class="btn primary block" type="button">' + T("连接") + "</button>" +
      '<div id="dev-flow"></div></div>');
    var keys = { keys: [], detected: [] }, chosen = "", fresh = "", auth = "key";
    body.querySelector(".auth-seg").addEventListener("click", function (e) {
      var b = e.target.closest("[data-auth]");
      if (!b) return;
      auth = b.getAttribute("data-auth");
      $$("[data-auth]", body).forEach(function (x) {
        var on = x === b;
        x.classList.toggle("on", on);
        x.setAttribute("aria-checked", String(on));
      });
      $("#dev-key-field").hidden = $("#dev-pub").hidden = auth !== "key";
      $("#dev-pw-field").hidden = $("#dev-pw-note").hidden = auth !== "password";
      if (auth === "password") setTimeout(function () { $("#dev-pw").focus(); }, 30);
    });
    function renderKey() {
      var label = keyLabel(keys, chosen);
      var b = $("#dev-key");
      b.querySelector(".t").textContent = label || T("选择或生成密钥");
      b.classList.toggle("placeholder", !label);
      var k = (keys.keys || []).filter(function (x) { return "id:" + x.id === chosen; })[0] ||
        (keys.detected || []).filter(function (x) { return "path:" + x.path === chosen; })[0];
      $("#dev-pub").innerHTML = k && k.public ? pubBox(k.public, chosen === fresh, chosen !== fresh) : "";
    }
    function loadKeys(select) {
      return api("/api/keys", { hub: true }).then(function (d) {
        keys = d;
        if (select) chosen = select;
        else if (!chosen) {
          var first = keyChoices(d, "", false).filter(function (x) { return x.value; })[0];
          chosen = first ? first.value : "";
        }
        renderKey();
      });
    }
    loadKeys().catch(function (e) { toast(e.message, true); });

    var hosts = [];
    api("/api/ssh/hosts", { hub: true }).then(function (d) {
      hosts = d.hosts || [];
      if (hosts.length) $("#dev-import").hidden = false;
    }).catch(function () {});
    $("#dev-import").addEventListener("click", function () {
      pickList(T("从 ~/.ssh/config 填入"), hosts.map(function (h, i) {
        return { value: i, label: h.alias, sub: (h.user ? h.user + "@" : "") + h.host + (h.port && h.port !== 22 ? ":" + h.port : "") };
      })).then(function (i) {
        var h = hosts[i];
        if (!h) return;
        $("#dev-name").value = h.alias; $("#dev-host").value = h.host; $("#dev-port").value = h.port || 22;
        if (h.user) $("#dev-user").value = h.user;
        if (h.key_path) {
          var want = h.key_path.replace(/^~\//, "");
          var m = (keys.detected || []).filter(function (k) { return k.path.slice(-want.length) === want; })[0];
          if (m) {
            var v = (keys.keys || []).filter(function (k) { return k.fingerprint === m.fingerprint; })[0];
            chosen = v ? "id:" + v.id : "path:" + m.path;
            renderKey();
          }
        }
      });
    });
    $("#dev-key").addEventListener("click", function () {
      pickList(T("认证密钥"), keyChoices(keys, chosen, true)).then(function (v) {
        if (v === "new") {
          return generateKeyFlow().then(function (k) {
            if (!k) return;
            fresh = "id:" + k.id;
            return loadKeys(fresh);
          });
        }
        if (!v) return;
        var f = (keys.detected || []).filter(function (x) { return "path:" + x.path === v; })[0];
        if (f && f.encrypted) {
          return importKeyFlow(f).then(function (k) { if (k) return loadKeys("id:" + k.id); });
        }
        chosen = v;
        renderKey();
      });
    });
    $("#dev-save").addEventListener("click", function () {
      var pw = $("#dev-pw").value;
      if (auth === "key" && !chosen) { toast(T("请先选择或生成一把密钥"), true); return; }
      if (auth === "password" && !pw) { toast(T("请输入服务器登录密码"), true); $("#dev-pw").focus(); return; }
      var req = Object.assign({
        transport: "ssh", name: $("#dev-name").value.trim(), ssh_host: $("#dev-host").value.trim(),
        ssh_port: parseInt($("#dev-port").value, 10) || 22, ssh_user: $("#dev-user").value.trim(),
        remote_port: parseInt($("#dev-rport").value, 10) || 8899
      }, auth === "key" ? keyFields(chosen) : { password: pw });
      var btn = this; btn.disabled = true;
      api("/api/devices", { method: "POST", body: req, hub: true }).then(function (d) {
        $("#dev-pw").value = "";
        btn.hidden = true;
        $$(".device-form > .field, .device-form > .row, .device-form > details, .device-form > .pick-btn, #dev-pub, #dev-pw-note", body)
          .forEach(function (el) { el.hidden = true; });
        deviceFlow(d.device.id, $("#dev-flow"));
      }).catch(function (e) { toast(e.message, true); btn.disabled = false; });
    });
  }

  // deviceFlow drives test → confirm fingerprint → install key → deploy
  // inside a sheet.
  function deviceFlow(id, box) {
    var path = "/api/devices/" + encodeURIComponent(id);
    box.innerHTML = '<div class="flow-step"><span class="spinner"></span> ' + T("正在连接…") + "</div>";
    api(path + "/test", { method: "POST", body: {}, hub: true }).then(function (r) {
      if (r.stage === "hostkey") {
        var hk = r.hostkey;
        box.innerHTML = '<div class="flow-step">' +
          (hk.mismatch ? '<div class="error-block">' + T("警告：这台主机的密钥和 ~/.ssh/known_hosts 里记录的不一致。除非你确认服务器重装过，否则不要信任。") + "</div>" : "") +
          "<div>" + T("首次连接，请核对服务器的主机指纹：") + "</div><pre class=\"mono fp\">" + esc(hk.fingerprint) + "</pre>" +
          '<div class="small muted">' + T("可在服务器上运行 <code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code> 对照。") + "</div>" +
          '<div class="row" style="margin-top:12px"><button class="btn primary grow" type="button" data-trust="1">' + T("信任并继续") + "</button></div></div>";
        box.querySelector("[data-trust]").addEventListener("click", function () {
          this.disabled = true;
          api(path + "/trust", { method: "POST", body: { key: hk.key }, hub: true })
            .then(function () { deviceFlow(id, box); })
            .catch(function (e) { toast(e.message, true); deviceFlow(id, box); });
        });
        return;
      }
      if (r.stage === "auth") {
        authStep(id, box, r);
        return;
      }
      if (r.stage === "error") {
        box.innerHTML = '<div class="flow-step"><div class="error-block">' + esc(TR(r.error)) + "</div>" +
          '<button class="btn block" type="button" data-retry="1">' + T("重试") + "</button></div>";
        box.querySelector("[data-retry]").addEventListener("click", function () { deviceFlow(id, box); });
        return;
      }
      var online = r.node && r.node.online;
      box.innerHTML = '<div class="flow-step"><div class="group">' +
        '<div class="g-row"><span class="g-label">' + T("连接") + '</span><span class="g-value">' + T("SSH 已连接 · %s", esc(r.platform || "")) + "</span></div>" +
        '<div class="g-row"><span class="g-label">' + T("节点") + '</span><span class="g-value">' +
        esc(r.installed ? r.installed + " · " + (online ? T("运行中") : T("未运行")) : T("未安装")) + "</span></div></div>" +
        '<button class="btn ' + (online ? "" : "primary ") + 'block" type="button" data-deploy="1" style="margin-top:12px">' +
        (r.installed ? T("升级 / 重新部署节点") : T("部署节点")) + "</button>" +
        (online && S.node !== id ? '<button class="btn primary block" type="button" data-use="1" style="margin-top:8px">' + T("切换到这台设备") + "</button>" : "") +
        '<pre class="deploy-log mono" hidden></pre></div>';
      var log = box.querySelector(".deploy-log");
      var use = box.querySelector("[data-use]");
      if (use) use.addEventListener("click", function () { loadNodes().then(function () { switchNode(id); }); });
      box.querySelector("[data-deploy]").addEventListener("click", function () {
        var b = this; b.disabled = true; log.hidden = false; log.textContent = "";
        postStream(path + "/deploy", function (ev, d) {
          if (ev === "progress") log.textContent += TR(d.text) + "\n";
          else if (ev === "failed") { log.textContent += "✕ " + TR(d.error) + "\n"; b.disabled = false; }
          else if (ev === "done") { log.textContent += "✓ " + T("部署完成") + "\n"; loadNodes().then(function () { deviceFlow(id, box); }); }
        }).catch(function (e) { log.textContent += "✕ " + e.message + "\n"; b.disabled = false; });
      });
    }).catch(function (e) {
      box.innerHTML = '<div class="flow-step"><div class="error-block">' + esc(e.message) + "</div></div>";
    });
  }

  // authStep: the server refused the key. Install it with the server password
  // once (as ssh-copy-id does), or add it by hand and retry.
  function authStep(id, box, r) {
    var path = "/api/devices/" + encodeURIComponent(id);
    if (r.password_auth && !r.public) {
      passwordStep(id, box, r.error);
      return;
    }
    if (!r.public) {
      box.innerHTML = '<div class="flow-step"><div class="error-block">' + T("服务器不接受 ssh-agent 里的密钥。") + "</div>" +
        '<button class="btn block" type="button" data-retry="1">' + T("重试") + "</button></div>";
      box.querySelector("[data-retry]").addEventListener("click", function () { deviceFlow(id, box); });
      return;
    }
    var cmd = "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '" + r.public + "' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys";
    box.innerHTML = '<div class="flow-step">' +
      '<div class="notice">' + T("服务器还不认识这把密钥。用下面任一方式把公钥放到服务器上：") + "</div>" +
      '<div class="choice-card"><div class="choice-title">' + T("用服务器密码自动安装") + "</div>" +
      '<form data-install="1"><input class="input" type="password" name="pw" placeholder="' + esc(T("服务器登录密码")) + '" autocomplete="off">' +
      '<button class="btn primary block" type="submit" style="margin-top:8px">' + T("安装公钥并连接") + "</button></form>" +
      '<div class="small muted" style="margin-top:6px">' + T("密码只用于这一次连接，不会保存；需要服务器允许密码登录。") + "</div></div>" +
      '<div class="choice-card"><div class="choice-title">' + T("手动添加") + "</div>" +
      '<div class="small muted">' + T("在服务器上执行这条命令（或把公钥加入 <code>~/.ssh/authorized_keys</code>）：") + "</div>" +
      '<pre class="mono">' + esc(cmd) + "</pre>" +
      '<div class="row" style="gap:8px"><button type="button" class="btn sm grow" data-copy-text="' + esc(cmd) + '">' + T("复制命令") + "</button>" +
      '<button type="button" class="btn sm grow" data-copy-text="' + esc(r.public) + '">' + T("复制公钥") + "</button>" +
      (canShare() ? '<button type="button" class="btn sm grow" data-share-text="' + esc(r.public) + '">' + T("分享…") + "</button>" : "") + "</div>" +
      '<button class="btn block" type="button" data-retry="1" style="margin-top:10px">' + T("已添加，重新连接") + "</button></div>" +
      '<div class="choice-card"><div class="choice-title">' + T("改用密码登录") + "</div>" +
      '<div class="small muted">' + T("服务器不允许密钥登录时选这个：密码加密保存在本应用里，每次连接时使用。") + "</div>" +
      '<button class="btn block" type="button" data-use-password="1" style="margin-top:8px">' + T("输入密码…") + "</button></div></div>";
    box.querySelector("[data-retry]").addEventListener("click", function () { deviceFlow(id, box); });
    box.querySelector("[data-use-password]").addEventListener("click", function () { passwordStep(id, box, ""); });
    box.querySelector("[data-install]").addEventListener("submit", function (e) {
      e.preventDefault();
      var pw = this.pw.value, b = this.querySelector("button");
      if (!pw) { this.pw.focus(); return; }
      b.disabled = true; b.textContent = T("正在安装…");
      var form = this;
      api(path + "/install-key", { method: "POST", hub: true, body: { password: pw } }).then(function () {
        form.pw.value = "";
        toast(T("公钥已安装"));
        deviceFlow(id, box);
      }).catch(function (err) {
        toast(err.message, true);
        b.disabled = false; b.textContent = T("安装公钥并连接");
      });
    });
  }

  // passwordStep asks for the server's login password, keeps it (sealed, on
  // this client) and connects again.
  function passwordStep(id, box, message) {
    var path = "/api/devices/" + encodeURIComponent(id);
    box.innerHTML = '<div class="flow-step">' +
      (message ? '<div class="error-block">' + esc(TR(message)) + "</div>" : "") +
      '<form data-password="1"><label class="field"><span>' + T("服务器登录密码") + "</span>" +
      '<input class="input" type="password" name="pw" autocomplete="new-password" autocapitalize="off" spellcheck="false"></label>' +
      '<button class="btn primary block" type="submit" style="margin-top:8px">' + T("保存并连接") + "</button></form>" +
      '<div class="small muted" style="margin-top:6px">' + T("密码加密保存在本应用里，用于以后每次连接。") + "</div></div>";
    var form = box.querySelector("[data-password]");
    setTimeout(function () { form.pw.focus(); }, 30);
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var pw = form.pw.value, b = form.querySelector("button");
      if (!pw) { form.pw.focus(); return; }
      b.disabled = true; b.textContent = T("正在连接…");
      api(path, { method: "PATCH", hub: true, body: { password: pw } }).then(function () {
        form.pw.value = "";
        deviceFlow(id, box);
      }).catch(function (err) {
        toast(err.message, true);
        b.disabled = false; b.textContent = T("保存并连接");
      });
    });
  }

  function deviceSheet(id) {
    var n = S.nodes.filter(function (x) { return x.id === id; })[0] || { id: id, name: id };
    var body = openSheet(T(n.name),
      '<div class="stack"><div class="group">' +
      '<div class="g-row"><span class="g-label">' + T("地址") + '</span><span class="g-value mono">' + esc(n.target || n.url || "") + "</span></div>" +
      '<div class="g-row"><span class="g-label">' + T("状态") + '</span><span class="g-value">' + esc(n.online ? T("在线 %s", n.version || "") : (TR(n.error) || T("离线"))) + "</span></div>" +
      '<button type="button" class="g-row link" data-dev-key="1" hidden><span class="g-label">' + T("认证密钥") + '</span><span class="g-value"></span></button>' +
      '<button type="button" class="g-row link" data-dev-pw="1" hidden><span class="g-label">' + T("登录密码") + '</span><span class="g-value"></span></button></div>' +
      '<div id="dev-flow"></div>' +
      '<button class="btn danger block" type="button" data-del-device="1">' + T("删除设备") + "</button>" +
      '<p class="small muted">' + T("删除只移除本客户端里的记录，不会卸载服务器上的节点。") + "</p></div>");
    deviceFlow(id, $("#dev-flow"));
    Promise.all([api("/api/devices", { hub: true }), api("/api/keys", { hub: true })]).then(function (res) {
      var dev = (res[0].devices || []).filter(function (x) { return x.id === id; })[0];
      var keys = res[1], row = body.querySelector("[data-dev-key]");
      if (!dev || dev.transport !== "ssh" || !row) return;
      var cur = keyValueOf(dev);
      row.hidden = false;
      row.querySelector(".g-value").textContent = keyLabel(keys, cur) || T("未设置");
      var pwRow = body.querySelector("[data-dev-pw]");
      pwRow.hidden = false;
      pwRow.querySelector(".g-value").textContent = dev.password_auth ? T("已保存") : T("未使用");
      pwRow.addEventListener("click", function () {
        var choice = dev.password_auth
          ? pickList(T("登录密码"), [{ value: "change", label: T("更换密码…") }].concat(cur
            ? [{ value: "off", label: T("不再使用密码"), sub: T("只用密钥登录，并删除保存的密码") }] : []))
          : Promise.resolve("change");
        choice.then(function (v) {
          if (v === "off") {
            return api("/api/devices/" + encodeURIComponent(id), { method: "PATCH", hub: true, body: { password_auth: false } })
              .then(function () { toast(T("已删除保存的密码")); deviceSheet(id); });
          }
          if (v !== "change") return;
          return askText(dev.password_auth ? T("更换登录密码") : T("使用密码登录"), { label: T("服务器登录密码"), type: "password", ok: T("保存"),
            note: T("密码加密保存在本应用里，每次连接时使用；有密钥时先用密钥。") }).then(function (pw) {
            if (!pw) return;
            return api("/api/devices/" + encodeURIComponent(id), { method: "PATCH", hub: true, body: { password: pw } })
              .then(function () { toast(T("已保存密码")); deviceSheet(id); });
          });
        }).catch(function (e) { toast(e.message, true); });
      });
      row.addEventListener("click", function () {
        pickList(T("认证密钥"), keyChoices(keys, cur, true)).then(function (v) {
          if (v === "new") return generateKeyFlow().then(function (k) { return k && "id:" + k.id; });
          var f = (keys.detected || []).filter(function (x) { return "path:" + x.path === v; })[0];
          if (f && f.encrypted) return importKeyFlow(f).then(function (k) { return k && "id:" + k.id; });
          return v;
        }).then(function (v) {
          if (!v || v === cur) return;
          return api("/api/devices/" + encodeURIComponent(id), { method: "PATCH", hub: true, body: keyFields(v) })
            .then(function () { toast(T("已更换密钥")); deviceSheet(id); });
        }).catch(function (e) { toast(e.message, true); });
      });
    }).catch(function () {});
    body.querySelector("[data-del-device]").addEventListener("click", function () {
      confirmSheet(T("删除设备「%s」？", T(n.name)), "<p>" + T("之后可以重新添加；服务器上的节点和会话不受影响。") + "</p>", T("删除"), true).then(function (ok) {
        if (!ok) return;
        api("/api/devices/" + encodeURIComponent(id), { method: "DELETE", body: {}, hub: true }).then(function () {
          if (S.node === id) switchNode("local");
          loadNodes().then(function () { if (S.route === "settings") renderSettings(); });
          toast(T("已删除"));
        }).catch(function (e) { toast(e.message, true); });
      });
    });
  }

  // ------------------------------------------------------- keys and transfer

  function keysSheet() {
    var body = openSheet(T("密钥"), '<div class="empty"><span class="spinner"></span></div>');
    api("/api/keys", { hub: true }).then(function (d) {
      var mine = (d.keys || []).map(function (k) {
        return '<button type="button" class="g-row link" data-key="' + esc(k.id) + '"><span class="g-label">' + esc(k.name) +
          '<span class="g-sub">' + esc(k.type.replace("ssh-", "") + " · " + shortFp(k.fingerprint)) +
          (k.used_by.length ? " · " + esc(T("用于 %s", k.used_by.join(T("、")))) : "") + "</span></span></button>";
      }).join("");
      var files = (d.detected || []).filter(function (k) { return !k.imported; }).map(function (k, i) {
        return '<div class="g-row"><span class="g-label">' + esc(k.name) + '<span class="g-sub">' + esc(shortFp(k.fingerprint)) +
          (k.encrypted ? " · " + T("有密码短语") : "") + (k.used_by.length ? " · " + esc(T("用于 %s", k.used_by.join(T("、")))) : "") + "</span></span>" +
          '<button type="button" class="btn sm" data-import-key="' + i + '">' + T("导入") + "</button></div>";
      }).join("");
      var detected = (d.detected || []).filter(function (k) { return !k.imported; });
      body.innerHTML = '<div class="stack">' +
        (mine ? '<div class="group">' + mine + "</div>" : '<p class="muted small">' + T("还没有密钥。") + "</p>") +
        '<button type="button" class="btn block" data-gen-key="1">' + T("生成新密钥") + "</button>" +
        (files ? '<div><div class="section-title">' + T("在 ~/.ssh 中发现") + '</div><div class="group">' + files + "</div>" +
          '<div class="g-note">' + T("导入后私钥保存在本应用的数据库里，可以随设备一起导出到其它客户端；原文件不受影响。") + "</div></div>" : "") +
        "</div>";
      body.addEventListener("click", function (e) {
        var k = e.target.closest("[data-key]");
        if (k) { keyDetailSheet((d.keys || []).filter(function (x) { return x.id === k.getAttribute("data-key"); })[0]); return; }
        if (e.target.closest("[data-gen-key]")) { generateKeyFlow().then(function (nk) { if (nk) keysSheet(); }); return; }
        var imp = e.target.closest("[data-import-key]");
        if (imp) importKeyFlow(detected[+imp.getAttribute("data-import-key")]).then(function (nk) { if (nk) keysSheet(); });
      });
    }).catch(function (e) { body.innerHTML = '<div class="error-block">' + esc(e.message) + "</div>"; });
  }

  function keyDetailSheet(k) {
    if (!k) return;
    var body = openSheet(k.name, '<div class="stack">' + pubBox(k.public, false) +
      '<div class="group"><div class="g-row"><span class="g-label fixed">' + T("类型") + '</span><span class="g-value">' + esc(k.type) + "</span></div>" +
      '<div class="g-row"><span class="g-label fixed">' + T("指纹") + '</span><span class="g-value mono">' + esc(k.fingerprint) + "</span></div>" +
      '<div class="g-row"><span class="g-label fixed">' + T("用于") + '</span><span class="g-value">' + esc(k.used_by.join(T("、")) || "—") + "</span></div></div>" +
      '<div class="row" style="gap:8px"><button type="button" class="btn grow" data-rename="1">' + T("重命名") + "</button>" +
      '<button type="button" class="btn danger grow" data-delete="1">' + T("删除") + "</button></div>" +
      '<button type="button" class="btn ghost block" data-back="1">' + T("返回密钥列表") + "</button></div>");
    body.addEventListener("click", function (e) {
      if (e.target.closest("[data-back]")) { keysSheet(); return; }
      if (e.target.closest("[data-rename]")) {
        askText(T("重命名密钥"), { label: T("名称"), value: k.name, ok: T("保存") }).then(function (name) {
          if (name === null || !name.trim() || name.trim() === k.name) return;
          return api("/api/keys/" + encodeURIComponent(k.id), { method: "PATCH", hub: true, body: { name: name.trim() } })
            .then(function () { toast(T("已重命名")); keysSheet(); });
        }).catch(function (err) { toast(err.message, true); });
        return;
      }
      if (e.target.closest("[data-delete]")) {
        confirmSheet(T("删除密钥「%s」？", k.name), "<p>" + T("私钥会从本应用中删除且无法恢复。已经加到服务器上的公钥不受影响，需要时请自行从 <code>authorized_keys</code> 中移除。") + "</p>", T("删除"), true)
          .then(function (ok) {
            if (!ok) { keyDetailSheet(k); return; }
            return api("/api/keys/" + encodeURIComponent(k.id), { method: "DELETE", hub: true, body: {} })
              .then(function () { toast(T("已删除")); keysSheet(); });
          }).catch(function (err) { toast(err.message, true); keysSheet(); });
      }
    });
  }

  function exportSheet() {
    var body = openSheet(T("导出设备"), '<div class="empty"><span class="spinner"></span></div>');
    api("/api/devices", { hub: true }).then(function (d) {
      var devs = d.devices || [];
      if (!devs.length) { body.innerHTML = '<p class="muted">' + T("还没有可以导出的设备。") + "</p>"; return; }
      body.innerHTML = '<div class="stack"><p class="small muted" style="margin:0">' + T("把选中的服务器连同它们的密钥、登录密码、主机指纹和节点令牌加密导出，在另一台设备（例如手机）上导入即可直接使用。") + "</p>" +
        '<div class="group">' + devs.map(function (n) {
          return '<label class="g-row check-row"><input type="checkbox" value="' + esc(n.id) + '" checked>' +
            '<span class="g-label">' + esc(T(n.name)) + '<span class="g-sub">' + esc(n.ssh_user ? n.ssh_user + "@" + n.ssh_host : n.url || "") + "</span></span></label>";
        }).join("") + "</div>" +
        '<button type="button" class="btn primary block" data-do-export="qr">' + T("显示二维码") + "</button>" +
        '<button type="button" class="btn block" data-do-export="file">' + T("导出为文件") + "</button>" +
        '<p class="small muted" style="margin:0">' + T("二维码给手机扫描用：先在手机上点 导入设备 → 扫描二维码，手机会显示一个配对码。") + "</p></div>";
      body.addEventListener("click", function (e) {
        var b = e.target.closest("[data-do-export]");
        if (!b) return;
        var ids = $$("input[type=checkbox]:checked", body).map(function (c) { return c.value; });
        if (!ids.length) { toast(T("请至少选择一台设备"), true); return; }
        if (b.getAttribute("data-do-export") === "qr") { exportQRPairing(body, ids); return; }
        $$("[data-do-export]", body).forEach(function (x) { x.disabled = true; });
        api("/api/vault/export", { method: "POST", hub: true, body: { devices: ids } }).then(function (r) {
          exportResult(body, r);
        }).catch(function (err) {
          toast(err.message, true);
          $$("[data-do-export]", body).forEach(function (x) { x.disabled = false; });
        });
      });
    }).catch(function (e) { body.innerHTML = '<div class="error-block">' + esc(e.message) + "</div>"; });
  }

  function exportResult(body, r) {
    body.innerHTML = '<div class="stack">' +
      '<div class="code-card"><div class="small muted">' + T("解密码") + '</div><div class="code-big mono">' + esc(r.code) + "</div>" +
      '<button type="button" class="btn sm" data-copy-text="' + esc(r.code) + '">' + T("复制解密码") + "</button></div>" +
      '<div class="notice">' + T("解密码只显示这一次。请把它和文件<b>分开</b>传递（例如文件用 AirDrop 或数据线，解密码手动输入）。") + "</div>" +
      warningsBlock(r.warnings) +
      '<button type="button" class="btn primary block" data-save="1">' + T("保存文件") + "</button>" +
      '<p class="small muted" style="margin:0">' + esc(r.filename) + "</p></div>";
    body.querySelector("[data-save]").addEventListener("click", function () { saveTextFile(r.filename, r.bundle); });
  }

  function warningsBlock(w) {
    return w && w.length ? '<div class="error-block">' + w.map(function (x) { return esc(TR(x)); }).join("\n") + "</div>" : "";
  }

  function importedToast(r) {
    var n = r.added + r.updated;
    toast(r.keys ? T("已导入 %d 台设备、%d 把密钥", n, r.keys) : T("已导入 %d 台设备", n));
  }

  // exportQRPairing asks for the pairing code the phone shows, then shows the
  // bundle sealed with it as a QR code. The code comes from the phone and is
  // never displayed here: a photo of this screen opens nothing.
  function exportQRPairing(body, ids) {
    body.innerHTML = '<form class="stack" data-pair="1">' +
      '<p class="small muted" style="margin:0">' + T("在手机上打开 Rymesh → 设置 → 导入设备 → 扫描二维码，把手机上显示的配对码输入到这里。") + "</p>" +
      '<label class="field"><span>' + T("手机上的配对码") + '</span><input name="code" class="mono" placeholder="XXXX-XXXX-XXXX-XXXX" ' +
      'autocomplete="off" autocapitalize="characters" spellcheck="false"></label>' +
      '<button type="submit" class="btn primary block">' + T("生成二维码") + "</button></form>";
    var form = body.querySelector("form");
    setTimeout(function () { form.code.focus(); }, 30);
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var code = form.code.value.toUpperCase().replace(/[^23456789A-HJ-NP-Z]/g, "");
      if (code.length !== 16) { toast(T("配对码应为 16 位"), true); form.code.focus(); return; }
      var b = form.querySelector("button"); b.disabled = true;
      api("/api/vault/export", { method: "POST", hub: true, body: { devices: ids, qr: true, code: code } }).then(function (r) {
        form.code.value = "";
        exportQR(body, r);
      }).catch(function (err) { toast(err.message, true); b.disabled = false; });
    });
  }

  // exportQR shows the QR code for two minutes.
  function exportQR(body, r) {
    if (!r.qr_svg) {
      body.innerHTML = '<div class="stack"><div class="error-block">' + esc(TR(r.qr_error) || T("无法生成二维码")) + "</div>" +
        '<button type="button" class="btn block" data-done="1">' + T("关闭") + "</button></div>";
      body.querySelector("[data-done]").addEventListener("click", closeSheet);
      return;
    }
    body.innerHTML = '<div class="stack">' +
      '<div class="qr-card"><div class="qr-img">' + r.qr_svg + "</div>" +
      '<div class="small muted" data-qr-left></div></div>' +
      '<div class="notice">' + T("用刚才显示配对码的那台手机扫描。二维码只能用那个配对码解开，这个屏幕上不显示任何密码。") + "</div>" +
      warningsBlock(r.warnings) +
      '<button type="button" class="btn block" data-done="1">' + T("完成") + "</button></div>";
    var left = 120, label = body.querySelector("[data-qr-left]");
    function tick() {
      if (!body.isConnected) { clearInterval(timer); return; }
      if (left <= 0) {
        clearInterval(timer);
        body.querySelector(".qr-img").innerHTML = '<div class="qr-gone">' + T("二维码已隐藏") + "</div>";
        label.textContent = T("需要时请重新导出");
        return;
      }
      label.textContent = T("%d 秒后隐藏", left);
      left--;
    }
    var timer = setInterval(tick, 1000);
    tick();
    body.querySelector("[data-done]").addEventListener("click", function () { clearInterval(timer); closeSheet(); });
  }

  // pairingCode is 16 random symbols from the export code alphabet (80 bits),
  // made on this device; 256 is a multiple of 32, so no symbol is favoured.
  function pairingCode() {
    var abc = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ", b = new Uint8Array(16), out = "";
    crypto.getRandomValues(b);
    for (var i = 0; i < 16; i++) out += (i && i % 4 === 0 ? "-" : "") + abc[b[i] % 32];
    return out;
  }

  // importByQR: the phone shows a fresh pairing code, the user types it on the
  // exporting device, and the QR code that device then shows is opened with
  // the code this page still holds. The code lives only in this page.
  function importByQR(body) {
    var code = pairingCode();
    body.innerHTML = '<div class="stack">' +
      '<p class="small muted" style="margin:0">' + T("在电脑的 Rymesh 里：设置 → 导出设备 → 显示二维码，输入下面的配对码。") + "</p>" +
      '<button type="button" class="code-card copyable" data-copy-text="' + esc(code) + '" aria-label="' + esc(T("复制配对码")) + '">' +
      '<div class="small muted">' + T("配对码 · 点按复制") + '</div><div class="code-big mono">' + esc(code) + "</div></button>" +
      '<button type="button" class="btn primary block" data-scan="1">' + T("电脑上已显示二维码，开始扫描") + "</button>" +
      '<button type="button" class="btn ghost block" data-back="1">' + T("返回") + "</button></div>";
    body.querySelector("[data-back]").addEventListener("click", function () { code = ""; importSheet(); });
    body.querySelector("[data-scan]").addEventListener("click", function () {
      var b = this;
      window.acScanResult = function (text) {
        window.acScanResult = null;
        if (!text) return;
        if (text.indexOf("RYMESH1:") !== 0) { toast(T("这不是 Rymesh 导出的二维码"), true); return; }
        b.disabled = true; b.textContent = T("正在导入…");
        api("/api/vault/import", { method: "POST", hub: true, body: { qr: text, code: code } }).then(function (r) {
          code = "";
          closeSheet();
          importedToast(r);
          loadNodes().then(function () { if (S.hubOnly && S.node === "local") boot(); else if (S.route === "settings") renderSettings(); });
        }).catch(function (err) {
          toast(/解密码/.test(err.raw || "") ? T("二维码不是用这个配对码生成的，请在电脑上重新输入配对码") : err.message, true);
          b.disabled = false; b.textContent = T("电脑上已显示二维码，开始扫描");
        });
      };
      window.RymeshNative.scanQR();
    });
  }

  function importSheet() {
    var canScan = !!(window.RymeshNative && window.RymeshNative.scanQR);
    var body = openSheet(T("导入设备"),
      '<form class="stack" data-import-form="1">' +
      (canScan ? '<button type="button" class="btn primary block" data-scan-start="1">' + T("扫描二维码") + "</button>" : "") +
      '<p class="small muted" style="margin:0">' + (canScan
        ? T("或选择从另一台设备导出的 <code>.rymesh</code> 文件，并输入导出时显示的解密码。")
        : T("选择从另一台设备导出的 <code>.rymesh</code> 文件，并输入导出时显示的解密码。")) + "</p>" +
      '<label class="file-pick"><input type="file" name="file" accept=".rymesh,.acbundle,application/json,application/octet-stream,*/*">' +
      '<span class="file-pick-label">' + T("选择文件…") + "</span></label>" +
      '<details class="fold"><summary>' + T("或粘贴文件内容") + '</summary><textarea name="text" class="input mono" rows="4" style="margin-top:8px"></textarea></details>' +
      '<label class="field"><span>' + T("解密码") + '</span><input name="code" class="mono" placeholder="XXXX-XXXX-XXXX-XXXX" autocomplete="off" autocapitalize="characters" spellcheck="false"></label>' +
      '<button type="submit" class="btn primary block">' + T("导入") + "</button></form>");
    var form = body.querySelector("form"), bundle = "";
    if (canScan) body.querySelector("[data-scan-start]").addEventListener("click", function () { importByQR(body); });
    form.file.addEventListener("change", function () {
      var f = this.files && this.files[0];
      if (!f) return;
      var rd = new FileReader();
      rd.onload = function () { bundle = String(rd.result || ""); form.querySelector(".file-pick-label").textContent = f.name; };
      rd.readAsText(f);
    });
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var data = bundle || form.text.value.trim();
      if (!data) { toast(T("请选择文件"), true); return; }
      if (!form.code.value.trim()) { form.code.focus(); return; }
      var b = form.querySelector("[type=submit]"); b.disabled = true; b.textContent = T("正在导入…");
      api("/api/vault/import", { method: "POST", hub: true, body: { bundle: data, code: form.code.value } }).then(function (r) {
        closeSheet();
        importedToast(r);
        loadNodes().then(function () { if (S.hubOnly && S.node === "local") boot(); else if (S.route === "settings") renderSettings(); });
      }).catch(function (err) { toast(err.message, true); b.disabled = false; b.textContent = T("导入"); });
    });
  }

  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-add-device]")) { addDeviceSheet(); return; }
    if (e.target.closest("[data-keys]")) { keysSheet(); return; }
    if (e.target.closest("[data-export]")) { exportSheet(); return; }
    if (e.target.closest("[data-import]")) { importSheet(); return; }
    var d = e.target.closest("[data-device]");
    if (d && S.route === "settings") deviceSheet(d.getAttribute("data-device"));
  });

  // The Android app's back gesture: close the top layer, or go home; false
  // lets the app leave.
  window.acBack = function () {
    if (overlays.length) { overlays[overlays.length - 1].close(); return true; }
    if (nodeMenu() && !nodeMenu().hidden) { closeNodeMenu(); return true; }
    if (closeAgentFilterMenu()) return true;
    if (!$("#sheet").hidden) { closeSheet(); return true; }
    if (!$("#term").hidden) { closeTerminalView(); return true; }
    if (!$("#files").hidden) { closeFilesView(); return true; }
    if (!$("#sidebar").hidden && !wide.matches) { closeSidebar(); return true; }
    if (location.hash && location.hash !== "#/" && location.hash !== "#") { location.hash = "#/"; return true; }
    return false;
  };

  // The device switcher drops down in place under the device name.
  function nodeMenu() { return $("#node-menu"); }
  function openNodeMenu() {
    var menu = nodeMenu();
    if (!menu) {
      menu = document.createElement("div");
      menu.id = "node-menu";
      menu.className = "menu node-menu";
      menu.setAttribute("role", "listbox");
      $(".sidebar-head").appendChild(menu);
    }
    function draw() {
      menu.innerHTML = '<div class="menu-section">' + T("切换设备") + "</div>" + nodeRows() +
        '<div class="menu-sep"></div>' +
        '<button type="button" class="menu-item" data-add-device="1"><span class="menu-text"><span class="menu-label">' + T("添加服务器…") + "</span></span></button>" +
        '<button type="button" class="menu-item" data-manage-devices="1"><span class="menu-text"><span class="menu-label">' + T("管理设备") + "</span></span></button>";
    }
    draw();
    menu.hidden = false;
    var btn = $("#node-switch");
    if (btn) btn.setAttribute("aria-expanded", "true");
    loadNodes().then(function () { if (!menu.hidden) draw(); }); // live status
  }
  function closeNodeMenu() {
    var menu = nodeMenu();
    if (menu) menu.hidden = true;
    var btn = $("#node-switch");
    if (btn) btn.setAttribute("aria-expanded", "false");
  }

  document.addEventListener("click", function (e) {
    if (e.target.closest("#node-switch")) {
      var m = nodeMenu();
      if (m && !m.hidden) closeNodeMenu(); else openNodeMenu();
      return;
    }
    var inMenu = e.target.closest("#node-menu");
    if (!inMenu) closeNodeMenu();
    if (inMenu && e.target.closest("[data-manage-devices]")) {
      closeNodeMenu();
      location.hash = "#/settings";
      return;
    }
    if (inMenu && e.target.closest("[data-add-device]")) closeNodeMenu();
    var n = e.target.closest("[data-node]");
    if (n && !n.disabled) { closeNodeMenu(); switchNode(n.getAttribute("data-node")); }
  });

  // Approvals on the other devices, so a waiting agent anywhere is noticed.
  function pollRemotePerms() {
    var others = S.nodes.filter(function (n) { return n.online && n.id !== S.node; });
    if (!others.length) { S.remotePerms = []; return Promise.resolve(); }
    return Promise.all(others.map(function (n) {
      return api("/api/permissions", { node: n.id }).then(function (d) {
        return (d.permissions || []).map(function (p) { p._node = n.id; return p; });
      }).catch(function () { return []; });
    })).then(function (lists) {
      S.remotePerms = [].concat.apply([], lists);
      renderApprovalNotifications();
    });
  }

  function isActiveTask(t) {
    return t.status === "running" || t.status === "pending" || t.status === "waiting_input";
  }

  // A running agent outranks queued work when describing the session state.
  function activeOnSession() {
    var running = null, pending = null;
    S.sessionActive.forEach(function (t) {
      if (t.status === "waiting_input" || t.status === "running") {
        if (!running) running = t;
      } else if (t.status === "pending" && !pending) {
        pending = t;
      }
    });
    return running || pending;
  }

  function startPolling() {
    if (S.pollTimer) clearInterval(S.pollTimer);
    S.pollTimer = setInterval(function () {
      if (document.hidden || !S.booted) return;
      refreshActive().catch(function () {});
      if (multiNode()) pollRemotePerms();
    }, 6000);
  }

  function refreshActive() {
    return Promise.all([
      api("/api/tasks?status=active").then(function (d) { S.activeTasks = d.tasks || []; }).catch(function () {}),
      api("/api/permissions").then(function (d) { S.perms = d.permissions || []; }).catch(function () {}),
      // Keep the sidebar's "recent conversation time" and titles fresh without
      // a full bootstrap.
      api("/api/sessions").then(function (d) { S.sessions = d.sessions || []; }).catch(function () {})
    ]).then(function () {
      if (S.route === "chat" && S.session) {
        S.sessionActive = S.activeTasks.filter(function (t) { return t.session_id === S.session.id; });
        S.sessionPerms = S.perms.filter(function (p) { return p.session_id === S.session.id; });
        updateChatChrome();
        renderPermissionBanner();
      }
      updateSessionBadges();
      renderApprovalNotifications();
      if (S.sidebarOpen) renderSessionList();
    });
  }

  // -------------------------------------------------------------------- theme

  function applyTheme(t) {
    S.theme = t;
    LS.set("theme", t);
    if (t === "auto") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = t;
    updateThemeColor();
  }
  function updateThemeColor() {
    var meta = $('meta[name="theme-color"]');
    if (!meta) return;
    var light = S.theme === "light" || (S.theme === "auto" && matchMedia("(prefers-color-scheme: light)").matches);
    meta.setAttribute("content", light ? "#fcfcfc" : "#0b0b0a");
    if (window.RymeshNative) window.RymeshNative.setTheme(!light);
  }
  matchMedia("(prefers-color-scheme: light)").addEventListener?.("change", updateThemeColor);

  // ------------------------------------------------------------------- router

  window.addEventListener("hashchange", function () { route(false); });

  function route(initial) {
    if (!S.booted) return;
    var raw = (location.hash || "").replace(/^#\/?/, "");
    if (S.es && S.route === "chat" && !/^session\//.test(raw)) closeStream();
    closeSidebar();
    closeTerminalView();
    closeFilesView();
    if (wide.matches) renderSessionList();
    if (/^session\//.test(raw)) {
      var id = decodeURIComponent(raw.slice(8));
      openSession(id, initial);
      return;
    }
    if (raw === "settings") { S.route = "settings"; renderSettings(); return; }
    if (raw === "sessions") { location.replace("#/"); return; }
    renderHome();
  }

  function go(hash) {
    if (location.hash === hash) route(false);
    else location.hash = hash;
  }

  // state, when given, is a task status shown as a dot beside the title.
  function setTop(title, sub, state) {
    $("#page-title").innerHTML = '<span class="t">' + esc(title) + "</span>" +
      (state ? '<span class="state-dot ' + esc(state) + '" title="' + esc(statusLabel(state)) + '"></span>' : "");
    $("#page-sub").textContent = sub || "";
  }

  // The cwd lives in the top bar subtitle, which is the only place it is
  // shown. Tapping it opens the directory editor for a new conversation and
  // just reports the fixed path for an existing one.
  function bindCwdEntry() {
    var sub = $("#page-sub");
    if (!sub || sub.__cwdBound) return;
    sub.__cwdBound = true;
    sub.classList.add("cwd-entry");
    sub.addEventListener("click", function () {
      if (S.route !== "home" && S.route !== "chat") return;
      if (S.session) toast(T("会话目录已固定：%s", S.session.cwd));
      else cwdSheet();
    });
  }

  function setConn(state) {
    S.conn = state;
    var d = $("#conn-dot");
    d.className = "conn-dot" + (state === "on" ? " on" : state === "off" ? " off" : state === "wait" ? " wait" : "");
    d.title = state === "on" ? T("实时连接正常") : state === "wait" ? T("正在连接…") : T("连接已断开，正在重连");
  }

  // ----------------------------------------------------------------- sessions

  function ensureDraftAgent() {
    var found = false;
    for (var i = 0; i < S.agents.length; i++) {
      if (S.agents[i].name === S.draftAgent && S.agents[i].available) { found = true; break; }
    }
    if (!found) {
      for (var j = 0; j < S.agents.length; j++) {
        if (S.agents[j].available) { S.draftAgent = S.agents[j].name; break; }
      }
      if (S.draftAgent) LS.set("draftAgent", S.draftAgent);
    }
  }

  function renderAgentSelect() {
    ensureDraftAgent();
    var sel = $("#agent-select");
    if (!sel) return;
    if (!S.agents.length) {
      sel.innerHTML = "<option>" + T("无 Agent") + "</option>";
      sel.disabled = true;
      $("#agent-label").textContent = T("无 Agent");
      $("#agent-btn").disabled = true;
      return;
    }
    var current = S.session ? S.session.agent : S.draftAgent;
    sel.innerHTML = S.agents.map(function (a) {
      return '<option value="' + esc(a.name) + '"' + (a.available ? "" : " disabled") + ">" +
        esc(a.label || a.name) + (a.available ? "" : T("（不可用）")) + "</option>";
    }).join("");
    var exists = S.agents.some(function (a) { return a.name === current; });
    sel.value = exists ? current : (S.agents[0] || {}).name || "";
    sel.disabled = false;
    $("#agent-btn").disabled = false;
    $("#agent-label").textContent = (agentMeta(sel.value).label || sel.value);
    // The permission options are agent-specific, so rebuild them whenever the
    // effective agent changes (draft switch, opening a conversation, new chat).
    if (S._profileAgent !== current) {
      S._profileAgent = current;
      renderProfileSelect();
    }
  }

  function setAgentPicker(visible) {
    $("#agent-wrap").hidden = !visible;
    if (!visible) closeAgentMenu();
  }

  // The three composer buttons (agent, permissions, model) open the same kind
  // of menu above the composer; one is open at a time.
  function closeAgentMenu() {
    $("#agent-menu").hidden = true;
    $("#agent-btn").setAttribute("aria-expanded", "false");
  }
  function openAgentMenu() {
    closeProfileMenu();
    closeModelMenu();
    var sel = $("#agent-select");
    $("#agent-menu").innerHTML = S.agents.map(function (a) {
      var on = a.name === sel.value;
      return '<button type="button" role="option" class="menu-item' + (on ? " on" : "") + '" aria-selected="' + on +
        '" data-agent="' + esc(a.name) + '"' + (a.available ? "" : " disabled") + ">" +
        '<span class="menu-text"><span class="menu-label">' + esc(a.label || a.name) + "</span>" +
        '<span class="menu-detail">' + esc(a.available ? (shortVersion(a.version) ? T("版本 %s", shortVersion(a.version)) : "") : T("不可用")) + "</span></span>" +
        '<svg class="ico sm menu-check" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg></button>';
    }).join("");
    $("#agent-menu").hidden = false;
    $("#agent-btn").setAttribute("aria-expanded", "true");
  }

  function profilesForAgent(name) {
    return (S.profiles && S.profiles[name]) || [];
  }

  function profileMeta(agent, id) {
    var list = profilesForAgent(agent);
    for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i];
    return null;
  }

  // The profile list follows the Agent that will actually run the next message:
  // the fixed agent inside a conversation, otherwise the draft agent.
  function currentAgentName() {
    if (S.route === "chat" && S.session) return S.session.agent;
    return S.draftAgent;
  }

  function renderProfileSelect() {
    var sel = $("#profile-select");
    if (!sel) return;
    var agent = currentAgentName();
    S._profileAgent = agent;
    var list = profilesForAgent(agent);
    if (!list.length) { sel.disabled = true; syncProfileBtn(); return; }
    var key = "profile." + agent;
    var saved = LS.get(key, "");
    if (!profileMeta(agent, saved)) saved = list[0].id;
    sel.innerHTML = list.map(function (p) {
      return '<option value="' + esc(p.id) + '"' + (p.dangerous ? ' class="danger-opt"' : "") + ">" +
        esc(p.label) + "</option>";
    }).join("");
    sel.value = saved;
    sel.disabled = false;
    sel.title = (profileMeta(agent, saved) || {}).detail || "";
    syncProfileBtn();
  }

  // The permission picker is a pill that opens a small menu; the hidden
  // <select> stays the single source of truth for the chosen profile.
  function syncProfileBtn() {
    var sel = $("#profile-select"), btn = $("#profile-btn");
    if (!sel || !btn) return;
    var meta = profileMeta(currentAgentName(), sel.value);
    $("#profile-label").textContent = meta ? meta.label : T("权限");
    btn.classList.toggle("danger", !!(meta && meta.dangerous));
    btn.disabled = sel.disabled;
  }

  function closeProfileMenu() {
    var m = $("#profile-menu"), b = $("#profile-btn");
    if (m) m.hidden = true;
    if (b) b.setAttribute("aria-expanded", "false");
  }

  function openProfileMenu() {
    closeModelMenu();
    closeAgentMenu();
    var sel = $("#profile-select"), menu = $("#profile-menu");
    var agent = currentAgentName();
    menu.innerHTML = profilesForAgent(agent).map(function (p) {
      var on = p.id === sel.value;
      return '<button type="button" role="option" class="menu-item' + (p.dangerous ? " danger" : "") +
        (on ? " on" : "") + '" aria-selected="' + on + '" data-profile="' + esc(p.id) + '">' +
        '<span class="menu-text"><span class="menu-label">' + esc(p.label) + "</span>" +
        (p.detail ? '<span class="menu-detail">' + esc(p.detail) + "</span>" : "") + "</span>" +
        '<svg class="ico sm menu-check" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg></button>';
    }).join("");
    menu.hidden = false;
    $("#profile-btn").setAttribute("aria-expanded", "true");
  }

  $("#profile-btn").addEventListener("click", function (e) {
    e.stopPropagation();
    if ($("#profile-menu").hidden) openProfileMenu(); else closeProfileMenu();
  });
  $("#profile-menu").addEventListener("click", function (e) {
    var it = e.target.closest("[data-profile]");
    if (!it) return;
    var sel = $("#profile-select");
    sel.value = it.getAttribute("data-profile");
    closeProfileMenu();
    sel.dispatchEvent(new Event("change"));
  });
  document.addEventListener("click", function (e) {
    if (!e.target.closest(".menu-wrap")) { closeProfileMenu(); closeAgentMenu(); }
  });

  function selectedProfile() {
    var sel = $("#profile-select");
    var agent = currentAgentName();
    var v = sel ? sel.value : "";
    if (!profileMeta(agent, v)) v = defaultProfile(agent);
    return v;
  }

  function defaultProfile(agent) {
    var list = profilesForAgent(agent);
    for (var i = 0; i < list.length; i++) if (!list[i].dangerous) return list[i].id;
    return list.length ? list[0].id : "read-only";
  }

  // The name, large and faint, fills the space above the greeting.
  var HOME_HERO = '<div class="home-hero" aria-hidden="true"><span>Rymesh</span></div>';

  // A conversation's title as shown: titles the server made up ("Codex 会话
  // 1a2b3c4d", "新会话 · Codex") are translated, the user's own stay.
  function titleOf(s, fallback) {
    return s && s.title ? trTitle(s.title) : fallback || T("未命名会话");
  }
  function trTitle(t) {
    return /^(Codex|Claude|Hermes) 会话 |^新会话 · | · 群聊（/.test(t || "") ? TR(t) : t;
  }

  // Where a new conversation will run, with the directory as a button.
  function homeWhere() {
    var node = multiNode() ? esc(nodeLabel(S.node)) : "";
    if (!S.draftCwd) return node ? T("新对话将在 %s 上运行", node) : T("新对话将在默认目录中运行");
    var chip = '<button type="button" class="cwd-chip" data-home-cwd="1" title="' + esc(T("更改工作目录：%s", S.draftCwd)) + '">' +
      '<svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 7.5a2 2 0 0 1 2-2h4l2 2h7a2 2 0 0 1 2 2v7.5a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z"/></svg>' +
      '<span class="t">' + esc(shortPath(S.draftCwd, 32)) + "</span>" +
      '<svg class="ico sm chev" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 10 5 5 5-5"/></svg>' +
      "</button>";
    return node ? T("新对话将在 %s 的 %s 中运行", node, chip) : T("新对话将在 %s 中运行", chip);
  }

  function renderHome() {
    S.route = "home";
    closeStream();
    S.session = null;
    S.sessionTasks = [];
    S.sessionPerms = [];
    S.sessionActive = [];
    S.eventSeqs = Object.create(null);
    S.toolNodes = Object.create(null);
    S.lastSeq = 0;
    S.lastEv = null;
    S.oldestSeq = 0;
    S.hasOlder = false;
    ensureDraftAgent();
    if (!S.draftCwd) S.draftCwd = S.config.default_cwd || "";
    document.body.classList.add("has-composer");
    $("#composer-wrap").hidden = false;
    $("#conn-dot").hidden = true;
    // A new conversation has nothing running: drop the previous chat's
    // "任务运行中" hint and stop button.
    $("#composer-hint").textContent = "";
    $("#stop-btn").hidden = true;
    $("#term-btn").hidden = true;
    $("#files-btn").hidden = true;
    $("#new-chat-btn").hidden = true; // already a new conversation
    $("#settings-btn").classList.remove("active");
    setBackButton(false);
    setAgentPicker(true);
    if (noDevice()) {
      setTop("Rymesh", "");
      $("#composer-wrap").hidden = true;
      document.body.classList.remove("has-composer");
      $("#main").innerHTML = '<div class="home-empty onboarding">' + HOME_HERO +
        '<div class="home-title">' + T("欢迎") + "</div>" +
        '<div class="home-sub">' + T("添加一台服务器开始使用：通过 SSH 连接，并在服务器上自动部署节点。服务器只需开放 SSH。") + "</div>" +
        '<button class="btn primary block" type="button" data-add-device="1" style="margin-top:24px">' + T("添加服务器") + "</button>" +
        '<button class="btn block" type="button" data-import="1" style="margin-top:10px">' + T("从其它设备导入") + "</button></div>";
      return;
    }
    var a = agentMeta(S.draftAgent);
    // The directory is shown (and changed) in the greeting, so the top bar
    // stays a plain title here.
    setTop(T("新对话"), "");
    $("#prompt").placeholder = S.agents.length && S.draftAgent ? T("给 %s 发消息…", a.label || a.name) : T("给 Agent 发消息…");
    var recent = S.sessions.slice(0, 3).map(function (s) {
      return '<button type="button" class="recent-card" data-recent="' + esc(s.id) + '">' +
        agentIcon(s.agent) +
        '<span class="recent-main"><span class="recent-title" style="display:block">' + esc(titleOf(s)) + "</span>" +
        '<span class="recent-sub" style="display:block">' + esc(dirName(s.cwd)) + "</span></span>" +
        '<span class="recent-time">' + esc(rel(s.last_message_at || s.updated_at)) + "</span></button>";
    }).join("");
    $("#main").innerHTML =
      '<div class="home-empty">' + HOME_HERO +
      '<div class="home-title">' + esc(greeting()) + "</div>" +
      '<div class="home-sub">' + homeWhere() + "</div>" +
      (S.link.state !== "ok" ? '<div class="home-recent">' + connCard() + "</div>" :
      recent ? '<div class="home-recent"><div class="home-recent-label">' + T("继续最近的对话") + "</div>" + recent + "</div>" : "") +
      "</div>";
    renderAgentSelect();
    updateComposerMeta();
    renderProfileSelect();
    syncModelBtn();
    renderApprovalNotifications();
  }

  function renderSidebar() {
    var box = $("#session-list");
    if (!box) return;
    var search = $("#session-search");
    if (search && search.value !== S.sessionQuery) search.value = S.sessionQuery;
    renderSessionList();
  }

  // On wide screens the list is docked and always visible.
  var wide = matchMedia("(min-width: 900px)");
  function dockSidebar() {
    if (!wide.matches || !S.booted) return;
    S.sidebarOpen = true;
    $("#sidebar").hidden = false;
    syncScrollLock();
    renderSidebar();
  }
  wide.addEventListener && wide.addEventListener("change", function () {
    if (!S.booted) return;
    if (wide.matches) dockSidebar();
    else closeSidebar(true);
  });

  function openSidebar() {
    S.sidebarOpen = true;
    $("#sidebar").hidden = false;
    syncScrollLock();
    renderSidebar();
    setTimeout(function () {
      var q = $("#session-search");
      if (q && matchMedia("(pointer: fine)").matches) q.focus();
    }, 40);
  }

  function closeSidebar(force) {
    if (wide.matches && S.booted && force !== true) return;
    if (!S.sidebarOpen && $("#sidebar").hidden) return;
    S.sidebarOpen = false;
    closeAgentFilterMenu();
    $("#sidebar").hidden = true;
    syncScrollLock();
  }

  function sessionById(id) {
    for (var i = 0; i < S.sessions.length; i++) if (S.sessions[i].id === id) return S.sessions[i];
    return null;
  }

  function filteredSessions() {
    var q = S.sessionQuery.trim().toLowerCase();
    return S.sessions.filter(function (s) {
      if (S.agentFilter !== "all" && s.agent !== S.agentFilter) return false;
      if (!q) return true;
      return (s.title || "").toLowerCase().indexOf(q) >= 0 ||
        (s.cwd || "").toLowerCase().indexOf(q) >= 0 ||
        (s.native_id || "").toLowerCase().indexOf(q) >= 0;
    });
  }

  function activeTaskFor(sessionId) {
    for (var i = 0; i < S.activeTasks.length; i++) {
      if (S.activeTasks[i].session_id === sessionId) return S.activeTasks[i];
    }
    return null;
  }

  // The agent filter sits behind one button beside the search, so the sidebar
  // keeps its size however many agents there are. It is offered only when
  // conversations of two or more agents exist; a saved filter whose agent has
  // none left falls back to "all". The active choice shows as a tag under the
  // search, with × to clear it.
  function agentCounts() {
    var counts = {};
    S.sessions.forEach(function (s) { counts[s.agent] = (counts[s.agent] || 0) + 1; });
    return counts;
  }

  function renderAgentFilter() {
    var btn = $("#agent-filter-btn"), tag = $("#agent-filter");
    if (!btn || !tag) return;
    var counts = agentCounts();
    var several = Object.keys(counts).length >= 2;
    if (S.agentFilter !== "all" && (!counts[S.agentFilter] || !several)) S.agentFilter = "all";
    var on = S.agentFilter !== "all";
    btn.hidden = !several;
    btn.classList.toggle("active", on);
    if (!several) closeAgentFilterMenu();
    tag.hidden = !on;
    tag.innerHTML = on
      ? '<button type="button" class="chip active filter-tag" data-agent-filter="all" title="' + esc(T("显示全部")) + '">' +
        esc(agentMeta(S.agentFilter).label || S.agentFilter) +
        '<svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6 6 18"/></svg></button>'
      : "";
    if (!$("#agent-filter-menu").hidden) drawAgentFilterMenu();
  }

  function drawAgentFilterMenu() {
    var counts = agentCounts();
    var names = Object.keys(counts).sort(function (a, b) { return counts[b] - counts[a] || (a < b ? -1 : 1); });
    var row = function (id, icon, label, n) {
      var on = S.agentFilter === id;
      return '<button type="button" role="option" class="menu-item' + (on ? " on" : "") + '" aria-selected="' + on +
        '" data-agent-filter="' + esc(id) + '">' + icon +
        '<span class="menu-text"><span class="menu-label">' + esc(label) + "</span></span>" +
        '<span class="filter-count">' + n + "</span>" +
        '<svg class="ico sm menu-check" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg></button>';
    };
    $("#agent-filter-menu").innerHTML =
      row("all", '<svg class="ico agent-ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h16"/></svg>',
        T("全部"), S.sessions.length) +
      '<div class="menu-sep"></div>' +
      names.map(function (n) { return row(n, agentIcon(n), agentMeta(n).label || n, counts[n]); }).join("");
  }

  function openAgentFilterMenu() {
    drawAgentFilterMenu();
    $("#agent-filter-menu").hidden = false;
    $("#agent-filter-btn").setAttribute("aria-expanded", "true");
  }

  function closeAgentFilterMenu() {
    var menu = $("#agent-filter-menu");
    if (!menu || menu.hidden) return false;
    menu.hidden = true;
    $("#agent-filter-btn").setAttribute("aria-expanded", "false");
    return true;
  }

  document.addEventListener("click", function (e) {
    if (e.target.closest("#agent-filter-btn")) {
      if (!closeAgentFilterMenu()) openAgentFilterMenu();
      return;
    }
    var b = e.target.closest("[data-agent-filter]");
    if (!e.target.closest("#agent-filter-menu")) closeAgentFilterMenu();
    if (!b) return;
    closeAgentFilterMenu();
    S.agentFilter = b.getAttribute("data-agent-filter");
    LS.set("agentFilter", S.agentFilter);
    renderSessionList();
  });

  function renderSessionList() {
    var box = $("#session-list");
    if (!box) return;
    renderAgentFilter();
    var list = filteredSessions();
    if (!list.length) {
      box.innerHTML = '<div class="empty" style="padding:32px 12px">' +
        ((S.sessionQuery || S.agentFilter !== "all") ? T("没有匹配的对话。") : T("还没有对话。点击上方新建，或输入消息开始。")) + "</div>";
      return;
    }
    var lastGroup = "";
    box.innerHTML = list.map(function (s) {
      var act = activeTaskFor(s.id);
      var active = S.session && S.session.id === s.id && S.route === "chat";
      var when = s.last_message_at || s.updated_at;
      var g = dayGroup(when);
      var head = g !== lastGroup ? '<div class="side-group">' + esc(g) + "</div>" : "";
      lastGroup = g;
      return head + '<div class="side-row">' +
        '<button class="side-item' + (active ? " active" : "") + '" type="button" ' +
        'data-side-session="' + esc(s.id) + '" title="' + esc((s.cwd || "") + (when ? " · " + rel(when) : "")) + '">' +
        agentIcon(s.agent) +
        '<span class="side-title" data-side-title="' + esc(s.id) + '">' + esc(titleOf(s)) + "</span>" +
        '<span class="badge status' + (act ? " " + esc(act.status) : "") + '" data-side-badge="' + esc(s.id) + '"' +
        (act ? "" : " hidden") + ">" + (act ? esc(statusLabel(act.status, act.queue_ahead)) : "") + "</span>" +
        "</button>" +
        '<button class="side-more" type="button" data-side-menu="' + esc(s.id) +
        '" aria-label="' + esc(T("对话操作")) + '" aria-haspopup="dialog">⋯</button>' +
        "</div>";
    }).join("");
  }

  // Sessions arrive newest first, so a heading is emitted whenever the bucket changes.
  function dayGroup(iso) {
    var d = new Date(iso || 0);
    if (isNaN(d) || !iso) return T("更早");
    var today = new Date(); today.setHours(0, 0, 0, 0);
    var days = Math.floor((today - new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 86400000);
    if (days <= 0) return T("今天");
    if (days === 1) return T("昨天");
    if (days < 7) return T("近 7 天");
    if (days < 30) return T("近 30 天");
    return T("更早");
  }

  function updateSessionBadges() {
    $$("[data-side-badge]").forEach(function (el) {
      var id = el.getAttribute("data-side-badge");
      var act = activeTaskFor(id);
      if (act) {
        el.className = "badge status " + act.status;
        el.hidden = false;
        el.textContent = statusLabel(act.status, act.queue_ahead);
      } else {
        el.className = "badge status";
        el.hidden = true;
        el.textContent = "";
      }
    });
  }

  function pendingForNotice() {
    var list = (S.perms || []).filter(function (p) { return p.status === "pending"; });
    if (S.route === "chat" && S.session) {
      list = list.filter(function (p) { return p.session_id !== S.session.id; });
    }
    return list.concat((S.remotePerms || []).filter(function (p) {
      return p.status === "pending" && p._node !== S.node;
    }));
  }

  function showApprovalStack() {
    var box = $("#approval-stack");
    if (!box || !box.innerHTML) return;
    box.hidden = false;
    S.approvalStackOpen = true;
    clearTimeout(S.approvalTimer);
    S.approvalTimer = setTimeout(hideApprovalStack, 7000);
  }

  function hideApprovalStack() {
    var box = $("#approval-stack");
    if (!box) return;
    box.hidden = true;
    S.approvalStackOpen = false;
    clearTimeout(S.approvalTimer);
  }

  function renderApprovalNotifications() {
    var box = $("#approval-stack");
    var btn = $("#approval-btn");
    var cnt = $("#approval-count");
    if (!box || !btn) return;
    var pend = pendingForNotice();
    if (!pend.length) {
      btn.hidden = true;
      if (cnt) cnt.textContent = "";
      box.innerHTML = "";
      hideApprovalStack();
      S.approvalKeys = "";
      return;
    }
    btn.hidden = false;
    if (cnt) cnt.textContent = pend.length > 9 ? "9+" : String(pend.length);
    // One card per session. Approvals from the same session used to render as
    // identical cards that each repeated the total count, which looked like a
    // broken stack; the per-session count is what distinguishes them.
    var groups = [];
    var bySid = {};
    pend.forEach(function (p) {
      var k = (p._node || "") + "/" + (p.session_id || "");
      var g = bySid[k];
      if (!g) {
        g = bySid[k] = { sid: p.session_id || "", node: p._node || "", first: p, items: [] };
        groups.push(g);
      }
      g.items.push(p);
    });
    var hiddenItems = 0;
    groups.slice(3).forEach(function (g) { hiddenItems += g.items.length; });
    box.innerHTML = groups.slice(0, 3).map(function (g) {
      var s = g.node ? null : sessionById(g.sid);
      var title = s && s.title ? trTitle(s.title) : (trTitle(g.first.session_title) || T("会话 %s", String(g.sid || "").slice(0, 8)));
      if (g.node) title = nodeLabel(g.node) + " · " + title;
      var tools = [];
      g.items.forEach(function (p) {
        var label = p.kind === "question" ? T("Agent 在提问") : p.kind === "plan" ? T("计划待批准") : toolLabel(p.tool_name);
        if (tools.indexOf(label) < 0) tools.push(label);
      });
      var toolText = tools.length > 2 ? T("%s 等", tools.slice(0, 2).join(T("、"))) : tools.join(T("、"));
      return '<button type="button" class="approval-toast" data-approval-node="' + esc(g.node) +
        '" data-approval-sid="' + esc(g.sid) +
        '" data-approval-pid="' + esc(g.first.id) + '">' +
        '<div class="approval-top"><span class="approval-dot"></span>' +
        '<span class="approval-title">' + (g.first.kind === "question" ? T("Agent 在提问") : g.first.kind === "plan" ? T("计划待批准") : T("需要审批")) + "</span>" +
        (g.items.length > 1 ? '<span class="approval-count">' + T("%d 项", g.items.length) + "</span>" : "") +
        "</div>" +
        '<div class="approval-session">' + esc(truncate(title, 48)) + "</div>" +
        '<div class="approval-tool">' + esc(T("%s · 点击前往处理", toolText)) + "</div></button>";
    }).join("") +
      (hiddenItems ? '<div class="approval-more">' + T("另有 %d 项待审批", hiddenItems) + "</div>" : "");
    var keys = pend.map(function (p) { return p.id; }).sort().join(",");
    // While the open conversation has its own card waiting, popping other
    // conversations' requests over it hides the thing to act on; the badge
    // still counts them.
    var ownPending = S.route === "chat" && (S.sessionPerms || []).some(function (p) { return p.status === "pending"; });
    if (keys !== S.approvalKeys) {
      S.approvalKeys = keys;
      if (ownPending) box.hidden = true;
      else showApprovalStack();
    } else if (!S.approvalStackOpen) {
      box.hidden = true;
    }
  }

  function focusPermissionCard(pid) {
    var el = document.querySelector('[data-permission-id="' + cssAttr(pid) + '"]');
    if (!el) return false;
    el.scrollIntoView({ behavior: "smooth", block: "center" });
    el.classList.add("flash");
    setTimeout(function () { el.classList.remove("flash"); }, 1800);
    S.focusPermission = "";
    return true;
  }

  // Touch: long-press a row for its menu (the ⋯ button only shows on hover or
  // on the open conversation). Desktop: right-click does the same.
  (function () {
    var list = $("#session-list"), timer = null, fired = false, x0 = 0, y0 = 0;
    function cancel() { clearTimeout(timer); timer = null; }
    list.addEventListener("touchstart", function (e) {
      var row = e.target.closest("[data-side-session]");
      if (!row) return;
      fired = false; x0 = e.touches[0].clientX; y0 = e.touches[0].clientY;
      timer = setTimeout(function () {
        var s = sessionById(row.getAttribute("data-side-session"));
        if (!s) return;
        fired = true;
        if (navigator.vibrate) navigator.vibrate(10);
        sessionMenu(s);
      }, 480);
    }, { passive: true });
    list.addEventListener("touchmove", function (e) {
      var t = e.touches[0];
      if (Math.abs(t.clientX - x0) > 8 || Math.abs(t.clientY - y0) > 8) cancel();
    }, { passive: true });
    // The click that follows a long press would land on the menu that just
    // opened under the finger (and could pick an item): cancel it at the
    // source. The capture handler stays for browsers that click anyway.
    list.addEventListener("touchend", function (e) {
      cancel();
      if (fired) e.preventDefault();
    });
    list.addEventListener("touchcancel", cancel);
    list.addEventListener("click", function (e) {
      if (fired) { fired = false; e.stopImmediatePropagation(); e.preventDefault(); }
    }, true);
    list.addEventListener("contextmenu", function (e) {
      var row = e.target.closest("[data-side-session]");
      if (!row) return;
      e.preventDefault();
      var s = sessionById(row.getAttribute("data-side-session"));
      if (s) sessionMenu(s);
    });
  })();

  $("#main").addEventListener("click", function (e) {
    var r = e.target.closest("[data-recent]");
    if (r) { go("#/session/" + encodeURIComponent(r.getAttribute("data-recent"))); return; }
    if (e.target.closest("[data-home-cwd]")) cwdSheet();
  });

  $("#session-list").addEventListener("click", function (e) {
    var m = e.target.closest("[data-side-menu]");
    if (m) {
      var s = sessionById(m.getAttribute("data-side-menu"));
      if (s) sessionMenu(s);
      return;
    }
    var b = e.target.closest("[data-side-session]");
    if (!b) return;
    closeSidebar();
    go("#/session/" + encodeURIComponent(b.getAttribute("data-side-session")));
  });

  function sessionMenu(s) {
    var body = openSheet(T("对话操作"),
      '<div class="stack">' +
      '<button class="btn block" data-a="rename">' + T("重命名对话") + "</button>" +
      '<button class="btn block danger" data-a="delete">' + T("删除对话") + "</button>" +
      '<p class="small muted">' + T("删除对话会同步移除控制台记录，并将该 Agent 的原生会话文件移动到服务端回收目录。此操作不可从界面撤销。") + "</p>" +
      "</div>");
    body.addEventListener("click", function (e) {
      var b = e.target.closest("[data-a]");
      if (!b) return;
      var a = b.getAttribute("data-a");
      closeSheet();
      if (a === "rename") renameSession(s);
      else if (a === "delete") deleteNativeFlow(s);
    });
  }

  function renameSession(s) {
    var body = openSheet(T("重命名会话"),
      '<label class="field"><span>' + T("标题") + '</span><input id="rename-input" value="' + esc(s.title || "") + '"></label>' +
      '<button id="rename-ok" class="btn primary block">' + T("保存") + "</button>");
    $("#rename-ok").addEventListener("click", function () {
      var v = $("#rename-input").value.trim();
      api("/api/sessions/" + encodeURIComponent(s.id), { method: "PATCH", body: { title: v } })
        .then(function () {
          s.title = v || "";
          if (S.session && S.session.id === s.id) S.session.title = s.title;
          closeSheet(); renderSidebar(); updateChatChrome(); toast(T("已重命名"));
        }).catch(function (e) { toast(e.message, true); });
    });
  }

  function deleteNativeFlow(s) {
    confirmSheet(T("删除对话？"),
      "<p>" + T("将删除「%s」，并同步处理磁盘文件：", esc(truncate(titleOf(s), 60))) + "</p>" +
      '<ul style="padding-left:20px;color:var(--text-dim);font-size:13px">' +
      "<li>" + T("该 Agent 的原生会话文件会移动到服务端回收目录") + "</li>" +
      "<li>" + T("控制台中的会话、消息与审批记录会被移除") + "</li></ul>" +
      '<p class="small muted">' + T("无法从界面恢复，不影响其他对话。") + "</p>",
      T("删除"), true).then(function (ok) {
      if (!ok) return;
      api("/api/sessions/" + encodeURIComponent(s.id) + "?confirm=" + encodeURIComponent(s.id), { method: "DELETE" })
        .then(function (d) {
          S.sessions = S.sessions.filter(function (x) { return x.id !== s.id; });
          if (S.session && S.session.id === s.id) {
            S.session = null;
            go("#/");
          } else renderSidebar();
          toast(d.native_trashed ? T("对话已删除，原生文件已移入回收目录") : T("对话已删除（未找到原生文件）"));
        }).catch(function (e) { toast(e.message, true); });
    });
  }

  // ------------------------------------------------------------- new chat / cwd

  function newChat(agent) {
    if (agent) {
      S.draftAgent = agent;
      LS.set("draftAgent", agent);
    }
    closeSidebar();
    go("#/");
  }

  function updateComposerMeta() {
    bindCwdEntry();
  }

  function cwdSheet() {
    if (S.session) { toast(T("会话目录已固定：%s", S.session.cwd)); return; }
    if (noDevice()) return;
    openFolderPicker(S.draftCwd || S.config.default_cwd || "");
  }

  // cwdInputSheet sets the directory by typing it.
  function cwdInputSheet() {
    var cur = S.draftCwd || S.config.default_cwd || "";
    var body = openSheet(T("工作目录"),
      '<div class="stack">' +
      '<label class="field"><span>' + T("新对话使用的服务器绝对路径") + "</span>" +
      '<input id="cwd-input" class="mono" value="' + esc(cur) + '" spellcheck="false" autocomplete="off"></label>' +
      '<div class="row"><button id="cwd-check" class="btn sm" type="button">' + T("检查目录") + "</button>" +
      '<button id="cwd-reset" class="btn sm ghost" type="button">' + T("恢复默认") + "</button>" +
      '<span id="cwd-result" class="small muted"></span></div>' +
      '<button id="cwd-save" class="btn primary block">' + T("保存") + "</button>" +
      '<p class="small muted">' + T("目录必须在允许根目录内。不同对话可以使用不同目录；同一目录并发运行时会给出提示。") + "</p>" +
      "</div>");
    $("#cwd-check").addEventListener("click", function () {
      var out = $("#cwd-result"); out.textContent = T("检查中…");
      api("/api/cwd/check", { method: "POST", body: { path: $("#cwd-input").value.trim() } })
        .then(function (d) {
          if (!d.ok) { out.textContent = TR(d.error); out.style.color = "var(--red)"; return; }
          out.style.color = "var(--text-dim)";
          out.textContent = "✓ " + d.cwd + (d.git_root ? " · " + T("git 仓库") : "") +
            (d.active_tasks ? " · " + T("%d 个任务运行中", d.active_tasks) : "");
          if (d.warning) toast(d.warning, true);
        }).catch(function (e) { out.textContent = e.message; out.style.color = "var(--red)"; });
    });
    $("#cwd-reset").addEventListener("click", function () {
      $("#cwd-input").value = S.config.default_cwd || "";
      $("#cwd-result").textContent = "";
    });
    $("#cwd-save").addEventListener("click", function () {
      var v = $("#cwd-input").value.trim();
      if (!v) { toast(T("目录不能为空"), true); return; }
      S.draftCwd = v;
      LS.set("draftCwd", v);
      closeSheet();
      if (S.route === "home") renderHome();
      else updateComposerMeta();
      toast(T("工作目录已保存"));
    });
  }

  // --------------------------------------------------------------------- chat

  function openSession(id, initial) {
    if (S.session && S.session.id === id && S.route === "chat" && !initial) {
      updateChatChrome();
      return;
    }
    S.route = "chat";
    var token = ++S.openToken;
    closeStream();
    S.session = null;
    S.eventSeqs = Object.create(null);
    S.toolNodes = Object.create(null);
    S.permDecisions = Object.create(null);
    S.lastSeq = 0;
    S.lastEv = null;
    S.oldestSeq = 0;
    S.hasOlder = false;
    S.sessionTasks = [];
    S.sessionActive = [];
    S.sessionPerms = [];
    $("#composer-wrap").hidden = false;
    $("#conn-dot").hidden = false;
    $("#new-chat-btn").hidden = false;
    $("#settings-btn").classList.remove("active");
    setBackButton(false);
    document.body.classList.add("has-composer");

    var main = $("#main");
    main.innerHTML =
      '<div class="chat-wrap"><div id="perm-banner"></div>' +
      '<div id="events" class="events"><div class="events-inner" id="events-inner">' +
      '<div class="empty"><span class="spinner"></span> ' + T("正在加载会话…") + "</div>" +
      '</div><div id="activity" class="activity" aria-live="polite" hidden></div></div>' +
      '<div id="readonly-note" class="readonly-note" hidden></div></div>';
    setTop(T("会话"), "");
    setConn("wait");

    Promise.all([
      api("/api/sessions/" + encodeURIComponent(id)),
      api("/api/sessions/" + encodeURIComponent(id) + "/events?tail=1&turns=" +
        S.pageTurns + "&limit=" + S.pageEvents)
    ]).then(function (res) {
      if (token !== S.openToken || S.route !== "chat") return; // user navigated away
      var sessData = res[0], evData = res[1];
      S.session = sessData.session;
      S.sessionTasks = sessData.tasks || [];
      S.sessionPerms = sessData.permissions || [];
      S.sessionSettings = sessData.settings || {};
      syncModelBtn();
      S.sessionActive = S.sessionTasks.filter(isActiveTask);
      if (!S.sessions.some(function (s) { return s.id === id; })) S.sessions.unshift(S.session);
      renderProfileSelect();

      var evs = evData.events || [];
      S.loadSeq = evData.last_seq || 0;
      S.loadPending = Object.create(null);
      (sessData.permissions || []).forEach(function (p) { if (p.status === "pending") S.loadPending[p.id] = true; });
      $("#events-inner").innerHTML = "";
      S.oldestSeq = evs.length ? evs[0].seq : 0;
      // The server decides whether older events exist; an event-count guess
      // would be wrong now that a page is cut by turns.
      S.hasOlder = !!evData.has_more;
      if (S.hasOlder) addOlderButton();
      appendEvents(evs, { quiet: true });
      // Do not jump past the events we actually rendered: the detail request
      // can finish after the events page and include newer rows, and starting
      // the stream at that seq would skip them forever. The stream's hello
      // event reports the true latest seq and triggers catchUp().
      S.lastSeq = Math.max(S.lastSeq, evData.last_seq || 0);
      ensurePermissionCards();
      updateChatChrome();
      renderPermissionBanner();
      renderApprovalNotifications();
      scrollBottom(true);
      connectStream();
      setTop(titleOf(S.session, T("会话")), sessionSub(), sessionState());
      if (S.focusPermission) {
        var pid = S.focusPermission;
        setTimeout(function () { focusPermissionCard(pid); }, 90);
      }
      if (S.session.title) {
        // If the session had only a default title, the server may have updated it.
        S.sessions.forEach(function (s) { if (s.id === id) s.title = S.session.title; });
      }
    }).catch(function (e) {
      // A failure for a session the user already left must not redirect or
      // overwrite the page they are looking at now.
      if (token !== S.openToken || S.route !== "chat") return;
      if (e.status === 404) {
        toast(T("会话不存在或已删除"), true);
        go("#/");
        return;
      }
      toast(e.message, true);
      $("#events-inner").innerHTML = '<div class="empty">' + esc(T("加载失败：%s", e.message)) + "</div>";
    });
  }

  // The subtitle is just the directory; the agent is named in the composer
  // placeholder and the state is the dot beside the title (sessionState).
  function sessionSub() {
    if (!S.session) return "";
    return multiNode() ? nodeLabel(S.node) + " · " + S.session.cwd : S.session.cwd;
  }

  function sessionState() {
    var act = activeOnSession();
    return act ? act.status : "";
  }

  function updateChatChrome() {
    if (!S.session) return;
    S.draftAgent = S.session.agent;
    LS.set("draftAgent", S.draftAgent);
    renderAgentSelect();
    setAgentPicker(false);
    updateComposerMeta();
    $("#prompt").placeholder = T("给 %s 发消息…", agentMeta(S.session.agent).label || S.session.agent);
    setTop(titleOf(S.session, T("会话")), sessionSub(), sessionState());
    // A conversation the agent cannot continue (a Hermes bot's WeChat chat)
    // is shown without a composer, with the reason in its place.
    var ro = S.session.read_only || "";
    var note = $("#readonly-note");
    if (note) { note.hidden = !ro; note.textContent = TR(ro); }
    $("#composer-wrap").hidden = !!ro;
    document.body.classList.toggle("has-composer", !ro);
    // Only show a hint while something is actually happening. The idle state
    // needs no caption: the top bar already shows agent, state and directory.
    var act = activeOnSession();
    var hint = "";
    if (act) {
      if (act.status === "waiting_input") hint = T("Agent 正在等待审批，请处理上方请求");
      else if (act.status === "running") hint = T("任务运行中 · 新消息将排队执行");
      else if (act.status === "pending") {
        // Only say "queued" when something really is ahead. A task that is
        // first in line is about to start, not waiting behind itself.
        hint = act.queue_ahead > 0
          ? T("排队中 · 前面还有 %d 个任务，结束后自动执行", act.queue_ahead)
          : T("正在启动…");
      }
    }
    $("#composer-hint").textContent = hint;
    renderProfileSelect();
    var permCount = S.sessionPerms.length;
    var sendBtn = $("#send-btn");
    if (sendBtn) sendBtn.disabled = S.sending;
    // A task that is running or queued can be stopped from the composer.
    var stopBtn = $("#stop-btn");
    if (stopBtn) { stopBtn.hidden = !act; stopBtn.disabled = false; }
    if (permCount) setConn(S.conn === "on" ? "on" : S.conn);
    renderApprovalNotifications();
    renderActivity();
    $("#term-btn").hidden = noDevice();
    $("#files-btn").hidden = noDevice();
  }

  function renderPermissionBanner() {
    var box = $("#perm-banner");
    if (!box) return;
    var pend = (S.sessionPerms || []).filter(function (p) { return p.status === "pending"; });
    if (!pend.length) { box.innerHTML = ""; return; }
    box.innerHTML = '<div class="permission-banner">' + T("%d 项等待你处理", pend.length) +
      ' <button class="btn sm" type="button" data-scroll-perm="1">' + T("查看") + "</button></div>";
  }
  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-scroll-perm]")) {
      var card = $("#events-inner [data-permission-id]");
      if (card) { card.scrollIntoView({ behavior: "smooth", block: "center" }); }
    }
  });

  // Scrolling up to the start of what is loaded fetches the page before it,
  // a little ahead of reaching the top. The button stays as the marker that
  // shows the load, and for a click when the automatic one failed.
  var OLDER_AHEAD = 600;
  var olderObserver = "IntersectionObserver" in window ? new IntersectionObserver(function (entries) {
    entries.forEach(function (en) { if (en.isIntersecting) loadOlder(); });
  }, { rootMargin: OLDER_AHEAD + "px 0px 0px 0px" }) : null;

  function addOlderButton() {
    var inner = $("#events-inner");
    if (!inner || $("#load-older")) return;
    var b = document.createElement("button");
    b.id = "load-older";
    b.className = "btn sm ghost load-older";
    b.type = "button";
    b.textContent = T("加载更早的消息");
    b.addEventListener("click", loadOlder);
    inner.prepend(b);
    if (olderObserver) olderObserver.observe(b);
  }

  // After a page lands the marker can still be in reach (a short page, or a
  // fast fling); the observer only reports changes, so check by hand.
  function olderStillInReach() {
    var b = $("#load-older");
    if (!b || !S.hasOlder) return;
    var r = b.getBoundingClientRect();
    if (r.height && r.bottom > -OLDER_AHEAD) loadOlder();
  }

  function loadOlder() {
    if (!S.session || !S.oldestSeq || S.loadingOlder) return;
    S.loadingOlder = true;
    var btn = $("#load-older");
    if (btn) { btn.disabled = true; btn.textContent = T("加载中…"); }
    var inner = $("#events-inner");
    var prevH = inner.scrollHeight;
    var sid = S.session.id;
    api("/api/sessions/" + encodeURIComponent(sid) + "/events?before=" + S.oldestSeq +
      "&turns=" + S.olderTurns + "&limit=" + S.olderEvents)
      .then(function (d) {
        // Navigating to another session while this page is in flight must not
        // insert this session's history into the new one.
        S.loadingOlder = false;
        if (!S.session || S.session.id !== sid || !inner.isConnected) { olderStillInReach(); return; }
        var evs = d.events || [];
        if (!evs.length) {
          S.hasOlder = false;
          if (btn) btn.remove();
          return;
        }
        S.oldestSeq = evs[0].seq;
        S.hasOlder = !!d.has_more;
        // Keep what the user was reading in place. groupTurns moves events into
        // collapsed <details>, so height deltas are unreliable; instead record
        // where each existing top-level node was and shift by how far the
        // topmost one still in view moved.
        // Only nodes that reach into the viewport count: the newly inserted page
        // lands above the current scroll position, so a node the user has
        // already scrolled past would measure the whole inserted block as
        // "movement" and jump the page to the new bottom. A node that merely
        // starts above the top edge is still what the user is reading, so it is
        // kept. The "load older" button itself is skipped: it sits before the
        // insertion point and therefore never moves, so using it as the anchor
        // would leave the transcript to jump down by the whole inserted page.
        // Keeping the first visible message fixed instead means the viewport
        // stays exactly where it was and the newly loaded messages wait above,
        // for the user to scroll up to.
        // A node that grouping moves into a closed <details> keeps a layout box
        // (content-visibility: hidden) whose top is the position it *would* have
        // when expanded. Measuring that reports the whole hidden body as growth,
        // so such nodes are skipped and the height delta below is used instead.
        var tops = [];
        Array.prototype.forEach.call(inner.children, function (c) {
          if (c.id === "load-older") return;
          var r = c.getBoundingClientRect();
          if (!r.height || r.bottom <= 0) return;
          if (c.closest("details:not([open])")) return;
          tops.push({ el: c, top: r.top });
        });
        var anchor = $("#load-older");
        var cursor = anchor;
        evs.forEach(function (ev) {
          if (S.eventSeqs[ev.seq]) return;
          S.eventSeqs[ev.seq] = true;
          var n = eventNode(ev);
          if (n) { cursor.insertAdjacentElement("afterend", n); cursor = n; }
        });
        var note = $("#trim-note");
        if (note) note.remove();
        if (btn) {
          btn.disabled = false; btn.textContent = T("加载更早的消息");
          if (!S.hasOlder) btn.remove();
        }
        groupTurns();
        var moved = null;
        for (var ti = 0; ti < tops.length; ti++) {
          var el = tops[ti].el;
          if (!el.isConnected) continue;
          if (el.closest("details:not([open])")) continue;
          var r = el.getBoundingClientRect();
          if (!r.height) continue;
          moved = r.top - tops[ti].top;
          break;
        }
        // Nothing measured (everything was above the viewport or got folded):
        // fall back to the height the transcript grew by, which is exactly how
        // far the content the user was looking at was pushed down.
        if (moved === null) {
          var grew = inner.scrollHeight - prevH;
          moved = grew > 0 ? grew : 0;
        }
        if (moved) getScroller().scrollTop += moved;
        requestAnimationFrame(olderStillInReach);
      }).catch(function (e) {
        // No automatic retry: a failing server would be asked in a loop. The
        // next scroll back up, or the button, tries again.
        S.loadingOlder = false;
        toast(e.message, true);
        if (btn) { btn.disabled = false; btn.textContent = T("加载更早的消息"); }
      });
  }

  function getScroller() {
    return document.scrollingElement || document.documentElement;
  }

  function nearBottom() {
    var sc = getScroller();
    return sc.scrollHeight - sc.scrollTop - window.innerHeight < 160;
  }
  function scrollBottom(instant) {
    setTimeout(function () {
      window.scrollTo({ top: document.body.scrollHeight, behavior: instant ? "auto" : "smooth" });
    }, 30);
  }

  function appendEvents(evs, opts) {
    opts = opts || {};
    var inner = $("#events-inner");
    if (!inner) return;
    var wasNear = nearBottom();
    var frag = document.createDocumentFragment();
    evs.forEach(function (ev) {
      if (S.eventSeqs[ev.seq]) return;
      S.eventSeqs[ev.seq] = true;
      if (ev.seq > S.lastSeq) { S.lastSeq = ev.seq; S.lastEv = ev; }
      var node = eventNode(ev);
      if (node) frag.appendChild(node);
    });
    inner.appendChild(frag);
    Object.keys(S.permDecisions).forEach(function (id) {
      var d = S.permDecisions[id];
      applyPermissionResolved(inner.querySelector('[data-permission-id="' + cssAttr(id) + '"]'), d.decision, d.info);
    });
    groupTurns();
    trimDOM(inner);
    renderActivity();
    if (!opts.quiet && wasNear) scrollBottom(false);
  }

  // The line under the newest event while the agent works: what it is doing
  // and for how long, so a quiet stretch (a long think, a slow command) does
  // not look like a hang. It ticks every second while shown.
  var activityTimer = null;
  function renderActivity() {
    var box = $("#activity");
    var act = box && S.session ? activeOnSession() : null;
    var text = act ? activityText(act) : "";
    if (!text) {
      if (box) box.hidden = true;
      clearInterval(activityTimer);
      activityTimer = null;
      return;
    }
    if (!box.firstChild) {
      box.innerHTML = '<span class="activity-dot"></span><span class="activity-text"></span><span class="activity-time"></span>';
    }
    box.classList.toggle("waiting", act.status === "waiting_input");
    box.children[1].textContent = text;
    var since = act.status === "pending" ? "" : dur(act.started_at);
    box.children[2].textContent = since;
    box.children[2].hidden = !since;
    if (box.hidden) {
      var wasNear = nearBottom();
      box.hidden = false;
      if (wasNear) scrollBottom(false);
    }
    if (!activityTimer) activityTimer = setInterval(renderActivity, 1000);
  }

  function activityText(act) {
    if (act.status === "pending") return act.queue_ahead > 0 ? T("排队中") : T("正在启动…");
    if (act.status === "waiting_input") return T("等待你的确认");
    var ev = S.lastEv;
    if (ev && ev.task_id === act.id && ev.kind === "tool") {
      var p = ev.payload || {};
      if (p.status === "running" || p.status === "in_progress") {
        var what = truncate(String(toolSummary(p) || "").split("\n")[0], 60);
        return T("正在执行 %s", toolLabel(p.name)) + (what ? " · " + what : "");
      }
    }
    return T("正在思考…");
  }

  // Between a user message and the final assistant answer, collapse thinking,
  // tool calls, tool results and other intermediate events into one disclosure.
  // A page can also begin in the middle of a turn (the first page is the last
  // N events and may contain no user message at all), so a run of intermediate
  // events with no user message in front of it is folded the same way; otherwise
  // the newest conversation shows its whole thinking/tool trace unfolded.
  function groupTurns() {
    var inner = $("#events-inner");
    if (!inner) return;
    var nodes = Array.prototype.slice.call(inner.children);
    var turn = [];
    function flush() {
      if (turn.length) groupTurn(turn);
      turn = [];
    }
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      if (!n.classList || (!n.classList.contains("ev") && !n.classList.contains("steps"))) {
        flush();
        continue;
      }
      if (n.classList.contains("ev") && n.getAttribute("data-kind") === "user") {
        flush();
        turn = [n];
        continue;
      }
      turn.push(n);
    }
    flush();
  }

  function groupTurn(turn) {
    var user = turn[0] && turn[0].getAttribute("data-kind") === "user" ? turn[0] : null;
    var anchor = turn[0];
    if (!anchor || !anchor.parentNode) return;
    var parent = anchor.parentNode;
    // Where the group goes when no user message opens the turn: the first node
    // is about to be moved into the group, so remember its position first.
    var before = anchor.previousSibling;
    var flat = [];
    var wasOpen = false;
    turn.forEach(function (n) {
      if (n.classList.contains("steps")) {
        wasOpen = wasOpen || n.open;
        var oldBody = n.querySelector(".steps-body");
        if (oldBody) {
          Array.prototype.slice.call(oldBody.children).forEach(function (c) {
            if (c.classList.contains("ev")) flat.push(c);
          });
        }
        n.remove();
      } else if (n.classList.contains("ev")) {
        flat.push(n);
      }
    });
    flat = flat.filter(function (n) { return n.getAttribute("data-kind") !== "user"; });

    var lastAssistant = -1;
    flat.forEach(function (n, i) {
      if (n.getAttribute("data-kind") === "assistant") lastAssistant = i;
    });
    var pre = [], fold = [], post = [], answer = null;
    flat.forEach(function (n, i) {
      if (i === lastAssistant) { answer = n; return; }
      if (lastAssistant >= 0 && i > lastAssistant) { post.push(n); return; }
      var kind = n.getAttribute("data-kind");
      // Approval cards and hard errors stay visible; they need user action or attention.
      if (kind === "permission" || kind === "error" || kind === "plan") pre.push(n);
      else fold.push(n);
    });

    var frag = document.createDocumentFragment();
    function place(n) {
      if (n.parentNode) n.remove();
      frag.appendChild(n);
    }
    pre.forEach(place);
    if (fold.length) {
      var d = document.createElement("details");
      d.className = "steps";
      d.open = wasOpen;
      var sum = document.createElement("summary");
      sum.textContent = T("执行过程 · %d 步", fold.length);
      var body = document.createElement("div");
      body.className = "steps-body";
      fold.forEach(function (n) {
        if (n.parentNode) n.remove();
        body.appendChild(n);
      });
      d.appendChild(sum);
      d.appendChild(body);
      bindSteps(d);
      if (d.open) openStepItems(d);
      frag.appendChild(d);
    }
    if (answer) place(answer);
    post.forEach(place);
    if (user) parent.insertBefore(frag, user.nextSibling);
    else if (before && before.parentNode === parent) parent.insertBefore(frag, before.nextSibling);
    else parent.insertBefore(frag, parent.firstChild);
  }

  function openStepItems(d) {
    Array.prototype.forEach.call(d.querySelectorAll("details.thinking"), function (x) { x.open = true; });
    Array.prototype.forEach.call(d.querySelectorAll(".tool"), function (x) { x.classList.add("open"); });
    fillLazy(d);
  }

  function bindSteps(d) {
    d.addEventListener("toggle", function () {
      if (d.open) openStepItems(d);
    });
  }

  function trimDOM(inner) {
    var nodes = inner.querySelectorAll(".ev");
    var max = 1400;
    if (nodes.length <= max) return;
    var removed = [];
    for (var i = 0; i < nodes.length - max; i++) {
      var seq = parseInt(nodes[i].getAttribute("data-seq"), 10);
      if (seq) removed.push(seq);
      nodes[i].remove();
    }
    Array.prototype.forEach.call(inner.querySelectorAll(".steps"), function (d) {
      var left = d.querySelectorAll(".steps-body > .ev").length;
      if (!left) { d.remove(); return; }
      var s = d.querySelector("summary");
      if (s) s.textContent = T("执行过程 · %d 步", left);
    });
    // The trim note promises that the removed range can be loaded again. Make
    // that true: move the older cursor up to the first node still on the page
    // and forget the removed seqs, otherwise loading older messages would fetch
    // past the gap and leave a hole in the transcript.
    var rest = inner.querySelectorAll(".ev");
    if (removed.length && rest.length) {
      var oldest = parseInt(rest[0].getAttribute("data-seq"), 10);
      if (oldest && oldest > S.oldestSeq) {
        S.oldestSeq = oldest;
        removed.forEach(function (q) { delete S.eventSeqs[q]; });
        S.hasOlder = true;
        addOlderButton();
      }
    }
    // Tool nodes of removed events would otherwise keep their DOM alive.
    Object.keys(S.toolNodes).forEach(function (k) {
      var nd = S.toolNodes[k];
      if (!nd || !nd.isConnected) delete S.toolNodes[k];
    });
    if (!$("#trim-note")) {
      var n = document.createElement("div");
      n.id = "trim-note";
      n.className = "notice";
      n.textContent = T("为保持流畅，较早的消息已从页面移除；继续向上滑动即可重新加载。");
      inner.prepend(n);
    }
  }

  function eventNode(ev) {
    var wrap = document.createElement("div");
    wrap.className = "ev";
    wrap.setAttribute("data-seq", ev.seq);
    wrap.setAttribute("data-kind", ev.kind);
    var p = ev.payload || {};
    switch (ev.kind) {
      case "user": {
        var pendNodes = document.querySelectorAll("[data-pending-text]");
        for (var pi = 0; pi < pendNodes.length; pi++) {
          if (pendNodes[pi].getAttribute("data-pending-text") === (p.text || "")) {
            pendNodes[pi].remove();
            break;
          }
        }
        wrap.innerHTML = '<div class="msg user" title="' + esc(clock(ev.ts)) +
          '"><div class="bubble">' + esc(p.text || "") + "</div></div>";
        break;
      }
      case "assistant":
        // A reply the console wrote itself (a slash command's answer) is
        // translated; the model's own words never are.
        wrap.innerHTML = '<div class="msg assistant"><div class="bubble">' + md(p.console ? TR(p.text || "") : p.text || "") + "</div></div>";
        break;
      case "thinking":
        wrap.innerHTML = '<details class="thinking"><summary>' + T("已思考") + '</summary><div class="thinking-body" data-lazy-text></div></details>';
        setLazy(wrap.querySelector("[data-lazy-text]"), p.text || "", "pre");
        break;
      case "tool":
        // Codex emits item.started / item.updated / item.completed for the same
        // item id. Updating the existing card keeps one card per tool call
        // instead of a duplicate per phase.
        if (p.id && updateToolNode(p)) return null;
        wrap.appendChild(toolNode(p));
        return wrap;
      case "tool_result": {
        if (mergeToolResult(p)) return null;
        wrap.innerHTML = '<details class="thinking tool-result"><summary>' + T("工具结果") + " · " +
          esc(summarize(p.output, 70)) + '</summary><div class="tool-result-body"><pre data-lazy-text></pre></div></details>';
        setLazy(wrap.querySelector("[data-lazy-text]"), p.output, "pre");
        break;
      }
      case "permission": {
        var existing = document.querySelector('[data-permission-id="' + cssAttr(p.permission_id) + '"]');
        var perm = permModel(p);
        perm.task_id = ev.task_id;
        perm.session_id = S.session ? S.session.id : "";
        // Older transcripts lack the result event. A card that already existed
        // when the session was opened and is not pending on the server has
        // been handled, even if the outcome is unknown.
        if (ev.seq <= S.loadSeq && !S.loadPending[perm.id] && !(perm.id in S.permDecisions)) {
          S.permDecisions[perm.id] = { decision: "", info: null };
        }
        if (!(perm.id in S.permDecisions)) {
          if (!S.sessionPerms.some(function (x) { return x.id === perm.id; })) S.sessionPerms.push(perm);
          if (!S.perms.some(function (x) { return x.id === perm.id; })) S.perms.push(perm);
        }
        renderPermissionBanner();
        renderApprovalNotifications();
        if (existing) return null;
        wrap.appendChild(permissionNode(perm));
        return wrap;
      }
      case "permission_result": {
        markPermissionResolved(p.permission_id, p.decision, p);
        return null;
      }
      case "plan": {
        // Codex plan mode ends the turn with a plan; running it is a new turn
        // outside plan mode, as with "Implement plan" in the Codex CLI.
        wrap.innerHTML = '<div class="permission-card kind-plan codex-plan"><div class="permission-title"><span class="dot"></span>' + T("计划") + "</div>" +
          '<div class="plan-body bubble">' + md(p.text || "") + "</div>" +
          '<div class="perm-opts"><button class="perm-opt primary" type="button" data-run-plan="1"><span class="perm-opt-label">' + T("按此计划执行") + "</span>" +
          '<span class="perm-opt-detail">' + T("切换到「自动」模式并开始执行") + "</span></button></div></div>";
        break;
      }
      case "status": {
        var st = p.status || "";
        wrap.innerHTML = '<div class="status-line">' + T("任务结束") + " · " + esc(statusLabel(st)) +
          (p.reason && p.reason !== "exited" ? " · " + esc(reasonLabel(p.reason)) : "") + "</div>";
        break;
      }
      case "notice":
        // Earlier runners also wrote the end of the task as a notice; the
        // status line below says the same.
        if (/^任务结束：/.test(p.text || "")) return null;
        if (/^已为这个对话保存/.test(p.text || "")) refreshSessionSettings();
        wrap.innerHTML = '<div class="notice">' + esc(TR(p.text || "")) + "</div>";
        break;
      case "stderr":
        wrap.innerHTML = '<details class="notice stderr"><summary>Agent stderr</summary>' +
          "<div>" + esc(p.text || "") + "</div></details>";
        break;
      case "error":
        wrap.innerHTML = '<div class="error-block">' + esc(TR(p.text) || T("错误")) + "</div>";
        break;
      case "permission_denied":
        wrap.innerHTML = '<div class="notice">' + esc(T("权限被拒绝：%s", p.tool_name || "")) +
          (p.message ? " · " + esc(TR(p.message)) : "") + "</div>";
        break;
      case "usage": {
        var u = p.usage || {};
        var bits = [];
        if (u.input_tokens) bits.push(T("输入 %d", u.input_tokens));
        if (u.output_tokens) bits.push(T("输出 %d", u.output_tokens));
        if (u.cached_input_tokens) bits.push(T("缓存 %d", u.cached_input_tokens));
        if (!bits.length) return null;
        wrap.innerHTML = '<div class="meta-line">' + esc(bits.join(" · ")) + "</div>";
        break;
      }
      case "result": {
        var meta = [];
        if (p.duration_ms) meta.push((p.duration_ms / 1000).toFixed(1) + "s");
        if (p.cost_usd !== undefined && p.cost_usd !== null) meta.push("$" + Number(p.cost_usd).toFixed(4));
        if (p.num_turns) meta.push(T("%d 轮", p.num_turns));
        if (!meta.length) return null;
        wrap.innerHTML = '<div class="meta-line">' + esc(meta.join(" · ")) + "</div>";
        break;
      }
      case "raw":
        wrap.className = "ev raw";
        wrap.textContent = pretty(p.text || p);
        break;
      default:
        wrap.innerHTML = '<div class="notice">' + esc(truncate(pretty(p), 200)) + "</div>";
    }
    return wrap;
  }

  function reasonLabel(r) {
    var label = ({ cancelled: "用户取消", signaled: "Agent 进程异常退出", exited: "正常退出", lost: "进程丢失" })[r];
    return label ? T(label) : r;
  }

  function toolNode(p) {
    var node = document.createElement("div");
    node.className = "tool";
    node.setAttribute("data-tool-id", p.id || "");
    var isErr = p.status === "failed" || p.status === "error";
    var statusText = p.status === "running" || p.status === "in_progress" ? T("运行中") :
      isErr ? T("失败") : p.status === "completed" ? T("完成") : "";
    var hasOut = p.output !== undefined && p.output !== null && p.output !== "";
    node.innerHTML =
      '<div class="tool-head"><span class="tool-name">' + esc(toolLabel(p.name)) + "</span>" +
      '<span class="tool-summary">' + esc(toolSummary(p)) + "</span>" +
      '<span class="tool-state' + (isErr ? " err" : "") + '">' + esc(statusText) + "</span></div>" +
      '<div class="tool-body"><div class="tool-body-in">' +
      '<div class="tool-block"><div class="tool-label">' + T("输入") + '</div><pre class="tool-input" data-lazy-text></pre></div>' +
      (hasOut
        ? '<div class="tool-block"><div class="tool-label">' + T("输出") + '</div><pre class="tool-output" data-lazy-text></pre></div>'
        : '<div class="tool-block tool-out-block" hidden><div class="tool-label">' + T("输出") + '</div><pre class="tool-output" data-lazy-text></pre></div>') +
      (p.exit_code !== undefined && p.exit_code !== null && p.exit_code !== 0
        ? '<div class="small" style="color:var(--red)">exit ' + esc(String(p.exit_code)) + "</div>" : "") +
      "</div></div>";
    // Tool input/output is usually the bulk of a transcript but stays hidden
    // until the user expands it; format it on first open instead of at load.
    setLazy(node.querySelector(".tool-input"), p.input, "pre");
    if (hasOut) setLazy(node.querySelector(".tool-output"), p.output, "pre");
    node.querySelector(".tool-head").addEventListener("click", function () {
      node.classList.toggle("open");
      if (node.classList.contains("open")) fillLazy(node);
    });
    if (p.id) S.toolNodes[p.id] = node;
    return node;
  }

  function updateToolNode(p) {
    var node = S.toolNodes[p.id];
    if (!node || !node.isConnected) return false;
    var summary = node.querySelector(".tool-summary");
    if (summary) summary.textContent = toolSummary(p);
    var state = node.querySelector(".tool-state");
    if (state) {
      var failed = p.status === "failed" || p.status === "error";
      state.textContent = p.status === "running" || p.status === "in_progress" ? T("运行中") :
        failed ? T("失败") : p.status === "completed" ? T("完成") : "";
      state.className = "tool-state" + (failed ? " err" : "");
    }
    var hasOut = p.output !== undefined && p.output !== null && p.output !== "";
    if (hasOut) {
      var block = node.querySelector(".tool-out-block");
      var out = node.querySelector(".tool-output");
      if (block) block.hidden = false;
      if (out) setLazy(out, p.output, "pre");
      if (node.classList.contains("open")) fillLazy(node);
    }
    return true;
  }

  // Lazy text: the raw payload is kept on the node, but formatting and DOM text
  // are only produced when the block is first shown. mode "md" renders markdown,
  // "pre" keeps text verbatim.
  function setLazy(el, value, mode) {
    if (!el) return;
    el.__lazy = { value: value, mode: mode || "pre", filled: false };
    el.textContent = "";
    var d = el.closest("details");
    if (d) {
      if (d.open) fillLazyText(el);
      else if (!d.__lazyBound) {
        d.__lazyBound = true;
        d.addEventListener("toggle", function () {
          if (d.open) fillLazy(d);
        });
      }
      return;
    }
    if (el.closest(".tool.open")) fillLazyText(el);
  }

  function isClipped(v) { return !!(v && typeof v === "object" && v.__clipped); }

  function fillLazyText(el) {
    var d = el.__lazy;
    if (!d || d.filled) return;
    d.filled = true;
    if (isClipped(d.value)) { fillClipped(el, d); return; }
    if (typeof d.value !== "string") {
      el.textContent = pretty(d.value);
      return;
    }
    if (d.mode === "md") el.innerHTML = md(d.value);
    else el.textContent = d.value;
  }

  // A clipped field (large thinking / tool output) arrives as a preview; the full
  // event is fetched the first time the block is opened.
  function fillClipped(el, d) {
    var c = d.value;
    el.textContent = c.preview + "\n…";
    if (!S.session) return;
    api("/api/sessions/" + encodeURIComponent(S.session.id) + "/events/" + c.seq).then(function (r) {
      var full = r.event && r.event.payload ? r.event.payload[c.field] : undefined;
      if (full === undefined) return;
      if (d.mode === "md" && typeof full === "string") el.innerHTML = md(full);
      else el.textContent = typeof full === "string" ? full : pretty(full);
    }).catch(function (e) {
      el.textContent = c.preview + "\n…" + T("（完整内容加载失败：%s）", e.message);
      d.filled = false;
    });
  }

  // A short single-line preview for collapsed blocks. It must not serialize the
  // whole value: for large tool output that alone costs more than the saved DOM.
  function summarize(v, n) {
    if (v === null || v === undefined || v === "") return "";
    if (typeof v === "string") return truncate(v, n);
    if (isClipped(v)) return truncate(v.preview, n);
    if (typeof v === "object") {
      if (Array.isArray(v)) {
        var first = v.length ? summarize(v[0], n) : "";
        return truncate(T("[%d 项] ", v.length) + first, n);
      }
      var keys = Object.keys(v);
      var bits = [];
      for (var i = 0; i < keys.length && bits.length < 3; i++) {
        var val = v[keys[i]];
        if (typeof val === "string" || typeof val === "number" || typeof val === "boolean") {
          bits.push(keys[i] + "=" + truncate(String(val), 40));
        }
      }
      return truncate(bits.length ? "{ " + bits.join(", ") + " }" : "{" + T("%d 个字段", keys.length) + "}", n);
    }
    return truncate(String(v), n);
  }

  // setLazyText is the preformatted variant used where the text is already built.
  function setLazyText(el, text, pre) {
    setLazy(el, String(text == null ? "" : text), pre ? "pre" : "md");
  }

  function fillLazy(root) {
    if (!root) return;
    Array.prototype.forEach.call(root.querySelectorAll("[data-lazy-text]"), fillLazyText);
    // Lazy text nested in a details element that just opened.
    Array.prototype.forEach.call(root.querySelectorAll("details[open] [data-lazy-text]"), fillLazyText);
  }

  function mergeToolResult(p) {
    if (!p.id) return false;
    var node = S.toolNodes[p.id] ||
      document.querySelector('[data-tool-id="' + cssAttr(p.id) + '"]');
    if (!node || !node.parentNode) return false;
    var block = node.querySelector(".tool-out-block");
    var out = node.querySelector(".tool-output");
    if (!out) return false;
    if (block) block.hidden = false;
    if (isClipped(p.output)) setLazy(out, p.output, "pre");
    else setLazyText(out, pretty(p.output) || T("(无输出)"), true);
    var state = node.querySelector(".tool-state");
    if (state) {
      state.textContent = p.is_error ? T("失败") : T("完成");
      state.className = "tool-state" + (p.is_error ? " err" : "");
    }
    return true;
  }

  // An interaction is a permission prompt, a question or a plan (interact.go).
  // Pending ones arrive from the API with their options under `detail`; ones in
  // the transcript carry them on the event payload.
  function permModel(src) {
    var d = src.detail || {};
    return {
      id: src.id || src.permission_id, tool_name: src.tool_name, input: src.input,
      task_id: src.task_id, session_id: src.session_id, status: src.status || "pending",
      kind: src.kind || "permission",
      description: TR(src.description || d.description || ""),
      options: trOptions((src.options && src.options.length ? src.options : d.options) || null),
      questions: (src.questions && src.questions.length ? src.questions : d.questions) || [],
      plan: src.plan || d.plan || ""
    };
  }

  // The options a server offers on a permission card are its own text.
  function trOptions(list) {
    return list && list.map(function (o) {
      var c = {};
      for (var k in o) c[k] = o[k];
      c.label = TR(o.label);
      if (o.detail) c.detail = TR(o.detail);
      return c;
    });
  }

  var DEFAULT_OPTIONS = [{ id: "allow", label: "允许" }, { id: "deny", label: "拒绝" }];

  function optionButton(o) {
    var cls = o.id === "allow" || o.id === "approve_accept_edits" ? "primary" :
      (o.id === "deny" || o.id === "deny_stop") ? "danger" : "";
    return '<button class="perm-opt ' + cls + '" type="button" data-perm-opt="' + esc(o.id) + '">' +
      '<span class="perm-opt-label">' + esc(TR(o.label)) + "</span>" +
      (o.detail ? '<span class="perm-opt-detail">' + esc(o.detail) + "</span>" : "") + "</button>";
  }

  function permissionNode(src) {
    var perm = permModel(src);
    var node = document.createElement("div");
    node.className = "permission-card kind-" + perm.kind;
    node.setAttribute("data-permission-id", perm.id);
    var body = "";
    if (perm.kind === "question") {
      body = '<div class="permission-title"><span class="dot"></span>' + T("Agent 在提问") + "</div>" +
        perm.questions.map(function (q, qi) {
          return '<div class="q-block" data-q="' + qi + '">' +
            (q.header ? '<div class="q-header">' + esc(q.header) + "</div>" : "") +
            '<div class="q-text">' + esc(q.question) + (q.multi ? ' <span class="muted small">' + T("（可多选）") + "</span>" : "") + "</div>" +
            '<div class="q-options">' + (q.options || []).map(function (o, oi) {
              return '<button type="button" class="q-opt" data-q-opt="' + oi + '">' +
                '<span class="perm-opt-label">' + esc(o.label) + "</span>" +
                (o.description ? '<span class="perm-opt-detail">' + esc(o.description) + "</span>" : "") + "</button>";
            }).join("") + "</div>" +
            (q.other ? '<input class="q-other" type="' + (q.secret ? "password" : "text") +
              '" placeholder="' + esc((q.options || []).length ? T("其它（自己输入）") : T("输入你的回答")) + '" maxlength="2000">' : "") +
            "</div>";
        }).join("") +
        '<div class="permission-actions"><button class="btn primary sm" type="button" data-perm="submit" disabled>' + T("提交回答") + "</button>" +
        '<button class="btn sm ghost" type="button" data-perm="skip">' + T("跳过") + "</button></div>";
    } else if (perm.kind === "plan") {
      body = '<div class="permission-title"><span class="dot"></span>' + T("计划待批准") + "</div>" +
        '<div class="plan-body bubble">' + md(perm.plan || "") + "</div>" +
        '<div class="perm-opts">' + (perm.options || []).map(optionButton).join("") + "</div>" +
        '<div class="permission-reason" hidden><textarea rows="3" placeholder="' + esc(T("希望怎么调整计划？（可选）")) + '" maxlength="2000"></textarea>' +
        '<div class="row" style="margin-top:7px;gap:7px"><button class="btn sm ghost grow" type="button" data-perm="reason-cancel">' + T("取消") + "</button>" +
        '<button class="btn sm grow" type="button" data-perm="reason-confirm">' + T("继续规划") + "</button></div></div>";
    } else {
      // A command reads best as the command itself (Bash, Codex shell).
      var input = perm.input && typeof perm.input.command === "string" ? perm.input.command : pretty(perm.input);
      body = '<div class="permission-title"><span class="dot"></span>' + esc(T("需要批准：%s", toolLabel(perm.tool_name))) + "</div>" +
        (perm.description ? '<div class="permission-desc">' + esc(perm.description) + "</div>" : "") +
        (input ? '<div class="permission-input"><pre>' + esc(input) + "</pre></div>" : "") +
        '<div class="perm-opts">' + (perm.options || DEFAULT_OPTIONS).map(optionButton).join("") + "</div>" +
        '<div class="permission-reason" hidden><input type="text" placeholder="' + esc(T("拒绝原因（可选，会告诉 Agent）")) + '" maxlength="300">' +
        '<div class="row" style="margin-top:7px;gap:7px"><button class="btn sm ghost grow" type="button" data-perm="reason-cancel">' + T("取消") + "</button>" +
        '<button class="btn sm danger grow" type="button" data-perm="reason-confirm">' + T("确认拒绝") + "</button></div></div>";
    }
    node.innerHTML = body + '<div class="permission-result" hidden></div>';

    var pendingDecision = "";
    function answers() {
      var out = {}, complete = true;
      perm.questions.forEach(function (q, qi) {
        var block = node.querySelector('[data-q="' + qi + '"]');
        var vals = $$(".q-opt.on", block).map(function (b) { return q.options[+b.getAttribute("data-q-opt")].label; });
        var other = block.querySelector(".q-other");
        if (other && other.value.trim()) vals.push(other.value.trim());
        if (!vals.length) complete = false;
        out[q.id] = vals;
      });
      return complete ? out : null;
    }
    node.addEventListener("input", function () {
      var sub = node.querySelector('[data-perm="submit"]');
      if (sub) sub.disabled = !answers();
    });
    node.addEventListener("click", function (e) {
      var qo = e.target.closest("[data-q-opt]");
      if (qo) {
        var qi = +qo.closest("[data-q]").getAttribute("data-q");
        if (!perm.questions[qi].multi) $$(".q-opt", qo.parentNode).forEach(function (b) { if (b !== qo) b.classList.remove("on"); });
        qo.classList.toggle("on");
        node.querySelector('[data-perm="submit"]').disabled = !answers();
        return;
      }
      var opt = e.target.closest("[data-perm-opt]");
      if (opt) {
        var id = opt.getAttribute("data-perm-opt");
        // Denying and revising a plan can carry a note for the agent.
        if (id === "deny" || id === "revise") {
          pendingDecision = id;
          node.querySelector(".permission-reason").hidden = false;
          var f = node.querySelector(".permission-reason input, .permission-reason textarea");
          if (f) f.focus();
          return;
        }
        resolvePermission(perm, { decision: id }, node);
        return;
      }
      var b = e.target.closest("[data-perm]");
      if (!b) return;
      var act = b.getAttribute("data-perm");
      if (act === "reason-cancel") node.querySelector(".permission-reason").hidden = true;
      else if (act === "reason-confirm") {
        var field = node.querySelector(".permission-reason input, .permission-reason textarea");
        resolvePermission(perm, { decision: pendingDecision, message: field ? field.value.trim() : "" }, node);
      } else if (act === "submit") {
        var a = answers();
        if (a) resolvePermission(perm, { decision: "answer", answers: a }, node);
      } else if (act === "skip") resolvePermission(perm, { decision: "skip" }, node);
    });
    return node;
  }

  function resolvePermission(perm, body, node) {
    $$("button", node).forEach(function (b) { b.disabled = true; });
    api("/api/permissions/" + encodeURIComponent(perm.id), { method: "POST", body: body }).then(function (r) {
      markPermissionResolved(perm.id, r.status, { label: r.label, detail: r.detail });
      // Approving a plan also decides how the next messages run, as in the CLI.
      if (body.decision === "approve_accept_edits") setProfile("acceptEdits");
      else if (body.decision === "approve_default") setProfile("manual");
      toast(r.label || T("已处理"));
    }).catch(function (e) {
      toast(e.message, true);
      $$("button", node).forEach(function (b) { b.disabled = false; });
      var sub = node.querySelector('[data-perm="submit"]');
      if (sub) node.dispatchEvent(new Event("input"));
    });
  }

  function setProfile(id) {
    var sel = $("#profile-select");
    if (!sel || !profileMeta(currentAgentName(), id)) return;
    sel.value = id;
    sel.dispatchEvent(new Event("change"));
  }

  // Decisions are remembered as well as applied: on a full load the result
  // event is handled while its card is still in a detached fragment, where a
  // document lookup cannot find it. appendEvents re-applies them once the
  // fragment is in the page; before, every resolved card in a reopened session
  // still offered 允许/拒绝 and clicking it failed with "already handled".
  function markPermissionResolved(id, decision, info) {
    S.permDecisions[id] = { decision: decision, info: info || null };
    applyPermissionResolved(document.querySelector('[data-permission-id="' + cssAttr(id) + '"]'), decision, info);
    S.sessionPerms = S.sessionPerms.filter(function (p) { return p.id !== id; });
    S.perms = S.perms.filter(function (p) { return p.id !== id; });
    renderPermissionBanner();
    renderApprovalNotifications();
  }

  function applyPermissionResolved(node, decision, info) {
    if (node && !node.classList.contains("resolved")) {
      node.classList.add("resolved");
      $$(".permission-actions, .perm-opts, .q-options, .q-other", node).forEach(function (el) { el.remove(); });
      var acts = node.querySelector(".permission-actions");
      if (acts) acts.remove();
      var reason = node.querySelector(".permission-reason");
      if (reason) reason.remove();
      var res = node.querySelector(".permission-result");
      if (res) {
        res.hidden = false;
        var ok = decision === "allowed" || decision === "allow" || decision === "answered";
        if (info && info.label) {
          res.textContent = (ok ? "✓ " : "✕ ") + TR(info.label) + (info.detail ? " · " + TR(info.detail) : "");
        } else {
          res.textContent = decision === "allowed" || decision === "allow" ? "✓ " + T("已允许") :
            decision === "answered" ? "✓ " + T("已回答") : decision === "expired" ? T("已超时") : decision ? "✕ " + T("已拒绝") : T("已处理");
        }
      }
    }
  }

  function ensurePermissionCards() {
    var pend = (S.sessionPerms || []).filter(function (p) { return p.status === "pending"; });
    var inner = $("#events-inner");
    if (!inner) return;
    pend.forEach(function (p) {
      if (document.querySelector('[data-permission-id="' + cssAttr(p.id) + '"]')) return;
      var n = permissionNode(p);
      n.classList.add("ev");
      n.setAttribute("data-kind", "permission");
      inner.appendChild(n);
    });
  }

  function cssAttr(v) {
    v = String(v || "");
    return v.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  }

  // ------------------------------------------------------------------- stream

  function connectStream() {
    closeStream();
    if (!S.session) return;
    var id = S.session.id;
    var url = nodePath("/api/sessions/" + encodeURIComponent(id) + "/stream?after=" + S.lastSeq);
    var es;
    try { es = new EventSource(url); } catch (e) { setConn("off"); return; }
    S.es = es;
    setConn("wait");
    es.addEventListener("hello", function (ev) {
      setConn("on");
      try {
        var d = JSON.parse(ev.data);
        if (d.last_seq > S.lastSeq) catchUp();
      } catch (e) {}
    });
    es.addEventListener("message", function (ev) {
      try {
        var d = JSON.parse(ev.data);
        appendEvents([d]);
      } catch (e) {}
    });
    es.addEventListener("tasks", function (ev) {
      try {
        var d = JSON.parse(ev.data);
        S.sessionActive = d.tasks || [];
        S.sessionPerms = d.permissions || [];
        updateChatChrome();
        ensurePermissionCards();
        renderPermissionBanner();
        // A task just finished: refresh sidebar/task data eventually.
        if (!S.sessionActive.length) refreshActive().catch(function () {});
      } catch (e) {}
    });
    // The server signals a broken event stream explicitly; EventSource would
    // otherwise treat it as a normal close and keep a dead connection.
    es.addEventListener("error", function (ev) {
      if (!ev.data) return; // native transport error, handled by onerror below
      setConn("off");
      if (es.readyState === 2) scheduleReconnect(es, 2000);
    });
    es.onopen = function () { if (S.es === es) setConn("on"); };
    es.onerror = function () {
      if (S.es !== es) return;
      setConn("off");
      if (es.readyState === 2) scheduleReconnect(es, 2500);
    };
  }

  // EventSource cannot see the HTTP status, so an expired session would make it
  // retry a 401 forever with no sign of what is wrong. Check the session before
  // reconnecting and fall back to the login screen.
  function scheduleReconnect(es, delay) {
    setTimeout(function () {
      if (S.es !== es || !S.session || S.route !== "chat") return;
      fetch("/api/session", { credentials: "same-origin", cache: "no-store" })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (!d || !d.authenticated) { showLogin(); return; }
          if (S.es === es && S.session && S.route === "chat") connectStream();
        })
        .catch(function () {
          if (S.es === es && S.session && S.route === "chat") connectStream();
        });
    }, delay);
  }

  function closeStream() {
    if (S.es) { try { S.es.close(); } catch (e) {} S.es = null; }
  }

  var catchingUp = false;
  function catchUp() {
    if (catchingUp || !S.session) return;
    catchingUp = true;
    var sid = S.session.id;
    function step() {
      if (!S.session || S.session.id !== sid) { catchingUp = false; return; }
      api("/api/sessions/" + encodeURIComponent(sid) + "/events?after=" + S.lastSeq + "&limit=500")
        .then(function (d) {
          // The user may have navigated to another session while this was in
          // flight; its events must not be appended to the new page.
          if (!S.session || S.session.id !== sid) { catchingUp = false; return; }
          var evs = d.events || [];
          if (evs.length) appendEvents(evs);
          if (d.has_more) { step(); return; }
          catchingUp = false;
          ensurePermissionCards();
          updateChatChrome();
        }).catch(function () { catchingUp = false; });
    }
    step();
  }

  // ------------------------------------------------- model picker + slash menu

  function cacheKey(agent) { return (S.node || "local") + "/" + agent; }

  function loadCommands(agent) {
    var k = cacheKey(agent);
    if (S.commands[k]) return Promise.resolve(S.commands[k]);
    return api("/api/agents/" + encodeURIComponent(agent) + "/commands").then(function (d) {
      S.commands[k] = d.commands || [];
      return S.commands[k];
    }).catch(function () { return []; });
  }

  function loadModels(agent) {
    // Always query the selected node, even if the picker has been opened before.
    return api("/api/agents/" + encodeURIComponent(agent) + "/models");
  }

  function currentSettings() { return (S.session ? S.sessionSettings : S.draftSettings) || {}; }

  function syncModelBtn() {
    var st = currentSettings();
    var agent = currentAgentName();
    var d = S.models[cacheKey(agent)];
    var m = d && (d.models || []).filter(function (x) { return x.id === (st.model || ""); })[0];
    var label = st.model ? (m ? m.label : st.model) : T("默认模型");
    $("#model-label").textContent = label + (st.effort ? " · " + st.effort : "");
  }

  function refreshSessionSettings() {
    if (!S.session) return;
    var sid = S.session.id;
    api("/api/sessions/" + encodeURIComponent(sid)).then(function (r) {
      if (S.session && S.session.id === sid) { S.sessionSettings = r.settings || {}; syncModelBtn(); }
    }).catch(function () {});
  }

  // Picking in the menu sets the conversation's settings directly. A bare
  // /model opens this picker; a command with arguments goes through the agent.
  function saveSettings(next) {
    if (!S.session) {
      S.draftSettings = next;
      syncModelBtn();
      return Promise.resolve();
    }
    return api("/api/sessions/" + encodeURIComponent(S.session.id), { method: "PATCH", body: { settings: next } })
      .then(function (r) { S.sessionSettings = r.settings || next; syncModelBtn(); });
  }

  var modelMenuRequest = 0;
  function closeModelMenu() {
    modelMenuRequest++;
    S.modelQuery = "";
    S.modelQueryFocused = false;
    $("#model-menu").hidden = true;
    $("#model-btn").setAttribute("aria-expanded", "false");
  }

  // The list always comes from the selected device's CLI, but the last one is
  // shown at once while the fresh one loads (Codex takes seconds to answer).
  // refresh=false redraws from that copy only (after a choice).
  function openModelMenu(refresh) {
    closeAgentMenu();
    var agent = currentAgentName(), menu = $("#model-menu");
    var key = cacheKey(agent), request = ++modelMenuRequest;
    var cached = S.models[key];
    menu.hidden = false;
    $("#model-btn").setAttribute("aria-expanded", "true");
    if (cached) renderModelMenu(cached, refresh !== false);
    else menu.innerHTML = '<div class="menu-item"><span class="spinner"></span> ' + esc(T("正在向 %s 查询模型列表…", nodeLabel(S.node))) + "</div>";
    if (refresh === false && cached) return;
    loadModels(agent).then(function (d) {
      if (request !== modelMenuRequest || key !== cacheKey(currentAgentName())) return;
      S.models[key] = d; // Retain only the accepted result for button labels.
      renderModelMenu(d, false);
      syncModelBtn();
    }).catch(function (e) {
      if (request !== modelMenuRequest || key !== cacheKey(currentAgentName())) return;
      if (cached) { renderModelMenu(cached, false, e.message); return; }
      menu.innerHTML = '<div class="menu-item menu-error">' + esc(e.message) +
        '<div class="menu-detail">' + T("仍可直接输入 <code>/model 模型名</code> 切换。") + "</div></div>";
    });
  }

  function renderModelMenu(d, refreshing, failed) {
    var menu = $("#model-menu");
    var st = currentSettings();
    // A refresh redraws the menu; whoever is typing in the search box keeps
    // their text and cursor.
    var old = menu.querySelector(".model-search input");
    var typing = old && document.activeElement === old;
    var caret = typing ? old.selectionStart : 0;
    var models = d.models || [];
    var cur = models.filter(function (m) { return m.id === (st.model || ""); })[0] ||
      models.filter(function (m) { return m.default; })[0];
    var efforts = (cur && cur.efforts && cur.efforts.length ? cur.efforts : d.efforts) || [];
    var searchable = (d.models || []).length > 8;
    menu.innerHTML = (searchable ? '<div class="model-search"><svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true">' +
      '<circle cx="11" cy="11" r="6"/><path d="m20 20-4.5-4.5"/></svg>' +
      '<input type="search" placeholder="' + esc(T("搜索模型")) + '" autocomplete="off" autocapitalize="off" spellcheck="false" value="' +
      esc(S.modelQuery || "") + '"></div>' : "") +
      '<div class="menu-section">' + T("模型") +
      (refreshing ? ' <span class="spinner sm"></span>' : failed ? ' <span class="menu-stale" title="' + esc(failed) + '">' + T("（未能刷新，显示上次的列表）") + "</span>" : "") +
      "</div>" + models.map(function (m) {
      var on = (st.model || "") === m.id || (!st.model && !m.id);
      return '<button type="button" class="menu-item' + (on ? " on" : "") + '" data-model="' + esc(m.id) + '" data-find="' +
        esc((m.label + " " + m.id).toLowerCase()) + '">' +
        '<span class="menu-text"><span class="menu-label">' + esc(TR(m.label)) + (m.default && m.id ? T("（默认）") : "") + "</span>" +
        (m.id ? '<span class="menu-detail mono">' + esc(m.id) + "</span>" : "") + "</span>" +
        '<svg class="ico sm menu-check" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg></button>';
    }).join("") + (efforts.length ? '<div class="menu-section">' + T("推理强度") + '</div><div class="effort-row">' +
      ['<button type="button" class="chip' + (!st.effort ? " active" : "") + '" data-effort="">' + T("默认") + "</button>"]
        .concat(efforts.map(function (e) {
          return '<button type="button" class="chip' + (st.effort === e ? " active" : "") + '" data-effort="' + esc(e) + '">' + esc(e) + "</button>";
        })).join("") + "</div>" : "");
    var input = menu.querySelector(".model-search input");
    if (!input) return;
    var empty = document.createElement("div");
    empty.className = "menu-item model-none";
    empty.textContent = T("没有匹配的模型");
    var items = $$("[data-model]", menu);
    if (items.length) items[items.length - 1].after(empty);
    filterModels(input.value);
    input.addEventListener("input", function () { S.modelQuery = this.value; filterModels(this.value); });
    input.addEventListener("keydown", function (e) {
      if (e.key === "Escape") { e.preventDefault(); closeModelMenu(); return; }
      if (e.key === "Enter") {
        e.preventDefault();
        var first = $$("[data-model]", menu).filter(function (b) { return !b.hidden; })[0];
        if (first) first.click();
      }
    });
    // Typing straight away on a computer; on a phone the keyboard waits for a tap.
    if (typing || (!refreshing && !failed && matchMedia("(pointer: fine)").matches && !S.modelQueryFocused)) {
      input.focus();
      if (typing) input.setSelectionRange(caret, caret);
      S.modelQueryFocused = true;
    }
  }

  // filterModels keeps the models whose name or id contains every word typed.
  function filterModels(q) {
    var menu = $("#model-menu");
    var words = String(q || "").toLowerCase().split(/\s+/).filter(Boolean);
    var shown = 0;
    $$("[data-model]", menu).forEach(function (b) {
      var hay = b.getAttribute("data-find") || "";
      b.hidden = !words.every(function (w) { return hay.indexOf(w) >= 0; });
      if (!b.hidden) shown++;
    });
    var none = menu.querySelector(".model-none");
    if (none) none.hidden = shown > 0;
  }

  $("#model-btn").addEventListener("click", function (e) {
    e.stopPropagation();
    closeProfileMenu();
    if ($("#model-menu").hidden) openModelMenu(); else closeModelMenu();
  });
  $("#model-menu").addEventListener("click", function (e) {
    e.stopPropagation();
    var st = currentSettings();
    var m = e.target.closest("[data-model]");
    if (m) {
      var next = { model: m.getAttribute("data-model"), effort: st.effort || "" };
      saveSettings(next).then(function () { toast(T("模型：%s", $("#model-label").textContent)); openModelMenu(false); })
        .catch(function (err) { toast(err.message, true); });
      return;
    }
    var ef = e.target.closest("[data-effort]");
    if (ef) {
      saveSettings({ model: st.model || "", effort: ef.getAttribute("data-effort") })
        .then(function () { closeModelMenu(); toast(T("推理强度：%s", ef.getAttribute("data-effort") || T("默认"))); })
        .catch(function (err) { toast(err.message, true); });
    }
  });
  document.addEventListener("click", function (e) {
    if (!e.target.closest("#model-menu") && !e.target.closest("#model-btn")) closeModelMenu();
    if (!e.target.closest("#slash-menu") && !e.target.closest("#prompt")) hideSlashMenu();
  });

  // Typing "/" offers the agent's commands, as the CLI does.
  function hideSlashMenu() {
    var m = $("#slash-menu");
    if (m) m.hidden = true;
    S.slashItems = [];
  }

  function updateSlashMenu() {
    var ta = $("#prompt");
    var match = ta.value.match(/^\/([^\s]*)$/);
    if (!match) { hideSlashMenu(); return; }
    var q = match[1].toLowerCase();
    var agent = currentAgentName();
    loadCommands(agent).then(function (list) {
      if (!$("#prompt").value.match(/^\/([^\s]*)$/)) return;
      var starts = list.filter(function (c) { return c.name.toLowerCase().indexOf(q) === 0; });
      var rest = list.filter(function (c) { return q && c.name.toLowerCase().indexOf(q) > 0; });
      S.slashItems = starts.concat(rest).slice(0, 9);
      S.slashIdx = 0;
      renderSlashMenu();
    });
  }

  function renderSlashMenu() {
    var m = $("#slash-menu");
    if (!S.slashItems.length) { m.hidden = true; return; }
    m.innerHTML = S.slashItems.map(function (c, i) {
      return '<button type="button" class="menu-item' + (i === S.slashIdx ? " on hl" : "") + '" data-slash="' + i + '">' +
        '<span class="menu-text"><span class="menu-label mono">/' + esc(c.name) +
        (c.args ? ' <span class="muted">' + esc(c.args) + "</span>" : "") + "</span>" +
        (c.description ? '<span class="menu-detail">' + esc(TR(c.description)) + "</span>" : "") + "</span></button>";
    }).join("");
    m.hidden = false;
  }

  function completeSlash(i) {
    var c = S.slashItems[i];
    if (!c) return;
    var ta = $("#prompt");
    ta.value = "/" + c.name + " ";
    hideSlashMenu();
    ta.focus();
  }

  // slashKey handles the menu's keys; true means the key was consumed.
  function slashKey(e) {
    var m = $("#slash-menu");
    if (!m || m.hidden || !S.slashItems.length) return false;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      var n = S.slashItems.length;
      S.slashIdx = (S.slashIdx + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
      renderSlashMenu();
      return true;
    }
    if (e.key === "Escape") { e.preventDefault(); hideSlashMenu(); return true; }
    if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey && !e.isComposing)) {
      var typed = $("#prompt").value.trim().slice(1).toLowerCase();
      var c = S.slashItems[S.slashIdx];
      // Enter on a fully typed command runs it, as in the CLI.
      if (e.key === "Enter" && c && c.name.toLowerCase() === typed) { hideSlashMenu(); return false; }
      e.preventDefault();
      completeSlash(S.slashIdx);
      return true;
    }
    return false;
  }

  $("#prompt").addEventListener("input", updateSlashMenu);
  $("#slash-menu").addEventListener("mousedown", function (e) { e.preventDefault(); });
  $("#slash-menu").addEventListener("click", function (e) {
    var b = e.target.closest("[data-slash]");
    if (b) completeSlash(+b.getAttribute("data-slash"));
  });

  // ---------------------------------------------------------------- composer

  $("#composer").addEventListener("submit", function (e) {
    e.preventDefault();
    sendMessage();
  });

  $("#prompt").addEventListener("input", function () {
    this.style.height = "auto";
    this.style.height = Math.min(this.scrollHeight, 140) + "px";
  });

  // The composer floats over the chat, so the chat reserves exactly its height
  // at the bottom. A fixed guess left a large blank gap under the last message.
  (function () {
    var wrap = $("#composer-wrap");
    if (!wrap || typeof ResizeObserver === "undefined") return;
    new ResizeObserver(function () {
      var h = wrap.offsetHeight;
      if (!h) return; // hidden: keep the last value
      var stick = nearBottom();
      document.documentElement.style.setProperty("--composer-h", h + "px");
      if (stick) window.scrollTo(0, document.body.scrollHeight);
    }).observe(wrap);
  })();

  function sendMessage() {
    var ta = $("#prompt");
    var text = ta.value.trim();
    if (!text || S.sending) return;
    if (S.link.state !== "ok") {
      toast(S.link.state === "failed" ? T("无法连接 %s，请重试", nodeLabel(S.node)) : T("正在连接 %s，请稍候", nodeLabel(S.node)));
      return;
    }
    // A bare /model or /effort opens the picker, as in the interactive CLI;
    // with an argument it goes to the agent like any command.
    if (/^\/(model|effort)$/i.test(text)) {
      ta.value = "";
      ta.style.height = "auto";
      hideSlashMenu();
      closeProfileMenu();
      openModelMenu();
      return;
    }
    if (!S.session) { sendNewChat(text); return; }
    var profile = selectedProfile();
    // Reuse the request id when the user retries the same text after a failed
    // response: the server dedupes on it, so a task that was actually created
    // is not queued twice.
    if (!S.pendingSend || S.pendingSend.text !== text) {
      S.pendingSend = { text: text, rid: rid() };
    }
    var ridValue = S.pendingSend.rid;
    var sid = S.session.id;
    S.sending = true;
    $("#send-btn").disabled = true;
    var optimistic = document.createElement("div");
    optimistic.className = "ev msg user pending";
    optimistic.setAttribute("data-kind", "user");
    optimistic.setAttribute("data-pending-text", text);
    optimistic.title = T("发送中…");
    optimistic.innerHTML = '<div class="bubble">' + esc(text) + "</div>";
    var inner = $("#events-inner");
    if (inner) { inner.appendChild(optimistic); scrollBottom(false); }

    api("/api/sessions/" + encodeURIComponent(sid) + "/messages", {
      method: "POST", body: { text: text, profile: profile, request_id: ridValue }
    }).then(function (d) {
      S.pendingSend = null;
      // The user may have switched sessions while the request was in flight;
      // the response belongs to the session that was open when it was sent.
      if (!S.session || S.session.id !== sid) return;
      ta.value = "";
      ta.style.height = "auto";
      hideSlashMenu();
      if (d.local) {
        // Answered by the console itself (slash.go): /clear and /new open a
        // fresh conversation; the others printed their reply into this one.
        optimistic.remove();
        if (d.session) {
          S.sessions.unshift(d.session);
          toast(T("已开始新对话"));
          go("#/session/" + encodeURIComponent(d.session.id));
        } else {
          catchUp();
          api("/api/sessions/" + encodeURIComponent(sid)).then(function (r) {
            S.session.title = r.session.title;
            S.sessions.forEach(function (x) { if (x.id === sid) x.title = r.session.title; });
            updateChatChrome();
            renderSessionList();
          }).catch(function () {});
        }
        return;
      }
      var t = d.task;
      S.sessionActive = S.sessionActive.filter(function (x) { return x.id !== t.id; });
      S.sessionActive.push(t);
      if (!S.session.title || S.session.title.indexOf("新会话") === 0) {
        S.session.title = truncate(text, 72);
        S.sessions.forEach(function (x) { if (x.id === S.session.id) x.title = S.session.title; });
      }
      updateChatChrome();
      refreshActive().catch(function () {});
      if (d.queue_ahead > 0) toast(T("已排队，前面还有 %d 个任务", d.queue_ahead));
    }).catch(function (err) {
      optimistic.remove();
      if (!S.session || S.session.id !== sid) { toast(err.message, true); return; }
      ta.value = text;
      toast(err.message, true);
    }).then(function () {
      S.sending = false;
      $("#send-btn").disabled = false;
    });
  }

  function sendNewChat(text) {
    ensureDraftAgent();
    var agent = agentMeta(S.draftAgent);
    if (!agent || !agent.name || !agent.available) { toast(T("请先选择可用的 Agent"), true); return; }
    var profile = selectedProfile();
    var cwd = S.draftCwd || S.config.default_cwd || "";
    // A retry after a lost response must not queue the same message twice.
    if (!S.pendingSend || S.pendingSend.text !== text) {
      S.pendingSend = { text: text, rid: rid() };
    }
    var ridValue = S.pendingSend.rid;
    var sid = "";
    S.sending = true;
    $("#send-btn").disabled = true;
    $("#composer-hint").textContent = T("正在创建对话…");

    api("/api/sessions", { method: "POST", body: { agent: agent.name, cwd: cwd, title: "", settings: S.draftSettings } })
      .then(function (d) {
        sid = d.session.id;
        S.sessions.unshift(d.session);
        return api("/api/sessions/" + encodeURIComponent(sid) + "/messages", {
          method: "POST", body: { text: text, profile: profile, request_id: ridValue }
        });
      }).then(function (d) {
        S.pendingSend = null;
        var ta = $("#prompt");
        ta.value = "";
        ta.style.height = "auto";
        hideSlashMenu();
        go("#/session/" + encodeURIComponent(d.local && d.session ? d.session.id : sid));
        if (d.queue_ahead > 0) toast(T("已排队，前面还有 %d 个任务", d.queue_ahead));
      }).catch(function (err) {
        toast(err.message, true);
        if (sid) go("#/session/" + encodeURIComponent(sid));
        else $("#composer-hint").textContent = "";
      }).then(function () {
        S.sending = false;
        $("#send-btn").disabled = false;
      });
  }

  // Keyboard: desktop Enter sends, Shift+Enter newline; touch devices use the button.
  var coarse = matchMedia("(pointer: coarse)").matches;
  $("#prompt").addEventListener("keydown", function (e) {
    if (slashKey(e)) return;
    if (e.key === "Enter" && !e.shiftKey && !coarse && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      sendMessage();
    }
  });

  $("#stop-btn").addEventListener("click", function () {
    var act = activeOnSession();
    if (!act) return;
    var btn = this;
    confirmSheet(T("停止任务？"), "<p>" + T("将向 Agent 发送终止信号，正在执行的命令可能被中断。排在后面的消息会继续执行。") + "</p>", T("停止"), true)
      .then(function (ok) {
        if (!ok) return;
        btn.disabled = true;
        api("/api/tasks/" + encodeURIComponent(act.id) + "/cancel", { method: "POST", body: {} })
          .then(function () { toast(T("正在停止…")); refreshActive().catch(function () {}); })
          .catch(function (err) { toast(err.message, true); btn.disabled = false; });
      });
  });

  $("#profile-select").addEventListener("change", function () {
    var agent = currentAgentName();
    var meta = profileMeta(agent, this.value);
    LS.set("profile." + agent, this.value);
    this.title = meta ? meta.detail : "";
    syncProfileBtn();
    if (meta && meta.dangerous) toast(T("Agent 将拥有完整权限，请谨慎使用"), true);
  });

  // ------------------------------------------------------- theme and language

  $("#main").addEventListener("click", function (e) {
    var themeChoice = e.target.closest("[data-theme-choice]");
    if (themeChoice) {
      applyTheme(themeChoice.getAttribute("data-theme-choice"));
      if (S.route === "settings") renderSettings();
      return;
    }
    var langChoice = e.target.closest("[data-lang-choice]");
    if (langChoice) setLanguage(langChoice.getAttribute("data-lang-choice"));
  });

  // The language belongs to this app (the hub), not to the device being
  // shown: the Mac and Android apps read it for their own menus and
  // notifications too. The page reloads to redraw everything in it.
  function setLanguage(lang) {
    if (lang === S.langPref) return;
    api("/api/settings", { method: "PATCH", hub: true, body: { language: lang } }).then(function () {
      S.langPref = lang;
      location.reload();
    }).catch(function (e) { toast(e.message, true); });
  }

  // ----------------------------------------------------------------- settings

  // Settings open at once from the device's last response and refresh in the
  // background. A remote device costs a round trip over SSH (seconds on a
  // phone that has just reconnected), and probing every device costs one per
  // device; waiting for both used to leave the page on a spinner.
  function renderSettings() {
    S.route = "settings";
    $("#composer-wrap").hidden = true;
    document.body.classList.remove("has-composer");
    $("#conn-dot").hidden = true;
    $("#term-btn").hidden = true;
    $("#files-btn").hidden = true;
    $("#new-chat-btn").hidden = true;
    $("#settings-btn").classList.add("active");
    setBackButton(true);
    setAgentPicker(false);
    setTop(T("系统设置"), "");
    renderApprovalNotifications();
    var node = S.node;
    var cached = S.settingsCache[node];
    S.settingsPainted = "";
    if (cached) paintSettings(cached);
    else $("#main").innerHTML = '<div class="pad"><div class="empty"><span class="spinner"></span> ' + T("加载中…") + "</div></div>";
    Promise.all([api("/api/settings"), loadNodes(true)]).then(function (res) {
      var d = res[0];
      S.settingsCache[node] = d;
      // The user may have navigated away while settings were loading; the
      // response must not repaint the page they are looking at now.
      if (S.route !== "settings" || S.node !== node) return;
      S.agents = d.agents || S.agents;
      S.config = d.config || S.config;
      paintSettings(d);
    }).catch(function (e) {
      if (S.route !== "settings" || S.node !== node || cached) return;
      $("#main").innerHTML = '<div class="empty">' + esc(T("加载失败：%s", e.message)) + "</div>";
    });
    // Fresh online states for the device rows.
    loadNodes().then(function () {
      var d = S.settingsCache[S.node];
      if (S.route === "settings" && d) paintSettings(d);
    });
  }

  // paintSettings repaints only when something shown changed, and keeps the
  // open sections and scroll position; it leaves the page alone while the
  // user is typing in it.
  function paintSettings(d) {
    var sig = JSON.stringify([d, S.nodes, S.hubOnly, S.theme, S.langPref]);
    if (sig === S.settingsPainted) return;
    var main = $("#main");
    var focus = document.activeElement;
    if (S.settingsPainted && focus && main.contains(focus) && /^(INPUT|TEXTAREA)$/.test(focus.tagName)) return;
    var open = Array.prototype.map.call(main.querySelectorAll("details"), function (x) { return x.open; });
    var keep = S.settingsPainted !== "";
    var y = window.scrollY, my = main.scrollTop;
    S.settingsPainted = sig;
    renderSettingsData(d);
    if (!keep) return;
    Array.prototype.forEach.call(main.querySelectorAll("details"), function (x, i) { if (open[i]) x.open = true; });
    main.scrollTop = my;
    window.scrollTo(0, y);
  }

  // In settings the top-left button goes back instead of opening the list.
  function setBackButton(on) {
    var b = $("#menu-btn");
    if (b.classList.contains("back") === on) return;
    b.classList.toggle("back", on);
    b.setAttribute("aria-label", on ? T("返回") : T("打开对话列表"));
    b.querySelector("path").setAttribute("d", on ? "M15 5l-7 7 7 7" : "M4 8h16M4 16h10");
  }

  function leaveSettings() {
    var back = S.settingsReturn || "#/";
    S.settingsReturn = "";
    go(back);
  }

  // "2.1.303 (Claude Code)" and "codex-cli 0.163.0" both shorten to the number.
  function shortVersion(v) {
    var m = String(v || "").match(/\d+\.\d+(?:\.\d+)?/);
    return m ? m[0] : "";
  }

  function countsSummary(c) {
    var order = [["completed", "已完成"], ["running", "运行中"], ["waiting_input", "等待审批"],
      ["pending", "启动中"], ["failed", "失败"], ["interrupted", "中断"], ["cancelled", "取消"]];
    var out = order.filter(function (x) { return c && c[x[0]]; })
      .map(function (x) { return T(x[1]) + " " + c[x[0]]; });
    return out.join(" · ") || T("暂无任务");
  }

  function renderSettingsData(d) {
    var cfg = d.config || {};
    // Agent rows expand in place; the summary line shows name, version and state.
    var agents = (d.agents || []).map(function (a) {
      var n = esc(a.name);
      var state = a.available ? (a.authenticated ? T("可用") : T("未认证")) : T("不可用");
      return '<details><summary>' +
        '<span class="agent-head">' + agentIcon(a.name) +
        '<span class="agent-name">' + esc(a.label || a.name) + "</span>" +
        '<span class="agent-ver">' + esc(shortVersion(a.version) || "—") + "</span></span>" +
        '<span class="badge ' + (a.available && a.authenticated ? "status completed" : a.available ? "status pending" : "status failed") + '">' +
        state + "</span></summary>" +
        '<div class="g-body"><dl class="agent-meta">' +
        "<dt>" + T("版本") + "</dt><dd>" + esc(a.version || "—") + "</dd>" +
        "<dt>" + T("路径") + "</dt><dd class=\"mono\">" + esc(a.binary || "—") + "</dd>" +
        "<dt>" + T("认证") + "</dt><dd>" + esc(TR(a.auth_detail || a.error) || "—") + "</dd>" +
        "<dt>" + T("会话续接") + "</dt><dd>" + (a.supports_resume ? T("支持") : T("不支持")) + "</dd>" +
        "<dt>" + T("权限交互") + "</dt><dd>" + (a.supports_permissions ? T("支持网页审批") : T("不支持（使用沙箱限制）")) + "</dd>" +
        "<dt>" + T("说明") + "</dt><dd>" + esc(TR(a.permission_detail || "")) + "</dd>" +
        "</dl>" +
        '<div class="agent-actions">' +
        '<button type="button" class="btn sm" data-agent-check="' + n + '" title="' + esc(T("向 npm 查询最新版本")) + '">' + T("检查更新") + "</button>" +
        '<button type="button" class="btn sm" data-agent-restart="' + n + '" title="' + esc(T("重新检测配置，并重启该 Agent 正在运行的任务")) + '">' + T("重启 Agent") + "</button>" +
        "</div>" +
        '<div class="agent-update small muted" data-agent-update="' + n + '"></div>' +
        "</div></details>";
    }).join("");

    var counts = d.counts || {};
    var addr = "http://" + (cfg.easytier_ip || location.hostname) + ":" + (cfg.port || "");

    var devices = '<div><div class="section-title">' + T("设备") + '</div><div class="group">' + settingsDeviceRows() +
      '<button type="button" class="g-row link" data-add-device="1"><span class="g-label">' + T("添加服务器") + "</span>" +
      '<span class="g-value">' + T("通过 SSH") + "</span></button></div>" +
      '<div class="g-note">' + T("客户端通过 SSH 连接服务器并自动部署节点；服务器只需开放 SSH。") +
      (multiNode() ? T("下面的 Agent、通用与服务设置属于当前设备「%s」。", esc(nodeLabel(S.node))) : "") + "</div></div>" +
      '<div><div class="section-title">' + T("密钥与迁移") + '</div><div class="group">' +
      '<button type="button" class="g-row link" data-keys="1"><span class="g-label">' + T("密钥") + '</span><span class="g-value">' + T("SSH 密钥库") + "</span></button>" +
      '<button type="button" class="g-row link" data-export="1"><span class="g-label">' + T("导出设备") + '</span><span class="g-value">' + T("二维码或文件") + "</span></button>" +
      '<button type="button" class="g-row link" data-import="1"><span class="g-label">' + T("导入设备") + "</span></button></div>" +
      '<div class="g-note">' + T("二维码用手机上生成的配对码加密，文件用随机生成的解密码加密，可以把电脑上的服务器配置快速迁到手机。只包含 SSH 与节点凭据，不含任何 Agent 账号。") + "</div></div>";
    // Appearance and language belong to this app, whichever device is shown.
    var langs = [["", T("跟随系统")], ["zh", "中文"], ["en", "English"]];
    var look = '<div><div class="section-title">' + T("界面") + '</div><div class="group">' +
      '<div class="g-row"><span class="g-label">' + T("外观") + '</span><span class="seg">' +
      ["auto", "light", "dark"].map(function (t) {
        return '<button type="button" class="' + (S.theme === t ? "on" : "") + '" data-theme-choice="' + t + '">' +
          T(({ auto: "自动", dark: "深色", light: "浅色" })[t]) + "</button>";
      }).join("") + "</span></div>" +
      '<div class="g-row"><span class="g-label">' + T("语言") + '</span><span class="seg">' +
      langs.map(function (l) {
        return '<button type="button" class="' + ((S.langPref || "") === l[0] ? "on" : "") + '" data-lang-choice="' + l[0] + '">' +
          esc(l[1]) + "</button>";
      }).join("") + "</span></div></div></div>";
    $("#main").innerHTML = '<div class="pad stack settings">' + devices + (noDevice() ? "" :
      '<div><div class="section-title">Agent</div><div class="group">' + agents + "</div></div>") +

      look + (noDevice() ? "" :
      '<div><div class="section-title">' + T("通用") + '</div><div class="group">' +
      '<label class="g-row"><span class="g-label">' + T("默认工作目录") + "</span>" +
      '<input id="set-cwd" class="g-input mono" value="' + esc(cfg.default_cwd || "") + '" spellcheck="false" autocomplete="off"></label>' +
      '<label class="g-row"><span class="g-label">' + T("最大并发任务") + "</span>" +
      '<input id="set-max" class="g-input num" type="number" inputmode="numeric" min="1" max="32" value="' + esc(String(cfg.max_concurrent || "")) + '"></label>' +
      '</div><div class="g-note">' + T("修改后自动保存。并发上限 1–32。") + "</div></div>" +

      '<div><div class="section-title">' + T("服务") + '</div><div class="group">' +
      '<div class="g-row"><span class="g-label">' + T("访问地址") + '</span><span class="g-value mono">' + esc(addr) + "</span></div>" +
      '<div class="g-row"><span class="g-label">' + T("版本") + '</span><span class="g-value">' + esc(d.version || "") + " · " + esc(T("已运行 %s", dur(d.started_at))) + "</span></div>" +
      '<details><summary><span class="g-label">' + T("运行信息") + '</span></summary><div class="g-body"><dl class="kv">' +
      "<dt>" + T("绑定") + "</dt><dd class=\"mono\">" + esc((cfg.bind_addrs || []).join("  ")) + "</dd>" +
      "<dt>PID</dt><dd>" + esc(String((d.status || {}).pid || "")) + "</dd>" +
      "<dt>" + T("数据目录") + "</dt><dd class=\"mono\">" + esc(cfg.data_dir || "") + "</dd>" +
      "<dt>" + T("数据库") + "</dt><dd class=\"mono\">" + esc(cfg.db_path || "") + "</dd>" +
      "<dt>" + T("日志目录") + "</dt><dd class=\"mono\">" + esc(cfg.log_dir || "") + "</dd>" +
      "<dt>" + T("允许根目录") + "</dt><dd class=\"mono\">" + esc((cfg.allowed_roots || []).join("  ")) + "</dd>" +
      "<dt>" + T("任务") + "</dt><dd>" + esc(countsSummary(counts)) + "</dd>" +
      "</dl></div></details>" +
      "</div></div>") +

      '<div class="about">Rymesh ' + esc(d.version || "") + " · " +
      T("Agent 在各自的服务器上运行，关闭页面或应用不会中断任务；需要审批或回答的问题会出现在对话里。") + "</div>" +
      "</div>";

    var agentLabels = {};
    (d.agents || []).forEach(function (a) { agentLabels[a.name] = a.label || a.name; });

    function agentUpdateBox(name) {
      var box = document.querySelector('[data-agent-update="' + name + '"]');
      var fold = box && box.closest("details");
      if (fold) fold.open = true;
      return box;
    }

    function renderAgentUpdate(u) {
      var box = agentUpdateBox(u.name);
      if (!box) return;
      if (u.error) {
        box.innerHTML = '<span class="error-text">' + esc(TR(u.error)) + "</span>";
        return;
      }
      var head = esc(T("当前 %s · 最新 %s", u.installed || T("未安装"), u.latest || "—"));
      if (u.available) {
        box.innerHTML = head + ' · <span style="color:var(--amber)">' + T("有更新") + "</span>" +
          '<div style="margin-top:8px"><button type="button" class="btn primary sm" data-agent-apply="' +
          esc(u.name) + '">' + esc(u.installed ? T("更新到 %s", u.latest || "") : T("安装 %s", u.latest || "")) + "</button></div>" +
          (u.command ? '<div class="mono agent-cmd">' + esc(u.command) + "</div>" : "");
        var b = box.querySelector("[data-agent-apply]");
        if (b) b.addEventListener("click", function () { applyAgentUpdate(u, b); });
      } else {
        box.innerHTML = head + " · " + T("已是最新") +
          (u.command ? '<div class="mono agent-cmd">' + esc(u.command) + "</div>" : "");
      }
    }

    function applyAgentUpdate(u, btn) {
      var label = agentLabels[u.name] || u.name;
      var cmd = u.command || "";
      confirmSheet(T("更新 %s？", label),
        "<p>" + T("将执行：") + '</p><pre class="mono agent-cmd">' + esc(cmd) + "</pre>" +
        "<p>" + T("安装期间不会中断正在运行的任务；新任务会使用新版本。") + "</p>", T("更新"))
        .then(function (ok) {
          if (!ok) return;
          btn.disabled = true; btn.textContent = T("更新中…");
          var box = agentUpdateBox(u.name);
          if (box) box.innerHTML = "<span>" + esc(T("正在执行 %s，请稍候…", cmd)) + "</span>";
          api("/api/agents/" + encodeURIComponent(u.name) + "/update", { method: "POST", body: {} })
            .then(function (r) {
              var v = (r.agent && r.agent.version) || r.version || "";
              toast(v ? T("已更新：%s", v) : T("更新完成"));
              renderSettings();
            })
            .catch(function (e) {
              toast(T("更新失败"), true);
              if (box) box.innerHTML = '<span class="error-text">' + esc(e.message) + "</span>";
              btn.disabled = false; btn.textContent = T("重试更新");
            });
        });
    }

    function restartAgent(name, btn) {
      var label = agentLabels[name] || name;
      btn.disabled = true;
      api("/api/tasks?status=active").then(function (r) {
        var mine = (r.tasks || []).filter(function (t) { return t.agent === name; });
        var running = mine.filter(function (t) { return t.status !== "pending"; });
        var queued = mine.length - running.length;
        var html;
        if (!running.length) {
          html = "<p>" + T("%s 当前没有正在运行的任务。", esc(label)) + "</p>" +
            "<p>" + T("点击后会重新检测版本、认证与模型。新消息本来就会读取最新的 config/settings，无需重启。") + "</p>";
        } else {
          html = "<p>" + T("将结束并用相同内容重新运行 <b>%d</b> 个正在运行或等待审批的任务，新进程会读取最新的 config/settings。已经执行过的步骤可能重复执行。", running.length) + "</p>" +
            (queued ? "<p>" + T("另有 %d 个排队任务不会被中断，它们启动时本就会使用新配置。", queued) + "</p>" : "");
        }
        return confirmSheet(T("重启 %s？", label), html, T("重启")).then(function (ok) {
          if (!ok) return;
          btn.disabled = true; btn.textContent = T("重启中…");
          return api("/api/agents/" + encodeURIComponent(name) + "/restart", { method: "POST", body: {} })
            .then(function (res) {
              if (res.restarted) toast(T("正在重启 %d 个任务…", res.restarted));
              else toast(T("已重新检测 %s 配置", label));
              renderSettings();
            });
        });
      }).catch(function (e) {
        toast(e.message, true);
      }).then(function () {
        btn.disabled = false; btn.textContent = T("重启 Agent");
      });
    }

    Array.prototype.forEach.call(document.querySelectorAll("[data-agent-check]"), function (btn) {
      btn.addEventListener("click", function () {
        var name = btn.getAttribute("data-agent-check");
        var box = agentUpdateBox(name);
        btn.disabled = true; btn.textContent = T("检查中…");
        if (box) box.innerHTML = "<span>" + T("正在向 npm 查询最新版本…") + "</span>";
        api("/api/agents/updates").then(function (r) {
          var u = (r.updates || []).filter(function (x) { return x.name === name; })[0];
          if (u) renderAgentUpdate(u);
          else if (box) box.innerHTML = T("未返回该 Agent 的版本信息");
        }).catch(function (e) {
          if (box) box.innerHTML = '<span class="error-text">' + esc(e.message) + "</span>";
        }).then(function () {
          btn.disabled = false; btn.textContent = T("检查更新");
        });
      });
    });

    Array.prototype.forEach.call(document.querySelectorAll("[data-agent-restart]"), function (btn) {
      btn.addEventListener("click", function () {
        restartAgent(btn.getAttribute("data-agent-restart"), btn);
      });
    });

    // Settings save on change; each field is sent alone so one bad value does
    // not block the other.
    function saveSetting(body, input) {
      api("/api/settings", { method: "PATCH", body: body }).then(function (r) {
        var c = r.config || {};
        if (c.default_cwd !== undefined) S.config.default_cwd = c.default_cwd;
        if (c.max_concurrent !== undefined) S.config.max_concurrent = c.max_concurrent;
        toast(T("已保存"));
      }).catch(function (e) {
        toast(e.message, true);
        input.value = input.name === "max" ? S.config.max_concurrent : S.config.default_cwd;
      });
    }
    if ($("#set-cwd")) {
    $("#set-cwd").name = "cwd";
    $("#set-max").name = "max";
    $("#set-cwd").addEventListener("change", function () {
      var v = this.value.trim();
      if (!v || v === S.config.default_cwd) { this.value = S.config.default_cwd || ""; return; }
      saveSetting({ default_cwd: v }, this);
    });
    $("#set-max").addEventListener("change", function () {
      var n = parseInt(this.value, 10);
      if (!n || n === S.config.max_concurrent) { this.value = S.config.max_concurrent; return; }
      saveSetting({ max_concurrent: n }, this);
    });
    [$("#set-cwd"), $("#set-max")].forEach(function (el) {
      el.addEventListener("keydown", function (e) { if (e.key === "Enter") this.blur(); });
    });
    }
  }

  // A restart is detected by a new started_at; an update additionally waits for
  // the new binary's build hash, so the page never reloads into the old process
  // (the old one answers for a moment before systemd stops it).
  // ------------------------------------------------------------- top actions

  $("#menu-btn").addEventListener("click", function () {
    if (S.route === "settings") leaveSettings();
    else openSidebar();
  });

  // Settings sit at the foot of the sidebar; on a phone the sidebar is an
  // overlay and goes away with the choice.
  $("#settings-btn").addEventListener("click", function () {
    closeSidebar();
    if (S.route === "settings") {
      leaveSettings();
    } else {
      S.settingsReturn = location.hash || "#/";
      go("#/settings");
    }
  });

  $("#approval-btn").addEventListener("click", function () {
    if ($("#approval-stack").hidden) showApprovalStack();
    else hideApprovalStack();
  });

  $("#agent-btn").addEventListener("click", function (e) {
    e.stopPropagation();
    if ($("#agent-menu").hidden) openAgentMenu(); else closeAgentMenu();
  });
  $("#agent-menu").addEventListener("click", function (e) {
    var it = e.target.closest("[data-agent]");
    if (!it || it.disabled) return;
    var sel = $("#agent-select");
    closeAgentMenu();
    if (sel.value === it.getAttribute("data-agent")) return;
    sel.value = it.getAttribute("data-agent");
    sel.dispatchEvent(new Event("change"));
  });

  $("#agent-select").addEventListener("change", function () {
    var name = this.value;
    if (!name) return;
    var a = agentMeta(name);
    S.draftAgent = name;
    LS.set("draftAgent", name);
    S.draftSettings = {}; // models differ per agent
    if (S.route !== "home") toast(T("已选择 %s，开始新对话", a.label || name));
    newChat(name);
  });

  $("#sidebar-new").addEventListener("click", function () { newChat(); });

  // Inside a conversation the top bar offers a new one with the same agent,
  // in the same directory.
  $("#new-chat-btn").addEventListener("click", function () {
    if (S.session) S.draftCwd = S.session.cwd;
    newChat(S.session ? S.session.agent : "");
  });
  $("#session-search").addEventListener("input", function () {
    S.sessionQuery = this.value;
    renderSessionList();
  });
  $("#sidebar").addEventListener("click", function (e) {
    if (e.target.closest("[data-sidebar-close]")) closeSidebar();
  });

  document.addEventListener("click", function (e) {
    var n = e.target.closest("[data-approval-sid]");
    if (!n) return;
    var sid = n.getAttribute("data-approval-sid");
    var pid = n.getAttribute("data-approval-pid");
    var node = n.getAttribute("data-approval-node");
    S.focusPermission = pid;
    hideApprovalStack();
    if (node && node !== S.node) {
      closeSidebar();
      switchNode(node, "#/session/" + encodeURIComponent(sid));
      return;
    }
    closeSheet();
    closeSidebar();
    if (S.session && S.session.id === sid && S.route === "chat") {
      ensurePermissionCards();
      if (!focusPermissionCard(pid)) toast(T("审批请求已失效"));
    } else go("#/session/" + encodeURIComponent(sid));
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    if (!$("#profile-menu").hidden) closeProfileMenu();
    else if (closeAgentFilterMenu()) return;
    else if (!$("#sheet").hidden) closeSheet();
    else if (S.sidebarOpen) closeSidebar();
  });

  // The macOS app opens the card a notification is about.
  window.acOpen = function (node, sid, pid) {
    if (!S.booted) return;
    S.focusPermission = pid || "";
    closeSheet();
    closeSidebar();
    var hash = "#/session/" + encodeURIComponent(sid);
    if (node && node !== S.node) switchNode(node, hash);
    else if (S.session && S.session.id === sid && S.route === "chat") {
      ensurePermissionCards();
      if (pid) focusPermissionCard(pid);
    } else go(hash);
  };

  // "按此计划执行" on a Codex plan: run it as a new turn outside plan mode.
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-run-plan]");
    if (!b || !S.session) return;
    if (currentAgentName() === "codex") setProfile("workspace-write");
    $("#prompt").value = T("请按上面的计划执行。");
    b.disabled = true;
    sendMessage();
  });

  // ----------------------------------------------------------------- terminal
  // A shell on the conversation's machine, in its directory (terminal.go).
  // That machine's console runs it, so a server's terminal comes through the
  // same /n/{node} relay as the rest of the page. The shell outlives the view:
  // going back, or leaving the app, keeps it, and reopening picks up at the
  // last byte shown. On touch screens a key row sits above the keyboard.

  var TERM = { xterm: null, fit: null, key: "", node: "", id: "", offset: 0, es: null,
    ended: false, ctrl: false, alt: false, fn: false, queue: "", sending: false };
  // Both rows start with the same keys, so Fn stays put when it flips them.
  var TERM_HEAD_KEYS = [
    ["Esc", "\x1b"], ["Tab", "\t"], ["Ctrl", "mod:ctrl"], ["Alt", "mod:alt"], ["Fn", "fn"]
  ];
  var TERM_KEYS = TERM_HEAD_KEYS.concat([
    ["↑", "arrow:A"], ["↓", "arrow:B"], ["←", "arrow:D"], ["→", "arrow:C"],
    ["|", "|"], ["/", "/"], ["~", "~"], ["-", "-"]
  ]);
  var TERM_FN_KEYS = TERM_HEAD_KEYS.concat([
    ["F1", "\x1bOP"], ["F2", "\x1bOQ"], ["F3", "\x1bOR"], ["F4", "\x1bOS"],
    ["F5", "\x1b[15~"], ["F6", "\x1b[17~"], ["F7", "\x1b[18~"], ["F8", "\x1b[19~"],
    ["F9", "\x1b[20~"], ["F10", "\x1b[21~"], ["F11", "\x1b[23~"], ["F12", "\x1b[24~"],
    ["Home", "\x1b[H"], ["End", "\x1b[F"], ["PgUp", "\x1b[5~"], ["PgDn", "\x1b[6~"]
  ]);

  var xtermLoad = null;
  function loadXterm() {
    if (xtermLoad) return xtermLoad;
    function script(src) {
      return new Promise(function (ok, fail) {
        var s = document.createElement("script");
        s.src = src;
        s.onload = ok;
        s.onerror = function () { fail(new Error(T("无法加载终端组件"))); };
        document.head.appendChild(s);
      });
    }
    var css = document.createElement("link");
    css.rel = "stylesheet";
    css.href = "/vendor/xterm/xterm.css";
    document.head.appendChild(css);
    xtermLoad = script("/vendor/xterm/xterm.js")
      .then(function () { return script("/vendor/xterm/addon-fit.js"); })
      .catch(function (e) { xtermLoad = null; throw e; });
    return xtermLoad;
  }

  function termTheme() {
    var cs = getComputedStyle(document.documentElement);
    var v = function (n) { return cs.getPropertyValue(n).trim(); };
    var dark = cs.colorScheme === "dark" || v("color-scheme") === "dark";
    var t = { background: v("--bg"), foreground: v("--text"), cursor: v("--text"),
      cursorAccent: v("--bg"), selectionBackground: v("--blue") + "55" };
    // The stock palette is tuned for a dark screen; on paper its white and
    // yellow all but vanish.
    if (!dark) {
      t.black = "#1a1a19"; t.red = "#c43c3c"; t.green = "#2f8f5b"; t.yellow = "#a66a12";
      t.blue = "#3d6fd1"; t.magenta = "#8b4fc4"; t.cyan = "#1f8a8f"; t.white = "#6b6b66";
      t.brightBlack = "#8c8c87"; t.brightWhite = "#1a1a19";
    }
    return t;
  }

  function termOpen() { return !$("#term").hidden; }

  function openTerminal() {
    if (!S.session || noDevice()) return;
    var key = "term:" + S.node + ":" + S.session.id;
    var cwd = S.session.cwd;
    $("#term").hidden = false;
    $("#term-sub").textContent = (nodeLabel(S.node) ? nodeLabel(S.node) + " · " : "") + cwd;
    drawTermKeys();
    loadXterm().then(function () {
      if (!termOpen()) return;
      if (!TERM.xterm) createXterm();
      TERM.xterm.options.theme = termTheme();
      fitTerm();
      if (TERM.key !== key || TERM.ended) {
        detachTerm();
        TERM.xterm.reset();
        TERM.key = key;
        TERM.node = S.node;
        TERM.cwd = cwd;
        var id = LS.get(key, "");
        if (id) attachTerm(id); else newTerm();
      }
      TERM.xterm.focus();
    }).catch(function (e) {
      toast(e.message, true);
      closeTerminalView();
    });
  }

  function closeTerminalView() {
    var view = $("#term");
    if (!view || view.hidden) return;
    view.hidden = true;
    if (TERM.xterm) TERM.xterm.blur();
  }

  function createXterm() {
    var coarse = matchMedia("(pointer: coarse)").matches;
    TERM.xterm = new window.Terminal({
      cursorBlink: true, scrollback: 5000, macOptionIsMeta: true,
      fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono").trim(),
      fontSize: coarse ? 12 : 13, lineHeight: 1.15, theme: termTheme()
    });
    TERM.fit = new window.FitAddon.FitAddon();
    TERM.xterm.loadAddon(TERM.fit);
    TERM.xterm.open($("#term-screen"));
    TERM.xterm.onData(function (d) {
      if (TERM.ended) { reopenTerm(); return; }
      termSend(applyTermMods(d));
    });
    TERM.xterm.onResize(function (sz) {
      if (TERM.id && !TERM.ended) {
        api("/api/terminals/" + TERM.id + "/resize", { method: "POST", node: TERM.node,
          body: { cols: sz.cols, rows: sz.rows } }).catch(function () {});
      }
    });
    var refit = null;
    function onViewport() {
      clearTimeout(refit);
      refit = setTimeout(function () { if (termOpen()) fitTerm(); }, 80);
    }
    window.addEventListener("resize", onViewport);
    if (window.visualViewport) window.visualViewport.addEventListener("resize", onViewport);
  }

  function fitTerm() {
    try { TERM.fit.fit(); } catch (e) { /* not laid out yet */ }
  }

  function newTerm() {
    var key = TERM.key;
    var sz = TERM.xterm;
    return api("/api/terminals", { method: "POST", node: TERM.node,
      body: { cwd: TERM.cwd, cols: sz.cols, rows: sz.rows } }).then(function (d) {
      if (TERM.key !== key) return;
      LS.set(key, d.id);
      attachTerm(d.id);
    }).catch(function (e) {
      if (TERM.key !== key) return;
      TERM.ended = true;
      TERM.xterm.write("\r\n\x1b[31m" + T("无法打开终端：%s", e.message) + "\x1b[0m\r\n");
    });
  }

  function reopenTerm() {
    LS.del(TERM.key);
    detachTerm();
    TERM.xterm.write("\r\n");
    newTerm();
  }

  function attachTerm(id) {
    TERM.id = id;
    TERM.offset = 0;
    TERM.ended = false;
    connectTerm();
  }

  function detachTerm() {
    if (TERM.es) { TERM.es.close(); TERM.es = null; }
    TERM.id = "";
    TERM.queue = "";
  }

  function connectTerm() {
    var id = TERM.id;
    var es = new EventSource(nodePath("/api/terminals/" + id + "/stream?after=" + TERM.offset, TERM.node));
    TERM.es = es;
    es.addEventListener("out", function (ev) {
      if (TERM.es !== es) return;
      var d = JSON.parse(ev.data);
      var bin = atob(d.d);
      var bytes = new Uint8Array(bin.length);
      for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      TERM.xterm.write(bytes);
      TERM.offset = d.at + bytes.length;
    });
    es.addEventListener("exit", function () {
      if (TERM.es !== es) return;
      es.close();
      TERM.es = null;
      TERM.ended = true;
      LS.del(TERM.key);
      TERM.xterm.write("\r\n\x1b[2m" + T("[已退出 · 按任意键重新打开]") + "\x1b[0m\r\n");
    });
    // EventSource cannot see a 404 and would retry a finished terminal
    // forever. Reconnect by hand, after asking whether it still exists (a
    // resize answers 404 once it is gone).
    es.onerror = function () {
      if (TERM.es !== es) return;
      es.close();
      TERM.es = null;
      setTimeout(function () {
        if (TERM.id !== id || TERM.ended) return;
        api("/api/terminals/" + id + "/resize", { method: "POST", node: TERM.node,
          body: { cols: TERM.xterm.cols, rows: TERM.xterm.rows } }).then(function () {
          if (TERM.id === id && !TERM.es) connectTerm();
        }).catch(function (e) {
          if (TERM.id !== id) return;
          if (e.status === 404) {
            TERM.xterm.write("\r\n\x1b[2m" + T("[终端已结束，已重新打开]") + "\x1b[0m\r\n");
            reopenTerm();
          } else {
            setTimeout(function () { if (TERM.id === id && !TERM.es) connectTerm(); }, 2000);
          }
        });
      }, 1200);
    };
  }

  // Keystrokes go out in order, one request at a time; what is typed while a
  // request is in flight joins the next one.
  function termSend(data) {
    TERM.queue += data || "";
    if (!TERM.queue || TERM.sending || !TERM.id) return;
    var id = TERM.id, chunk = TERM.queue;
    TERM.queue = "";
    TERM.sending = true;
    api("/api/terminals/" + id + "/input", { method: "POST", node: TERM.node, body: { data: chunk } })
      .catch(function (e) { if (e.status === 404 || e.status === 410) toast(T("终端已结束"), true); })
      .then(function () {
        TERM.sending = false;
        if (TERM.queue) termSend("");
      });
  }

  // The sticky Ctrl and Alt keys of the key row act on the next keystroke.
  function applyTermMods(d) {
    if (!TERM.ctrl && !TERM.alt) return d;
    if (TERM.ctrl && d.length) {
      var c = d[0].toUpperCase().charCodeAt(0);
      var ctl = c >= 64 && c <= 95 ? String.fromCharCode(c - 64) : d[0] === " " ? "\x00" : d[0] === "?" ? "\x7f" : d[0];
      d = ctl + d.slice(1);
    }
    if (TERM.alt) d = "\x1b" + d;
    TERM.ctrl = TERM.alt = false;
    drawTermKeys();
    return d;
  }

  function drawTermKeys() {
    var keys = TERM.fn ? TERM_FN_KEYS : TERM_KEYS;
    $("#term-keys").innerHTML = keys.map(function (k) {
      var on = (k[1] === "mod:ctrl" && TERM.ctrl) || (k[1] === "mod:alt" && TERM.alt) || (k[1] === "fn" && TERM.fn);
      return '<button type="button" tabindex="-1" data-k="' + esc(k[1]) + '"' + (on ? ' class="on"' : "") + ">" + esc(k[0]) + "</button>";
    }).join("");
  }

  function pressTermKey(k) {
    if (k === "fn") { TERM.fn = !TERM.fn; drawTermKeys(); return; }
    if (k === "mod:ctrl") { TERM.ctrl = !TERM.ctrl; drawTermKeys(); return; }
    if (k === "mod:alt") { TERM.alt = !TERM.alt; drawTermKeys(); return; }
    if (!TERM.xterm) return;
    if (TERM.ended) { reopenTerm(); return; }
    var seq = k;
    if (k.indexOf("arrow:") === 0) {
      var dir = k.slice(6);
      var mod = TERM.ctrl ? 5 : TERM.alt ? 3 : 0;
      seq = mod ? "\x1b[1;" + mod + dir : (TERM.xterm.modes.applicationCursorKeysMode ? "\x1bO" : "\x1b[") + dir;
      if (mod) { TERM.ctrl = TERM.alt = false; drawTermKeys(); }
      termSend(seq);
      return;
    }
    termSend(applyTermMods(seq));
  }

  // The key row must not take focus from the terminal, or the keyboard would
  // close: act on mousedown / on a touch that did not scroll the row, and
  // cancel the default that would move focus.
  (function () {
    var bar = $("#term-keys"), start = null;
    bar.addEventListener("mousedown", function (e) {
      var b = e.target.closest("[data-k]");
      if (!b) return;
      e.preventDefault();
      if (!e.sourceCapabilities || !e.sourceCapabilities.firesTouchEvents) pressTermKey(b.getAttribute("data-k"));
    });
    bar.addEventListener("touchstart", function (e) {
      var t = e.touches[0];
      start = { x: t.clientX, y: t.clientY, b: e.target.closest("[data-k]") };
    }, { passive: true });
    bar.addEventListener("touchend", function (e) {
      var t = e.changedTouches[0];
      if (!start || !start.b) return;
      var moved = Math.abs(t.clientX - start.x) > 10 || Math.abs(t.clientY - start.y) > 10;
      if (!moved) {
        e.preventDefault();
        pressTermKey(start.b.getAttribute("data-k"));
      }
      start = null;
    });
  })();

  $("#term-btn").addEventListener("click", openTerminal);
  $("#term-back").addEventListener("click", closeTerminalView);
  $("#term-end").addEventListener("click", function () {
    var id = TERM.id, node = TERM.node, key = TERM.key;
    if (key) LS.del(key);
    detachTerm();
    TERM.key = "";
    closeTerminalView();
    if (id) api("/api/terminals/" + id, { method: "DELETE", node: node }).catch(function () {});
    toast(T("终端已结束"));
  });

  // -------------------------------------------------------------------- files
  // The conversation machine's files (files.go), starting in its directory.
  // A tap on a folder opens it, a tap on a file downloads it: the apps write
  // it into Downloads themselves (a WebView cannot), a browser saves it as
  // usual. Uploads go into the folder on screen.

  var FILES = { node: "", path: "", parent: "", entries: [], truncated: false, token: 0,
    hidden: LS.get("files.hidden", "") === "1", jobs: [] };

  function openFiles() {
    if (!S.session || noDevice()) return;
    var start = FILES.node === S.node && FILES.session === S.session.id && FILES.path ? FILES.path : S.session.cwd;
    setPickMode(false);
    FILES.node = S.node;
    FILES.session = S.session.id;
    $("#files").hidden = false;
    $("#files-hidden").setAttribute("aria-pressed", FILES.hidden ? "true" : "false");
    loadDir(start);
  }

  function closeFilesView() {
    var view = $("#files");
    if (view && !view.hidden) view.hidden = true;
  }

  // The same view picks the working directory of a new conversation: only
  // folders can be entered, and the bar at the bottom takes the one on screen.
  function setPickMode(on) {
    FILES.pick = on;
    $("#files").classList.toggle("picking", on);
    $("#files-pick").hidden = !on;
    $("#files-title").textContent = on ? T("选择工作目录") : T("文件");
  }

  function openFolderPicker(start) {
    setPickMode(true);
    FILES.node = S.node;
    FILES.session = "";
    $("#files").hidden = false;
    $("#files-hidden").setAttribute("aria-pressed", FILES.hidden ? "true" : "false");
    loadDir(start);
  }

  function pickFolder() {
    var path = FILES.path;
    if (!path) return;
    var btn = $("#files-pick-ok");
    btn.disabled = true;
    api("/api/cwd/check", { method: "POST", node: FILES.node, body: { path: path } }).then(function (d) {
      if (!d.ok) { toast(d.error, true); return; }
      S.draftCwd = d.cwd;
      LS.set("draftCwd", d.cwd);
      closeFilesView();
      if (S.route === "home") renderHome();
      else updateComposerMeta();
      toast(T("新对话将在 %s 中运行", d.cwd.length > 40 ? "…" + d.cwd.slice(-39) : d.cwd));
      if (d.warning) toast(d.warning, true);
    }).catch(function (e) { toast(e.message, true); })
      .then(function () { btn.disabled = false; });
  }

  // shortPath shows the home folder as "~" and keeps the end of a long path,
  // where the folder that matters is.
  function shortPath(p, max) {
    var h = S.config.home;
    if (h && h !== "/" && (p === h || p.indexOf(h + "/") === 0)) p = "~" + p.slice(h.length);
    return p.length > max ? "…" + p.slice(-(max - 1)) : p;
  }

  function joinPath(dir, name) { return dir === "/" ? "/" + name : dir + "/" + name; }

  function fmtSize(n) {
    if (n < 1024) return n + " B";
    var u = ["KB", "MB", "GB", "TB"], i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
    return (n < 10 ? n.toFixed(1) : Math.round(n)) + " " + u[i];
  }

  function loadDir(path) {
    var tok = ++FILES.token;
    $("#files-list").innerHTML = '<div class="empty"><span class="spinner"></span></div>';
    api("/api/files?path=" + encodeURIComponent(path), { node: FILES.node }).then(function (d) {
      if (tok !== FILES.token) return;
      FILES.path = d.path;
      FILES.parent = d.parent;
      FILES.entries = d.entries || [];
      FILES.truncated = d.truncated;
      renderFiles();
      $("#files-list").scrollTop = 0;
    }).catch(function (e) {
      if (tok !== FILES.token) return;
      $("#files-sub").textContent = path;
      $("#files-list").innerHTML = '<div class="empty">' + esc(T("无法打开：%s", e.message)) +
        (FILES.path && FILES.path !== path ? '<div><button type="button" class="btn sm" data-dir="' + esc(FILES.path) + '">' + T("返回") + "</button></div>" : "") + "</div>";
    });
  }

  function renderFiles() {
    $("#files-sub").textContent = (nodeLabel(FILES.node) ? nodeLabel(FILES.node) + " · " : "") + FILES.path;
    // The path as tappable segments, scrolled to its end.
    var parts = FILES.path.split("/").filter(Boolean), acc = "";
    var crumbs = ['<button type="button" data-dir="/">/</button>'];
    parts.forEach(function (p) {
      acc += "/" + p;
      crumbs.push('<button type="button" data-dir="' + esc(acc) + '">' + esc(p) + "</button>");
    });
    var bar = $("#files-crumbs");
    bar.innerHTML = crumbs.join('<span class="sep">›</span>');
    bar.scrollLeft = bar.scrollWidth;

    var shown = FILES.entries.filter(function (e) { return FILES.hidden || e.name.charAt(0) !== "."; });
    var rows = [];
    if (FILES.parent) {
      rows.push('<button type="button" class="file-row up" data-dir="' + esc(FILES.parent) + '">' +
        '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M9 14 4.5 9.5 9 5M5 9.5h9a5.5 5.5 0 0 1 0 11h-2"/></svg>' +
        '<span class="file-name">' + T("上一级") + "</span></button>");
    }
    shown.forEach(function (e) {
      var full = joinPath(FILES.path, e.name);
      if (FILES.pick && !e.dir) {
        rows.push('<div class="file-row inert"><svg class="ico" viewBox="0 0 24 24" aria-hidden="true">' +
          '<path d="M7 3.5h6.5L18 8v12.5H7z"/><path d="M13 3.5V8h5"/></svg>' +
          '<span class="file-text"><span class="file-name">' + esc(e.name) + "</span></span></div>");
        return;
      }
      var ico = e.dir
        ? '<path d="M3.5 7.5a2 2 0 0 1 2-2h4l2 2h7a2 2 0 0 1 2 2v7.5a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z"/>'
        : '<path d="M7 3.5h6.5L18 8v12.5H7z"/><path d="M13 3.5V8h5"/>';
      var meta = (e.dir ? "" : fmtSize(e.size) + " · ") + rel(e.mtime) + (e.link ? " · " + T("链接") : "");
      rows.push('<button type="button" class="file-row' + (e.dir ? " dir" : "") + '" data-entry="' + esc(full) + '" data-name="' + esc(e.name) + '" ' +
        (e.dir ? 'data-dir="' + esc(full) + '"' : 'data-file="' + esc(full) + '" data-size="' + e.size + '"') + ">" +
        '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true">' + ico + "</svg>" +
        '<span class="file-text"><span class="file-name">' + esc(e.name) + '</span><span class="file-meta">' + esc(meta) + "</span></span>" +
        '<svg class="ico sm file-more" viewBox="0 0 24 24" aria-hidden="true"><circle cx="6" cy="12" r="1.2"/><circle cx="12" cy="12" r="1.2"/><circle cx="18" cy="12" r="1.2"/></svg>' +
        "</button>");
    });
    if (!shown.length) {
      var hiddenN = FILES.entries.length - shown.length;
      rows.push('<div class="empty">' + (hiddenN ? T("只有 %d 个隐藏项", hiddenN) : T("空文件夹")) + "</div>");
    }
    if (FILES.truncated) rows.push('<div class="empty">' + T("只显示了前 5000 项") + "</div>");
    $("#files-list").innerHTML = rows.join("");
    $("#files-pick-path").textContent = FILES.path;
  }

  function fileJob(kind, name, total) {
    var job = { id: rid(), kind: kind, name: name, done: 0, total: total || 0, state: "", info: "" };
    FILES.jobs.unshift(job);
    FILES.jobs = FILES.jobs.slice(0, 6);
    renderJobs();
    return job;
  }

  function renderJobs() {
    var box = $("#files-jobs");
    box.innerHTML = FILES.jobs.map(function (j) {
      var pct = j.total > 0 ? Math.min(100, Math.round(j.done / j.total * 100)) : 0;
      var up = j.kind === "up";
      var status = j.state === "done"
        ? (up ? T("已上传") : j.info ? T("已保存到 %s", TR(j.info)) : T("已保存"))
        : j.state === "error" ? (up ? T("上传失败：%s", TR(j.info)) : T("下载失败：%s", TR(j.info)))
        : j.state === "browser" ? T("已交给浏览器下载")
        : (j.total > 0 ? pct + "% · " + fmtSize(j.done) + " / " + fmtSize(j.total) : (up ? T("上传中…") : T("下载中…")) + " " + (j.done ? fmtSize(j.done) : ""));
      var open = j.kind === "down" && j.state === "done" && (androidNative || macNative)
        ? '<button type="button" class="btn sm" data-open-dl="' + esc(j.id) + '">' + (macNative ? T("在访达中显示") : T("打开")) + "</button>" : "";
      return '<div class="file-job ' + esc(j.state || "run") + '">' +
        '<div class="file-job-text"><span class="file-name">' + esc(j.name) + '</span><span class="file-meta">' + esc(status) + "</span></div>" +
        open + (j.state ? "" : '<div class="file-job-bar"><i style="width:' + pct + '%"></i></div>') + "</div>";
    }).join("");
  }

  // Native downloads report here (MainActivity / Download.swift).
  window.acDownload = function (id, done, total, state, info) {
    var j = FILES.jobs.filter(function (x) { return x.id === id; })[0];
    if (!j) return;
    j.done = done;
    if (total > 0) j.total = total;
    if (state) { j.state = state; j.info = info || ""; }
    renderJobs();
    if (state === "error") toast(T("%s 下载失败", j.name), true);
  };

  function downloadFile(path, name, size) {
    var url = nodePath("/api/files/download?path=" + encodeURIComponent(path), FILES.node);
    var job = fileJob("down", name, size);
    if (androidNative && androidNative.download) {
      androidNative.download(job.id, url, name);
    } else if (macNative) {
      macNative.postMessage({ op: "download", id: job.id, path: url, name: name });
    } else {
      var a = document.createElement("a");
      a.href = url;
      a.download = name;
      document.body.appendChild(a);
      a.click();
      a.remove();
      job.state = "browser";
      renderJobs();
    }
  }

  // XMLHttpRequest rather than fetch: it reports upload progress.
  function uploadFile(file) {
    var dir = FILES.path, node = FILES.node;
    var job = fileJob("up", file.name, file.size);
    var xhr = new XMLHttpRequest();
    xhr.open("POST", nodePath("/api/files/upload?dir=" + encodeURIComponent(dir) + "&name=" + encodeURIComponent(file.name), node));
    xhr.setRequestHeader("X-CSRF-Token", S.csrf);
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.upload.onprogress = function (e) {
      job.done = e.loaded;
      if (e.lengthComputable) job.total = e.total;
      renderJobs();
    };
    xhr.onload = function () {
      var d = {};
      try { d = JSON.parse(xhr.responseText); } catch (e) {}
      if (xhr.status === 200) {
        job.state = "done";
        job.done = job.total;
        if (d.name && d.name !== file.name) job.name = file.name + " → " + d.name;
        if (FILES.path === dir && FILES.node === node && !$("#files").hidden) loadDir(dir);
      } else {
        job.state = "error";
        job.info = d.error || "HTTP " + xhr.status;
        if (xhr.status === 401) showLogin();
      }
      renderJobs();
    };
    xhr.onerror = function () { job.state = "error"; job.info = T("连接中断"); renderJobs(); };
    xhr.send(file);
  }

  $("#files-btn").addEventListener("click", openFiles);
  $("#files-pick-ok").addEventListener("click", pickFolder);
  $("#files-pick-type").addEventListener("click", function () {
    closeFilesView();
    cwdInputSheet();
  });
  $("#files-back").addEventListener("click", closeFilesView);
  $("#files-hidden").addEventListener("click", function () {
    FILES.hidden = !FILES.hidden;
    LS.set("files.hidden", FILES.hidden ? "1" : "");
    this.setAttribute("aria-pressed", FILES.hidden ? "true" : "false");
    renderFiles();
  });
  $("#files-input").addEventListener("change", function () {
    Array.prototype.forEach.call(this.files || [], uploadFile);
    this.value = "";
  });
  // What can be done to one file or folder. Rename and delete each ask once
  // more before acting.
  function fileMenu(row) {
    if (FILES.pick) return;
    var path = row.getAttribute("data-entry"), name = row.getAttribute("data-name");
    var isDir = row.hasAttribute("data-dir");
    var items = isDir
      ? [{ label: T("打开"), value: "open" }]
      : [{ label: T("下载"), value: "download", sub: fmtSize(+row.getAttribute("data-size")) }];
    items.push({ label: T("重命名"), value: "rename" }, { label: T("删除"), value: "delete", danger: true });
    pickList(name, items).then(function (a) {
      if (a === "open") loadDir(path);
      else if (a === "download") downloadFile(path, name, +row.getAttribute("data-size"));
      else if (a === "rename") renameFile(path, name);
      else if (a === "delete") deleteFile(path, name, isDir);
    });
  }

  function renameFile(path, name) {
    var dir = FILES.path, node = FILES.node;
    askText(T("重命名"), { label: T("新名称"), value: name, ok: T("确认重命名") }).then(function (v) {
      v = (v || "").trim();
      if (!v || v === name) return;
      api("/api/files/rename", { method: "POST", node: FILES.node, body: { path: path, name: v } }).then(function () {
        toast(T("已重命名为 %s", v));
        if (FILES.path === dir && FILES.node === node) loadDir(dir);
      }).catch(function (e) { toast(e.message, true); });
    });
  }

  function deleteFile(path, name, isDir) {
    var dir = FILES.path, node = FILES.node;
    confirmSheet(T("删除「%s」？", truncate(name, 40)),
      "<p>" + (isDir ? T("文件夹和其中的所有内容都会被删除，删除后无法恢复。") : T("删除后无法恢复。")) + "</p>" +
      '<p class="small muted mono" style="word-break:break-all">' + esc(path) + "</p>",
      T("删除"), true).then(function (ok) {
      if (!ok) return;
      api("/api/files/delete", { method: "POST", node: FILES.node, body: { path: path } }).then(function () {
        toast(T("已删除 %s", name));
        if (FILES.path === dir && FILES.node === node) loadDir(dir);
      }).catch(function (e) { toast(e.message, true); });
    });
  }

  // Touch: long-press any row for its menu. Desktop: right-click.
  (function () {
    var list = $("#files-list"), timer = null, fired = false, x0 = 0, y0 = 0;
    function cancel() { clearTimeout(timer); timer = null; }
    list.addEventListener("touchstart", function (e) {
      var row = e.target.closest("[data-entry]");
      if (!row) return;
      fired = false; x0 = e.touches[0].clientX; y0 = e.touches[0].clientY;
      timer = setTimeout(function () {
        fired = true;
        if (navigator.vibrate) navigator.vibrate(10);
        fileMenu(row);
      }, 480);
    }, { passive: true });
    list.addEventListener("touchmove", function (e) {
      var t = e.touches[0];
      if (Math.abs(t.clientX - x0) > 8 || Math.abs(t.clientY - y0) > 8) cancel();
    }, { passive: true });
    // As in the sidebar: the click after a long press must not reach the
    // menu now under the finger, nor open the folder.
    list.addEventListener("touchend", function (e) {
      cancel();
      if (fired) e.preventDefault();
    });
    list.addEventListener("touchcancel", cancel);
    list.addEventListener("click", function (e) {
      if (fired) { fired = false; e.stopImmediatePropagation(); e.preventDefault(); }
    }, true);
    list.addEventListener("contextmenu", function (e) {
      var row = e.target.closest("[data-entry]");
      if (!row) return;
      e.preventDefault();
      fileMenu(row);
    });
  })();

  $("#files").addEventListener("click", function (e) {
    var f = e.target.closest("[data-file]");
    if (f) { fileMenu(f); return; }
    var d = e.target.closest("[data-dir]");
    if (d) { loadDir(d.getAttribute("data-dir")); return; }
    var o = e.target.closest("[data-open-dl]");
    if (o) {
      var id = o.getAttribute("data-open-dl");
      if (androidNative && androidNative.openDownload) androidNative.openDownload(id);
      else if (macNative) macNative.postMessage({ op: "openDownload", id: id });
    }
  });

  // Global click for code copy buttons.
  document.addEventListener("click", function (e) {
    var c = e.target.closest(".code-copy");
    if (!c) return;
    var block = c.closest(".code-block");
    var code = block ? block.querySelector("code") : null;
    if (!code) return;
    writeClipboard(code.textContent).then(function (ok) {
      if (!ok) { toast(T("复制失败"), true); return; }
      // The tick holds for 2.5s and fades back; another click while it shows
      // only restarts the wait, so the icon does not flicker.
      clearTimeout(c._copyTimer);
      c.classList.add("done");
      c.setAttribute("aria-label", T("已复制"));
      c._copyTimer = setTimeout(function () {
        c.classList.remove("done");
        c.setAttribute("aria-label", T("复制代码"));
      }, 2500);
    });
  });

  // Reconnect when the phone comes back online / tab becomes visible.
  window.addEventListener("online", function () {
    if (S.route === "chat" && S.session) connectStream();
    else refreshActive().catch(function () {});
  });
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden && S.route === "chat" && S.session) {
      if (!S.es || S.es.readyState === 2) connectStream();
      else catchUp();
    }
  });

  // PWA. Service workers need a secure context; on plain HTTP over EasyTier the
  // browser usually refuses registration, so register only when allowed.
  if ("serviceWorker" in navigator && window.isSecureContext) {
    navigator.serviceWorker.register("/sw.js").catch(function () {});
  }

  // ---------------------------------------------------------------- start-up
  applyTheme(S.theme);
  // The language comes first: everything drawn after it is in that language.
  function startLanguage(pref) {
    S.langPref = pref || "";
    return window.I18N.load(S.langPref).then(function (lang) {
      window.I18N.localize(document.body);
      // The Mac app names its menus and notifications in the same language.
      if (macNative) macNative.postMessage({ op: "lang", lang: lang });
    });
  }
  fetch("/api/session", { credentials: "same-origin", cache: "no-store" })
    .then(function (r) { return r.json(); })
    .then(function (d) {
      return startLanguage(d.language).then(function () {
        if (d.authenticated) {
          S.csrf = d.csrf || "";
          return boot();
        }
        showLogin();
      });
    })
    .catch(function () {
      (S.langPref === undefined ? startLanguage("") : Promise.resolve()).then(showLogin);
    });
})();
