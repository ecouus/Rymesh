/* Agent Console SPA — vanilla JS, no build step. */
(function () {
  "use strict";

  var $ = function (sel, root) { return (root || document).querySelector(sel); };
  var $$ = function (sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); };
  var esc = window.MD.esc;
  var md = window.MD.render;

  var LS = {
    get: function (k, d) { try { var v = localStorage.getItem("ac." + k); return v === null ? d : v; } catch (e) { return d; } },
    set: function (k, v) { try { localStorage.setItem("ac." + k, v); } catch (e) { /* private mode */ } },
    del: function (k) { try { localStorage.removeItem("ac." + k); } catch (e) {} }
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
    if (s < 45) return "刚刚";
    if (s < 3600) return Math.floor(s / 60) + " 分钟前";
    if (s < 86400) return Math.floor(s / 3600) + " 小时前";
    if (s < 7 * 86400) return Math.floor(s / 86400) + " 天前";
    return d.getFullYear() + "/" + (d.getMonth() + 1) + "/" + d.getDate();
  }

  function clock(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    if (isNaN(d)) return "";
    return d.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
  }

  function dur(start, end) {
    if (!start) return "";
    var a = new Date(start).getTime();
    var b = end ? new Date(end).getTime() : Date.now();
    var s = Math.max(0, Math.round((b - a) / 1000));
    if (s < 60) return s + " 秒";
    if (s < 3600) return Math.floor(s / 60) + " 分 " + (s % 60) + " 秒";
    return Math.floor(s / 3600) + " 小时 " + Math.floor((s % 3600) / 60) + " 分";
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

  function agentBadge(name) {
    var a = agentMeta(name);
    return '<span class="badge agent-' + esc(name) + '">' + esc(a.label || name) + "</span>";
  }

  // A small monochrome glyph identifies the agent in lists, so rows do not
  // need to spell out "Claude Code" / "Codex CLI" every time.
  var AGENT_ICONS = {
    claude: '<path d="M12 4v16M4 12h16M6.3 6.3l11.4 11.4M17.7 6.3 6.3 17.7"/>',
    codex: '<path d="m6 8 4 4-4 4M13 16h5"/>'
  };
  function agentIcon(name) {
    var a = agentMeta(name);
    return '<svg class="ico agent-ico" viewBox="0 0 24 24" aria-label="' + esc(a.label || name) + '">' +
      (AGENT_ICONS[name] || '<circle cx="12" cy="12" r="5"/>') + "</svg>";
  }

  function greeting() {
    var h = new Date().getHours();
    if (h >= 5 && h < 11) return "早上好";
    if (h >= 11 && h < 13) return "中午好";
    if (h >= 13 && h < 18) return "下午好";
    if (h >= 18 && h < 23) return "晚上好";
    return "夜深了";
  }

  function dirName(p) {
    return String(p || "").replace(/\/+$/, "").split("/").pop() || p || "";
  }

  function statusBadge(st, ahead) {
    var label = statusLabel(st, ahead);
    return '<span class="badge status ' + esc(st) + '">' + esc(label) + "</span>";
  }

  function statusLabel(st, ahead) {
    // "排队中" is only true when something is actually ahead of the task.
    if (st === "pending") return ahead > 0 ? "排队中" : "启动中";
    return ({
      running: "运行中", waiting_input: "等待审批",
      completed: "已完成", failed: "失败", cancelled: "已取消", interrupted: "已中断", idle: "空闲"
    })[st] || st || "";
  }

  var TOOL_LABELS = {
    Bash: "终端", shell: "终端", Read: "读取", Write: "写入", Edit: "编辑",
    Glob: "查找文件", Grep: "搜索", WebFetch: "抓取网页", WebSearch: "搜索网页",
    Task: "子任务", TodoWrite: "待办", file_change: "文件变更", mcp_tool_call: "MCP",
    NotebookEdit: "编辑笔记本", MultiEdit: "批量编辑", AskUserQuestion: "提问",
    web_search: "搜索网页", permissions: "额外权限", request_user_input: "提问", ExitPlanMode: "计划"
  };

  function toolLabel(name) { return TOOL_LABELS[name] || name || "工具"; }

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
    for (var i = 0; i < S.nodes.length; i++) if (S.nodes[i].id === id) return S.nodes[i].name;
    return id === "local" ? "本机" : id;
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
          throw new Error("登录已过期");
        }
        if (!res.ok) {
          var msg = data && data.error ? data.error : "HTTP " + res.status;
          var err = new Error(msg);
          err.status = res.status;
          throw err;
        }
        return data;
      });
    });
  }

  // -------------------------------------------------------------------- toast

  var toastTimer = null;
  function toast(msg, isErr) {
    var t = $("#toast");
    t.textContent = msg;
    t.className = "toast" + (isErr ? " err" : "");
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.hidden = true; }, isErr ? 4200 : 2400);
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
        '<button class="btn ghost grow" data-act="no">取消</button>' +
        '<button class="btn ' + (danger ? "danger" : "primary") + ' grow" data-act="yes">' +
        esc(okLabel || "确认") + "</button></div>");
      body.addEventListener("click", function (e) {
        var b = e.target.closest("[data-act]");
        if (!b) return;
        closeSheet();
        resolve(b.getAttribute("data-act") === "yes");
      });
    });
  }

  // --------------------------------------------------------------------- auth

  var pendingLoginPassword = "";

  function showLogin() {
    closeStream();
    S.booted = false;
    closeSidebar(true);
    closeSheet();
    $("#approval-stack").hidden = true;
    $("#app").hidden = true;
    $("#login").hidden = false;
    $("#login-form").hidden = false;
    $("#force-form").hidden = true;
    $("#login-error").hidden = true;
    $("#force-error").hidden = true;
    // Only advertise the default password while it is still in use.
    $("#login-default-hint").hidden = !S.defaultPassword;
    pendingLoginPassword = "";
    S.booted = false;
    if (S.pollTimer) { clearInterval(S.pollTimer); S.pollTimer = null; }
    setTimeout(function () { var p = $("#login-password"); if (p) p.focus(); }, 50);
  }

  function showForceChange() {
    closeStream();
    S.booted = false;
    closeSidebar(true);
    closeSheet();
    $("#approval-stack").hidden = true;
    $("#app").hidden = true;
    $("#login").hidden = false;
    $("#login-form").hidden = true;
    $("#force-form").hidden = false;
    $("#force-error").hidden = true;
    $("#force-old").value = pendingLoginPassword;
    $("#force-new").value = "";
    $("#force-new2").value = "";
    S.booted = false;
    if (S.pollTimer) { clearInterval(S.pollTimer); S.pollTimer = null; }
    setTimeout(function () { var p = $("#force-new"); if (p) p.focus(); }, 50);
  }

  function showApp() {
    $("#login").hidden = true;
    $("#app").hidden = false;
  }

  $("#login-form").addEventListener("submit", function (e) {
    e.preventDefault();
    var btn = $("#login-btn"), errEl = $("#login-error");
    var pw = $("#login-password").value;
    if (!pw) return;
    btn.disabled = true; btn.textContent = "登录中…"; errEl.hidden = true;
    fetch("/api/login", {
      method: "POST", headers: { "Content-Type": "application/json" },
      credentials: "same-origin", body: JSON.stringify({ password: pw })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (d) {
        if (!r.ok) throw new Error(d.error || "登录失败");
        S.csrf = d.csrf || "";
        if (d.must_change) {
          pendingLoginPassword = pw;
          showForceChange();
          return;
        }
        return boot();
      });
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.hidden = false;
    }).then(function () {
      btn.disabled = false; btn.textContent = "登录";
    });
  });

  $("#force-form").addEventListener("submit", function (e) {
    e.preventDefault();
    var btn = $("#force-btn"), errEl = $("#force-error");
    var oldPw = $("#force-old").value;
    var np = $("#force-new").value, np2 = $("#force-new2").value;
    function fail(msg) { errEl.textContent = msg; errEl.hidden = false; }
    errEl.hidden = true;
    if (!oldPw) { fail("请输入当前密码"); return; }
    if (!np) { fail("新密码不能为空"); return; }
    if (np !== np2) { fail("两次输入的新密码不一致"); return; }
    btn.disabled = true; btn.textContent = "修改中…";
    fetch("/api/password", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": S.csrf },
      body: JSON.stringify({ old: oldPw, new: np })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (d) {
        if (!r.ok) throw new Error(d.error || "修改失败");
        S.csrf = d.csrf || S.csrf;
        pendingLoginPassword = "";
        $("#login-password").value = "";
        $("#force-old").value = "";
        $("#force-new").value = "";
        $("#force-new2").value = "";
        return boot();
      });
    }).catch(function (err) {
      fail(err.message || "修改失败");
      btn.disabled = false; btn.textContent = "修改密码并进入";
    });
  });

  // --------------------------------------------------------------------- boot

  function boot() {
    // The device list decides which device to load (a hub-only console has no
    // local one), so it comes first.
    return loadNodes().then(function () {
      if (S.hubOnly && !S.nodes.some(function (n) { return n.id === S.node; })) {
        var first = S.nodes.filter(function (n) { return n.online; })[0] || S.nodes[0];
        S.node = first ? first.id : "local";
        S.draftCwd = ""; // a directory on the previous device means nothing here
        LS.del("draftCwd");
        renderNodeSwitch(); // it was drawn before the device was chosen
      }
      return api("/api/bootstrap");
    }).then(function (d) {
      S.booted = true;
      S.nodeName = d.node_name || "";
      S.csrf = d.csrf || S.csrf;
      S.agents = d.agents || [];
      S.profiles = d.profiles || {};
      S.config = d.config || {};
      S.counts = d.counts || {};
      S.imports = d.import || {};
      S.sessions = d.sessions || [];
      S.tasks = d.tasks || [];
      S.perms = d.permissions || [];
      S.activeTasks = S.tasks.filter(isActiveTask);
      ensureDraftAgent();
      if (!S.draftCwd) S.draftCwd = S.config.default_cwd || "";
      showApp();
      updateThemeColor();
      renderApprovalNotifications();
      route(true);
      dockSidebar();
      startPolling();
    }).catch(function (err) {
      // An unreachable device must not lock the user out: fall back to this
      // console's own machine.
      if (S.node !== "local" && !(err && err.status === 401)) {
        toast((err && err.message) || "设备不可用", true);
        S.node = "local";
        LS.set("node", "local");
        return boot();
      }
      if (err && err.status === 401) showLogin();
      else { showLogin(); toast(err.message || "无法连接服务", true); }
    });
  }

  function loadNodes() {
    return api("/api/nodes", { hub: true }).then(function (d) {
      S.nodes = d.nodes || [];
      S.hubOnly = !!d.hub_only;
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
    if (!multiNode()) { t.textContent = "对话"; return; }
    var cur = S.nodes.filter(function (n) { return n.id === S.node; })[0];
    t.innerHTML = '<button type="button" id="node-switch" class="node-switch" aria-haspopup="dialog">' +
      '<span class="node-dot' + (cur && !cur.online ? " off" : "") + '"></span>' +
      esc(cur ? cur.name : nodeLabel(S.node)) +
      '<svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 10 5 5 5-5"/></svg></button>';
  }

  function nodeRows() {
    return S.nodes.map(function (n) {
      return '<button type="button" class="g-row" data-node="' + esc(n.id) + '"' + (n.online ? "" : " disabled") + ">" +
        '<span class="node-dot' + (n.online ? "" : " off") + '"></span>' +
        '<span class="g-label">' + esc(n.name) + (n.local ? ' <span class="muted small">主控</span>' : "") + "</span>" +
        '<span class="g-value">' + esc(n.online ? (n.id === S.node ? "当前" : "") : (n.error || "离线")) + "</span></button>";
    }).join("");
  }

  function settingsDeviceRows() {
    return S.nodes.map(function (n) {
      var sub = n.local ? "本机" : (n.target || n.url || "");
      return '<button type="button" class="g-row link" ' + (n.local ? 'data-node="local"' : 'data-device="' + esc(n.id) + '"') + ">" +
        '<span class="node-dot' + (n.online ? "" : " off") + '"></span>' +
        '<span class="g-label">' + esc(n.name) + ' <span class="muted small">' + esc(sub) + "</span></span>" +
        '<span class="g-value">' + esc(n.online ? (n.id === S.node ? "当前" : (n.version || "")) : (n.error || "离线")) + "</span></button>";
    }).join("");
  }

  // ------------------------------------------------------------ native bridge

  // The Android app injects AgentConsoleNative; the Mac app listens on the
  // "ac" message handler. In a plain browser both are absent and the web
  // fallbacks apply.
  var androidNative = window.AgentConsoleNative || null;
  var macNative = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.ac || null;

  function copyText(text, msg) {
    var done = function () { toast(msg || "已复制"); };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done).catch(function () { fallbackCopy(text, done); });
    } else fallbackCopy(text, done);
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
        return '<button type="button" class="pick-row' + (it.action ? " action" : "") + (it.selected ? " on" : "") +
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
        '<div class="row" style="gap:8px"><button type="button" class="btn ghost grow" data-ov-close="1">取消</button>' +
        '<button type="submit" class="btn primary grow">' + esc(o.ok || "确定") + "</button></div></form>");
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
        (k.used_by.length ? " · 用于 " + k.used_by.join("、") : ""), selected: current === "id:" + k.id });
    });
    var files = (d.detected || []).filter(function (k) { return !k.imported; });
    if (files.length) items.push({ section: "~/.ssh 中的密钥" });
    files.forEach(function (k) {
      items.push({ value: "path:" + k.path, label: k.name, tag: k.encrypted ? "有密码短语" : "",
        sub: shortFp(k.fingerprint) + (k.encrypted ? " · 使用前需导入" : ""), selected: current === "path:" + k.path });
    });
    if (d.agent) items.push({ value: "agent", label: "ssh-agent", sub: "使用 ssh-agent 里已加载的密钥", selected: current === "agent" });
    if (withNew) items.push({ value: "new", label: "生成新密钥…", action: true });
    return items;
  }

  function keyLabel(d, v) {
    if (!v) return "";
    if (v === "agent") return "ssh-agent";
    var k = (d.keys || []).filter(function (x) { return "id:" + x.id === v; })[0];
    if (k) return k.name;
    var f = (d.detected || []).filter(function (x) { return "path:" + x.path === v; })[0];
    return f ? f.name + "（~/.ssh）" : "";
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
    var def = (S.nodeName || "设备") + "-" + new Date().toISOString().slice(5, 10).replace("-", "");
    return askText("生成新密钥", { label: "名称（便于在服务器上认出它）", value: def, ok: "生成",
      note: "生成一把 ed25519 密钥，私钥保存在本应用的数据库里。" }).then(function (name) {
      if (name === null) return null;
      return api("/api/keys/generate", { method: "POST", body: { name: name.trim() }, hub: true }).then(function (d) {
        toast("已生成密钥「" + d.key.name + "」");
        return d.key;
      });
    }).catch(function (e) { toast(e.message, true); return null; });
  }

  // importKeyFlow copies a ~/.ssh key into the vault, asking for its
  // passphrase if it has one.
  function importKeyFlow(k) {
    function go(pass) {
      return api("/api/keys/import", { method: "POST", hub: true, body: { path: k.path, passphrase: pass || "" } })
        .then(function (d) { toast("已导入「" + d.key.name + "」"); return d.key; });
    }
    var first = k.encrypted ? askText("导入 " + k.name, { label: "密码短语", type: "password", ok: "导入",
      note: "只用这一次：解开后由本应用保存，以后不再需要密码短语。" }).then(function (p) { return p === null ? null : go(p); }) : go("");
    return first.catch(function (e) { toast(e.message, true); return null; });
  }

  // pubBox shows a public key with copy and share; compact leaves out the
  // key text (the add-server form, for a key the user already knows).
  function pubBox(pub, fresh, compact) {
    if (compact) {
      return '<div class="key-card compact"><span class="small muted grow">公钥</span>' +
        '<button type="button" class="btn sm" data-copy-text="' + esc(pub) + '">复制</button>' +
        (canShare() ? '<button type="button" class="btn sm" data-share-text="' + esc(pub) + '">分享…</button>' : "") + "</div>";
    }
    return '<div class="key-card' + (fresh ? " fresh" : "") + '">' +
      (fresh ? '<div class="key-card-head">新密钥已生成</div>' : "") +
      '<div class="small muted">公钥（可以公开）：</div><pre class="mono">' + esc(pub) + "</pre>" +
      '<div class="row" style="gap:8px"><button type="button" class="btn sm grow" data-copy-text="' + esc(pub) + '">复制公钥</button>' +
      (canShare() ? '<button type="button" class="btn sm grow" data-share-text="' + esc(pub) + '">分享…</button>' : "") +
      "</div>" + (fresh ? '<div class="small muted" style="margin-top:8px">连接时可以用服务器密码自动安装它，或者手动加入服务器的 <code>~/.ssh/authorized_keys</code>。</div>' : "") +
      "</div>";
  }

  document.addEventListener("click", function (e) {
    var c = e.target.closest("[data-copy-text]");
    if (c) { copyText(c.getAttribute("data-copy-text")); return; }
    var s = e.target.closest("[data-share-text]");
    if (s) shareText(s.getAttribute("data-share-text"), "公钥");
  });

  function addDeviceSheet() {
    var body = openSheet("添加服务器",
      '<div class="stack device-form">' +
      '<button type="button" class="g-row link pick-btn" id="dev-import" hidden><span class="g-label">从 ~/.ssh/config 填入</span><span class="g-value"></span></button>' +
      '<label class="field"><span>名称</span><input id="dev-name" placeholder="例如 生产服务器" maxlength="60"></label>' +
      '<div class="row"><label class="field grow"><span>主机</span><input id="dev-host" class="mono" placeholder="IP 或域名" autocapitalize="off" spellcheck="false"></label>' +
      '<label class="field" style="width:84px"><span>SSH 端口</span><input id="dev-port" class="mono" inputmode="numeric" value="22"></label></div>' +
      '<label class="field"><span>用户</span><input id="dev-user" class="mono" value="root" autocapitalize="off" spellcheck="false"></label>' +
      '<div class="field"><span>认证密钥</span><button type="button" class="pick-btn input-like" id="dev-key"><span class="t"></span>' +
      '<svg class="ico sm" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 10 5 5 5-5"/></svg></button></div>' +
      '<div id="dev-pub"></div>' +
      '<details class="fold"><summary>高级</summary><label class="field" style="margin-top:8px"><span>节点端口（服务器本机回环端口）</span>' +
      '<input id="dev-rport" class="mono" inputmode="numeric" value="8899"></label></details>' +
      '<button id="dev-save" class="btn primary block" type="button">连接</button>' +
      '<div id="dev-flow"></div></div>');
    var keys = { keys: [], detected: [] }, chosen = "", fresh = "";
    function renderKey() {
      var label = keyLabel(keys, chosen);
      var b = $("#dev-key");
      b.querySelector(".t").textContent = label || "选择或生成密钥";
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
      pickList("从 ~/.ssh/config 填入", hosts.map(function (h, i) {
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
      pickList("认证密钥", keyChoices(keys, chosen, true)).then(function (v) {
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
      if (!chosen) { toast("请先选择或生成一把密钥", true); return; }
      var req = Object.assign({
        transport: "ssh", name: $("#dev-name").value.trim(), ssh_host: $("#dev-host").value.trim(),
        ssh_port: parseInt($("#dev-port").value, 10) || 22, ssh_user: $("#dev-user").value.trim(),
        remote_port: parseInt($("#dev-rport").value, 10) || 8899
      }, keyFields(chosen));
      var btn = this; btn.disabled = true;
      api("/api/devices", { method: "POST", body: req, hub: true }).then(function (d) {
        btn.hidden = true;
        $$(".device-form > .field, .device-form > .row, .device-form > details, .device-form > .pick-btn, #dev-pub", body)
          .forEach(function (el) { el.hidden = true; });
        deviceFlow(d.device.id, $("#dev-flow"));
      }).catch(function (e) { toast(e.message, true); btn.disabled = false; });
    });
  }

  // deviceFlow drives test → confirm fingerprint → install key → deploy
  // inside a sheet.
  function deviceFlow(id, box) {
    var path = "/api/devices/" + encodeURIComponent(id);
    box.innerHTML = '<div class="flow-step"><span class="spinner"></span> 正在连接…</div>';
    api(path + "/test", { method: "POST", body: {}, hub: true }).then(function (r) {
      if (r.stage === "hostkey") {
        var hk = r.hostkey;
        box.innerHTML = '<div class="flow-step">' +
          (hk.mismatch ? '<div class="error-block">警告：这台主机的密钥和 ~/.ssh/known_hosts 里记录的不一致。除非你确认服务器重装过，否则不要信任。</div>' : "") +
          "<div>首次连接，请核对服务器的主机指纹：</div><pre class=\"mono fp\">" + esc(hk.fingerprint) + "</pre>" +
          '<div class="small muted">可在服务器上运行 <code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code> 对照。</div>' +
          '<div class="row" style="margin-top:12px"><button class="btn primary grow" type="button" data-trust="1">信任并继续</button></div></div>';
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
        box.innerHTML = '<div class="flow-step"><div class="error-block">' + esc(r.error) + "</div>" +
          '<button class="btn block" type="button" data-retry="1">重试</button></div>';
        box.querySelector("[data-retry]").addEventListener("click", function () { deviceFlow(id, box); });
        return;
      }
      var online = r.node && r.node.online;
      box.innerHTML = '<div class="flow-step"><div class="group">' +
        '<div class="g-row"><span class="g-label">连接</span><span class="g-value">SSH 已连接 · ' + esc(r.platform || "") + "</span></div>" +
        '<div class="g-row"><span class="g-label">节点</span><span class="g-value">' +
        esc(r.installed ? r.installed + (online ? " · 运行中" : " · 未运行") : "未安装") + "</span></div></div>" +
        '<button class="btn ' + (online ? "" : "primary ") + 'block" type="button" data-deploy="1" style="margin-top:12px">' +
        (r.installed ? "升级 / 重新部署节点" : "部署节点") + "</button>" +
        (online ? '<button class="btn primary block" type="button" data-use="1" style="margin-top:8px">切换到这台设备</button>' : "") +
        '<pre class="deploy-log mono" hidden></pre></div>';
      var log = box.querySelector(".deploy-log");
      var use = box.querySelector("[data-use]");
      if (use) use.addEventListener("click", function () { loadNodes().then(function () { switchNode(id); }); });
      box.querySelector("[data-deploy]").addEventListener("click", function () {
        var b = this; b.disabled = true; log.hidden = false; log.textContent = "";
        postStream(path + "/deploy", function (ev, d) {
          if (ev === "progress") log.textContent += d.text + "\n";
          else if (ev === "failed") { log.textContent += "✕ " + d.error + "\n"; b.disabled = false; }
          else if (ev === "done") { log.textContent += "✓ 部署完成\n"; loadNodes().then(function () { deviceFlow(id, box); }); }
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
    if (!r.public) {
      box.innerHTML = '<div class="flow-step"><div class="error-block">服务器不接受 ssh-agent 里的密钥。</div>' +
        '<button class="btn block" type="button" data-retry="1">重试</button></div>';
      box.querySelector("[data-retry]").addEventListener("click", function () { deviceFlow(id, box); });
      return;
    }
    var cmd = "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '" + r.public + "' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys";
    box.innerHTML = '<div class="flow-step">' +
      '<div class="notice">服务器还不认识这把密钥。用下面任一方式把公钥放到服务器上：</div>' +
      '<div class="choice-card"><div class="choice-title">用服务器密码自动安装</div>' +
      '<form data-install="1"><input class="input" type="password" name="pw" placeholder="服务器登录密码" autocomplete="off">' +
      '<button class="btn primary block" type="submit" style="margin-top:8px">安装公钥并连接</button></form>' +
      '<div class="small muted" style="margin-top:6px">密码只用于这一次连接，不会保存；需要服务器允许密码登录。</div></div>' +
      '<div class="choice-card"><div class="choice-title">手动添加</div>' +
      '<div class="small muted">在服务器上执行这条命令（或把公钥加入 <code>~/.ssh/authorized_keys</code>）：</div>' +
      '<pre class="mono">' + esc(cmd) + "</pre>" +
      '<div class="row" style="gap:8px"><button type="button" class="btn sm grow" data-copy-text="' + esc(cmd) + '">复制命令</button>' +
      '<button type="button" class="btn sm grow" data-copy-text="' + esc(r.public) + '">复制公钥</button>' +
      (canShare() ? '<button type="button" class="btn sm grow" data-share-text="' + esc(r.public) + '">分享…</button>' : "") + "</div>" +
      '<button class="btn block" type="button" data-retry="1" style="margin-top:10px">已添加，重新连接</button></div></div>';
    box.querySelector("[data-retry]").addEventListener("click", function () { deviceFlow(id, box); });
    box.querySelector("[data-install]").addEventListener("submit", function (e) {
      e.preventDefault();
      var pw = this.pw.value, b = this.querySelector("button");
      if (!pw) { this.pw.focus(); return; }
      b.disabled = true; b.textContent = "正在安装…";
      var form = this;
      api(path + "/install-key", { method: "POST", hub: true, body: { password: pw } }).then(function () {
        form.pw.value = "";
        toast("公钥已安装");
        deviceFlow(id, box);
      }).catch(function (err) {
        toast(err.message, true);
        b.disabled = false; b.textContent = "安装公钥并连接";
      });
    });
  }

  function deviceSheet(id) {
    var n = S.nodes.filter(function (x) { return x.id === id; })[0] || { id: id, name: id };
    var body = openSheet(n.name,
      '<div class="stack"><div class="group">' +
      '<div class="g-row"><span class="g-label">地址</span><span class="g-value mono">' + esc(n.target || n.url || "") + "</span></div>" +
      '<div class="g-row"><span class="g-label">状态</span><span class="g-value">' + esc(n.online ? "在线 " + (n.version || "") : (n.error || "离线")) + "</span></div>" +
      '<button type="button" class="g-row link" data-dev-key="1" hidden><span class="g-label">认证密钥</span><span class="g-value"></span></button></div>' +
      '<div id="dev-flow"></div>' +
      '<button class="btn danger block" type="button" data-del-device="1">删除设备</button>' +
      '<p class="small muted">删除只移除本客户端里的记录，不会卸载服务器上的节点。</p></div>');
    deviceFlow(id, $("#dev-flow"));
    Promise.all([api("/api/devices", { hub: true }), api("/api/keys", { hub: true })]).then(function (res) {
      var dev = (res[0].devices || []).filter(function (x) { return x.id === id; })[0];
      var keys = res[1], row = body.querySelector("[data-dev-key]");
      if (!dev || dev.transport !== "ssh" || !row) return;
      var cur = keyValueOf(dev);
      row.hidden = false;
      row.querySelector(".g-value").textContent = keyLabel(keys, cur) || "未设置";
      row.addEventListener("click", function () {
        pickList("认证密钥", keyChoices(keys, cur, true)).then(function (v) {
          if (v === "new") return generateKeyFlow().then(function (k) { return k && "id:" + k.id; });
          var f = (keys.detected || []).filter(function (x) { return "path:" + x.path === v; })[0];
          if (f && f.encrypted) return importKeyFlow(f).then(function (k) { return k && "id:" + k.id; });
          return v;
        }).then(function (v) {
          if (!v || v === cur) return;
          return api("/api/devices/" + encodeURIComponent(id), { method: "PATCH", hub: true, body: keyFields(v) })
            .then(function () { toast("已更换密钥"); deviceSheet(id); });
        }).catch(function (e) { toast(e.message, true); });
      });
    }).catch(function () {});
    body.querySelector("[data-del-device]").addEventListener("click", function () {
      confirmSheet("删除设备「" + n.name + "」？", "<p>之后可以重新添加；服务器上的节点和会话不受影响。</p>", "删除", true).then(function (ok) {
        if (!ok) return;
        api("/api/devices/" + encodeURIComponent(id), { method: "DELETE", body: {}, hub: true }).then(function () {
          if (S.node === id) switchNode("local");
          loadNodes().then(function () { if (S.route === "settings") renderSettings(); });
          toast("已删除");
        }).catch(function (e) { toast(e.message, true); });
      });
    });
  }

  // ------------------------------------------------------- keys and transfer

  function keysSheet() {
    var body = openSheet("密钥", '<div class="empty"><span class="spinner"></span></div>');
    api("/api/keys", { hub: true }).then(function (d) {
      var mine = (d.keys || []).map(function (k) {
        return '<button type="button" class="g-row link" data-key="' + esc(k.id) + '"><span class="g-label">' + esc(k.name) +
          '<span class="g-sub">' + esc(k.type.replace("ssh-", "") + " · " + shortFp(k.fingerprint)) +
          (k.used_by.length ? " · 用于 " + esc(k.used_by.join("、")) : "") + "</span></span></button>";
      }).join("");
      var files = (d.detected || []).filter(function (k) { return !k.imported; }).map(function (k, i) {
        return '<div class="g-row"><span class="g-label">' + esc(k.name) + '<span class="g-sub">' + esc(shortFp(k.fingerprint)) +
          (k.encrypted ? " · 有密码短语" : "") + (k.used_by.length ? " · 用于 " + esc(k.used_by.join("、")) : "") + "</span></span>" +
          '<button type="button" class="btn sm" data-import-key="' + i + '">导入</button></div>';
      }).join("");
      var detected = (d.detected || []).filter(function (k) { return !k.imported; });
      body.innerHTML = '<div class="stack">' +
        (mine ? '<div class="group">' + mine + "</div>" : '<p class="muted small">还没有密钥。</p>') +
        '<button type="button" class="btn block" data-gen-key="1">生成新密钥</button>' +
        (files ? '<div><div class="section-title">在 ~/.ssh 中发现</div><div class="group">' + files + "</div>" +
          '<div class="g-note">导入后私钥保存在本应用的数据库里，可以随设备一起导出到其它客户端；原文件不受影响。</div></div>' : "") +
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
      '<div class="group"><div class="g-row"><span class="g-label">类型</span><span class="g-value">' + esc(k.type) + "</span></div>" +
      '<div class="g-row"><span class="g-label">指纹</span><span class="g-value mono">' + esc(k.fingerprint) + "</span></div>" +
      '<div class="g-row"><span class="g-label">用于</span><span class="g-value">' + esc(k.used_by.join("、") || "—") + "</span></div></div>" +
      '<div class="row" style="gap:8px"><button type="button" class="btn grow" data-rename="1">重命名</button>' +
      '<button type="button" class="btn danger grow" data-delete="1">删除</button></div>' +
      '<button type="button" class="btn ghost block" data-back="1">返回密钥列表</button></div>');
    body.addEventListener("click", function (e) {
      if (e.target.closest("[data-back]")) { keysSheet(); return; }
      if (e.target.closest("[data-rename]")) {
        askText("重命名密钥", { label: "名称", value: k.name, ok: "保存" }).then(function (name) {
          if (name === null || !name.trim() || name.trim() === k.name) return;
          return api("/api/keys/" + encodeURIComponent(k.id), { method: "PATCH", hub: true, body: { name: name.trim() } })
            .then(function () { toast("已重命名"); keysSheet(); });
        }).catch(function (err) { toast(err.message, true); });
        return;
      }
      if (e.target.closest("[data-delete]")) {
        confirmSheet("删除密钥「" + k.name + "」？", "<p>私钥会从本应用中删除且无法恢复。已经加到服务器上的公钥不受影响，需要时请自行从 <code>authorized_keys</code> 中移除。</p>", "删除", true)
          .then(function (ok) {
            if (!ok) { keyDetailSheet(k); return; }
            return api("/api/keys/" + encodeURIComponent(k.id), { method: "DELETE", hub: true, body: {} })
              .then(function () { toast("已删除"); keysSheet(); });
          }).catch(function (err) { toast(err.message, true); keysSheet(); });
      }
    });
  }

  function exportSheet() {
    var body = openSheet("导出设备", '<div class="empty"><span class="spinner"></span></div>');
    api("/api/devices", { hub: true }).then(function (d) {
      var devs = d.devices || [];
      if (!devs.length) { body.innerHTML = '<p class="muted">还没有可以导出的设备。</p>'; return; }
      body.innerHTML = '<div class="stack"><p class="small muted" style="margin:0">把选中的服务器连同它们的密钥、主机指纹和节点令牌导出成一个加密文件，在另一台设备（例如手机）上导入即可直接使用。</p>' +
        '<div class="group">' + devs.map(function (n) {
          return '<label class="g-row check-row"><input type="checkbox" value="' + esc(n.id) + '" checked>' +
            '<span class="g-label">' + esc(n.name) + '<span class="g-sub">' + esc(n.ssh_user ? n.ssh_user + "@" + n.ssh_host : n.url || "") + "</span></span></label>";
        }).join("") + "</div>" +
        '<button type="button" class="btn primary block" data-do-export="1">导出</button></div>';
      body.querySelector("[data-do-export]").addEventListener("click", function () {
        var ids = $$("input[type=checkbox]:checked", body).map(function (c) { return c.value; });
        if (!ids.length) { toast("请至少选择一台设备", true); return; }
        this.disabled = true;
        api("/api/vault/export", { method: "POST", hub: true, body: { devices: ids } }).then(function (r) {
          exportResult(body, r);
        }).catch(function (e) { toast(e.message, true); body.querySelector("[data-do-export]").disabled = false; });
      });
    }).catch(function (e) { body.innerHTML = '<div class="error-block">' + esc(e.message) + "</div>"; });
  }

  function exportResult(body, r) {
    body.innerHTML = '<div class="stack">' +
      '<div class="code-card"><div class="small muted">解密码</div><div class="code-big mono">' + esc(r.code) + "</div>" +
      '<button type="button" class="btn sm" data-copy-text="' + esc(r.code) + '">复制解密码</button></div>' +
      '<div class="notice">解密码只显示这一次。请把它和文件<b>分开</b>传递（例如文件用 AirDrop 或数据线，解密码手动输入）。</div>' +
      (r.warnings.length ? '<div class="error-block">' + r.warnings.map(esc).join("\n") + "</div>" : "") +
      '<button type="button" class="btn primary block" data-save="1">保存文件</button>' +
      '<p class="small muted" style="margin:0">' + esc(r.filename) + "</p></div>";
    body.querySelector("[data-save]").addEventListener("click", function () { saveTextFile(r.filename, r.bundle); });
  }

  function importSheet() {
    var body = openSheet("导入设备",
      '<form class="stack" data-import-form="1">' +
      '<p class="small muted" style="margin:0">选择从另一台设备导出的 <code>.acbundle</code> 文件，并输入导出时显示的解密码。</p>' +
      '<label class="file-pick"><input type="file" name="file" accept=".acbundle,application/json,application/octet-stream,*/*">' +
      '<span class="file-pick-label">选择文件…</span></label>' +
      '<details class="fold"><summary>或粘贴文件内容</summary><textarea name="text" class="input mono" rows="4" style="margin-top:8px"></textarea></details>' +
      '<label class="field"><span>解密码</span><input name="code" class="mono" placeholder="XXXX-XXXX-XXXX-XXXX" autocomplete="off" autocapitalize="characters" spellcheck="false"></label>' +
      '<button type="submit" class="btn primary block">导入</button></form>');
    var form = body.querySelector("form"), bundle = "";
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
      if (!data) { toast("请选择文件", true); return; }
      if (!form.code.value.trim()) { form.code.focus(); return; }
      var b = form.querySelector("[type=submit]"); b.disabled = true; b.textContent = "正在导入…";
      api("/api/vault/import", { method: "POST", hub: true, body: { bundle: data, code: form.code.value } }).then(function (r) {
        closeSheet();
        toast("已导入 " + (r.added + r.updated) + " 台设备" + (r.keys ? "、" + r.keys + " 把密钥" : ""));
        loadNodes().then(function () { if (S.hubOnly && S.node === "local") boot(); else if (S.route === "settings") renderSettings(); });
      }).catch(function (err) { toast(err.message, true); b.disabled = false; b.textContent = "导入"; });
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
    if (!$("#sheet").hidden) { closeSheet(); return true; }
    if (!$("#sidebar").hidden && !wide.matches) { closeSidebar(); return true; }
    if (location.hash && location.hash !== "#/" && location.hash !== "#") { location.hash = "#/"; return true; }
    return false;
  };

  function nodeSheet() {
    var body = openSheet("切换设备", '<div class="group">' + nodeRows() + "</div>" +
      '<p class="g-note">每台设备上的 Agent 都在各自机器上运行；切换只是换一个查看和操作的对象。</p>');
    loadNodes().then(function () {
      var g = body.querySelector(".group");
      if (g) g.innerHTML = nodeRows();
    });
  }

  document.addEventListener("click", function (e) {
    if (e.target.closest("#node-switch")) { nodeSheet(); return; }
    var n = e.target.closest("[data-node]");
    if (n && !n.disabled) switchNode(n.getAttribute("data-node"));
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
    meta.setAttribute("content", light ? "#ffffff" : "#0f0f10");
    if (window.AgentConsoleNative) window.AgentConsoleNative.setTheme(!light);
  }
  matchMedia("(prefers-color-scheme: light)").addEventListener?.("change", updateThemeColor);

  // ------------------------------------------------------------------- router

  window.addEventListener("hashchange", function () { route(false); });

  function route(initial) {
    if (!S.booted) return;
    var raw = (location.hash || "").replace(/^#\/?/, "");
    if (S.es && S.route === "chat" && !/^session\//.test(raw)) closeStream();
    closeSidebar();
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
      if (S.session) toast("会话目录已固定：" + S.session.cwd);
      else cwdSheet();
    });
  }

  function setConn(state) {
    S.conn = state;
    var d = $("#conn-dot");
    d.className = "conn-dot" + (state === "on" ? " on" : state === "off" ? " off" : state === "wait" ? " wait" : "");
    d.title = state === "on" ? "实时连接正常" : state === "wait" ? "正在连接…" : "连接已断开，正在重连";
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
      sel.innerHTML = '<option>无 Agent</option>';
      sel.disabled = true;
      return;
    }
    var current = S.session ? S.session.agent : S.draftAgent;
    sel.innerHTML = S.agents.map(function (a) {
      return '<option value="' + esc(a.name) + '"' + (a.available ? "" : " disabled") + ">" +
        esc(a.label || a.name) + (a.available ? "" : "（不可用）") + "</option>";
    }).join("");
    var exists = S.agents.some(function (a) { return a.name === current; });
    sel.value = exists ? current : (S.agents[0] || {}).name || "";
    sel.disabled = false;
    // The permission options are agent-specific, so rebuild them whenever the
    // effective agent changes (draft switch, opening a conversation, new chat).
    if (S._profileAgent !== current) {
      S._profileAgent = current;
      renderProfileSelect();
    }
  }

  function setAgentPicker(visible) {
    var sel = $("#agent-select");
    if (sel) sel.hidden = !visible;
  }

  function profilesForAgent(name) {
    return (S.profiles && S.profiles[name]) || [];
  }

  function profileMeta(agent, id) {
    var list = profilesForAgent(agent);
    for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i];
    return null;
  }

  function profileLabel(agent, id) {
    var p = profileMeta(agent, id);
    return p ? p.label : (id || "");
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
    $("#profile-label").textContent = meta ? meta.label : "权限";
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
    if (!e.target.closest(".menu-wrap")) closeProfileMenu();
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
    $("#settings-btn").classList.remove("active");
    setAgentPicker(true);
    if (noDevice()) {
      setTop("Agent Console", "");
      $("#composer-wrap").hidden = true;
      document.body.classList.remove("has-composer");
      $("#main").innerHTML = '<div class="home-empty onboarding">' +
        '<div class="home-title">欢迎</div>' +
        '<div class="home-sub">添加一台服务器开始使用：通过 SSH 连接，并在服务器上自动部署节点。服务器只需开放 SSH。</div>' +
        '<button class="btn primary block" type="button" data-add-device="1" style="margin-top:24px">添加服务器</button>' +
        '<button class="btn block" type="button" data-import="1" style="margin-top:10px">从其它设备导入</button></div>';
      return;
    }
    var a = agentMeta(S.draftAgent);
    // The directory is shown (and changed) in the greeting, so the top bar
    // stays a plain title here.
    setTop("新对话", "");
    $("#prompt").placeholder = "给 " + (a.label || a.name || "Agent") + " 发消息…";
    var recent = S.sessions.slice(0, 3).map(function (s) {
      return '<button type="button" class="recent-card" data-recent="' + esc(s.id) + '">' +
        agentIcon(s.agent) +
        '<span class="recent-main"><span class="recent-title" style="display:block">' + esc(s.title || "未命名会话") + "</span>" +
        '<span class="recent-sub" style="display:block">' + esc(dirName(s.cwd)) + "</span></span>" +
        '<span class="recent-time">' + esc(rel(s.last_message_at || s.updated_at)) + "</span></button>";
    }).join("");
    $("#main").innerHTML =
      '<div class="home-empty">' +
      '<div class="home-title">' + esc(greeting()) + "</div>" +
      '<div class="home-sub">新对话将在 ' + (multiNode() ? esc(nodeLabel(S.node)) + " 的 " : "") +
      '<button type="button" data-home-cwd="1" title="更改工作目录">' +
      esc(truncate(S.draftCwd, 48)) + "</button> 中运行</div>" +
      (recent ? '<div class="home-recent"><div class="home-recent-label">继续最近的对话</div>' + recent + "</div>" : "") +
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

  // One chip per agent that actually has conversations, plus "all". A saved
  // filter whose agent no longer has any conversation falls back to "all".
  function renderAgentFilter() {
    var bar = $("#agent-filter");
    if (!bar) return;
    var counts = {};
    S.sessions.forEach(function (s) { counts[s.agent] = (counts[s.agent] || 0) + 1; });
    var names = Object.keys(counts);
    if (S.agentFilter !== "all" && !counts[S.agentFilter]) S.agentFilter = "all";
    bar.hidden = names.length < 2;
    bar.innerHTML = [["all", "全部"]].concat(names.map(function (n) {
      return [n, agentMeta(n).label || n];
    })).map(function (f) {
      return '<button type="button" role="tab" class="chip' + (S.agentFilter === f[0] ? " active" : "") +
        '" aria-selected="' + (S.agentFilter === f[0]) + '" data-agent-filter="' + esc(f[0]) + '">' + esc(f[1]) + "</button>";
    }).join("");
  }

  $("#agent-filter").addEventListener("click", function (e) {
    var b = e.target.closest("[data-agent-filter]");
    if (!b) return;
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
        ((S.sessionQuery || S.agentFilter !== "all") ? "没有匹配的对话。" : "还没有对话。点击上方新建，或输入消息开始。") + "</div>";
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
        '<span class="side-title" data-side-title="' + esc(s.id) + '">' + esc(s.title || "未命名会话") + "</span>" +
        '<span class="badge status' + (act ? " " + esc(act.status) : "") + '" data-side-badge="' + esc(s.id) + '"' +
        (act ? "" : " hidden") + ">" + (act ? esc(statusLabel(act.status, act.queue_ahead)) : "") + "</span>" +
        "</button>" +
        '<button class="side-more" type="button" data-side-menu="' + esc(s.id) +
        '" aria-label="对话操作" aria-haspopup="dialog">⋯</button>' +
        "</div>";
    }).join("");
  }

  // Sessions arrive newest first, so a heading is emitted whenever the bucket changes.
  function dayGroup(iso) {
    var d = new Date(iso || 0);
    if (isNaN(d) || !iso) return "更早";
    var today = new Date(); today.setHours(0, 0, 0, 0);
    var days = Math.floor((today - new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 86400000);
    if (days <= 0) return "今天";
    if (days === 1) return "昨天";
    if (days < 7) return "近 7 天";
    if (days < 30) return "近 30 天";
    return "更早";
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
      var title = s && s.title ? s.title : (g.first.session_title || "会话 " + String(g.sid || "").slice(0, 8));
      if (g.node) title = nodeLabel(g.node) + " · " + title;
      var tools = [];
      g.items.forEach(function (p) {
        var label = p.kind === "question" ? "Agent 在提问" : p.kind === "plan" ? "计划待批准" : toolLabel(p.tool_name);
        if (tools.indexOf(label) < 0) tools.push(label);
      });
      var toolText = tools.slice(0, 2).join("、") + (tools.length > 2 ? " 等" : "");
      return '<button type="button" class="approval-toast" data-approval-node="' + esc(g.node) +
        '" data-approval-sid="' + esc(g.sid) +
        '" data-approval-pid="' + esc(g.first.id) + '">' +
        '<div class="approval-top"><span class="approval-dot"></span>' +
        '<span class="approval-title">' + (g.first.kind === "question" ? "Agent 在提问" : g.first.kind === "plan" ? "计划待批准" : "需要审批") + "</span>" +
        (g.items.length > 1 ? '<span class="approval-count">' + g.items.length + " 项</span>" : "") +
        "</div>" +
        '<div class="approval-session">' + esc(truncate(title, 48)) + "</div>" +
        '<div class="approval-tool">' + esc(toolText) + " · 点击前往处理</div></button>";
    }).join("") +
      (hiddenItems ? '<div class="approval-more">另有 ' + hiddenItems + " 项待审批</div>" : "");
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
    list.addEventListener("touchend", cancel);
    list.addEventListener("touchcancel", cancel);
    // The click that follows a long press must not also open the conversation.
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
    var body = openSheet("对话操作",
      '<div class="stack">' +
      '<button class="btn block" data-a="rename">重命名对话</button>' +
      '<button class="btn block danger" data-a="delete">删除对话</button>' +
      '<p class="small muted">删除对话会同步移除控制台记录，并将该 Agent 的原生会话文件移动到服务端回收目录。此操作不可从界面撤销。</p>' +
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
    var body = openSheet("重命名会话",
      '<label class="field"><span>标题</span><input id="rename-input" value="' + esc(s.title || "") + '"></label>' +
      '<button id="rename-ok" class="btn primary block">保存</button>');
    $("#rename-ok").addEventListener("click", function () {
      var v = $("#rename-input").value.trim();
      api("/api/sessions/" + encodeURIComponent(s.id), { method: "PATCH", body: { title: v } })
        .then(function () {
          s.title = v || "未命名会话";
          if (S.session && S.session.id === s.id) S.session.title = s.title;
          closeSheet(); renderSidebar(); updateChatChrome(); toast("已重命名");
        }).catch(function (e) { toast(e.message, true); });
    });
  }

  function deleteNativeFlow(s) {
    confirmSheet("删除对话？",
      '<p>将删除「' + esc(truncate(s.title || "未命名会话", 60)) + '」，并同步处理磁盘文件：</p>' +
      '<ul style="padding-left:20px;color:var(--text-dim);font-size:13px">' +
      "<li>该 Agent 的原生会话文件会移动到服务端回收目录</li>" +
      "<li>控制台中的会话、消息与审批记录会被移除</li></ul>" +
      '<p class="small muted">不会影响其他对话。</p>',
      "继续", true).then(function (ok) {
      if (!ok) return;
      confirmSheet("再次确认删除",
        '<p>该对话及其原生会话文件将被移除，且无法从界面恢复。</p>' +
        '<p class="small muted">确定要删除吗？</p>',
        "永久删除", true).then(function (yes) {
        if (!yes) return;
        api("/api/sessions/" + encodeURIComponent(s.id) + "?confirm=" + encodeURIComponent(s.id), { method: "DELETE" })
          .then(function (d) {
            S.sessions = S.sessions.filter(function (x) { return x.id !== s.id; });
            if (S.session && S.session.id === s.id) {
              S.session = null;
              go("#/");
            } else renderSidebar();
            toast(d.native_trashed ? "对话已删除，原生文件已移入回收目录" : "对话已删除（未找到原生文件）");
          }).catch(function (e) { toast(e.message, true); });
      });
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
    if (S.session) { toast("会话目录已固定：" + S.session.cwd); return; }
    var cur = S.draftCwd || S.config.default_cwd || "";
    var body = openSheet("工作目录",
      '<div class="stack">' +
      '<label class="field"><span>新对话使用的服务器绝对路径</span>' +
      '<input id="cwd-input" class="mono" value="' + esc(cur) + '" spellcheck="false" autocomplete="off"></label>' +
      '<div class="row"><button id="cwd-check" class="btn sm" type="button">检查目录</button>' +
      '<button id="cwd-reset" class="btn sm ghost" type="button">恢复默认</button>' +
      '<span id="cwd-result" class="small muted"></span></div>' +
      '<button id="cwd-save" class="btn primary block">保存</button>' +
      '<p class="small muted">目录必须在允许根目录内。不同对话可以使用不同目录；同一目录并发运行时会给出提示。</p>' +
      "</div>");
    $("#cwd-check").addEventListener("click", function () {
      var out = $("#cwd-result"); out.textContent = "检查中…";
      api("/api/cwd/check", { method: "POST", body: { path: $("#cwd-input").value.trim() } })
        .then(function (d) {
          if (!d.ok) { out.textContent = d.error; out.style.color = "var(--red)"; return; }
          out.style.color = "var(--text-dim)";
          out.textContent = "✓ " + d.cwd + (d.git_root ? " · git 仓库" : "") +
            (d.active_tasks ? " · " + d.active_tasks + " 个任务运行中" : "");
          if (d.warning) toast(d.warning, true);
        }).catch(function (e) { out.textContent = e.message; out.style.color = "var(--red)"; });
    });
    $("#cwd-reset").addEventListener("click", function () {
      $("#cwd-input").value = S.config.default_cwd || "";
      $("#cwd-result").textContent = "";
    });
    $("#cwd-save").addEventListener("click", function () {
      var v = $("#cwd-input").value.trim();
      if (!v) { toast("目录不能为空", true); return; }
      S.draftCwd = v;
      LS.set("draftCwd", v);
      closeSheet();
      if (S.route === "home") renderHome();
      else updateComposerMeta();
      toast("工作目录已保存");
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
    S.oldestSeq = 0;
    S.hasOlder = false;
    S.sessionTasks = [];
    S.sessionActive = [];
    S.sessionPerms = [];
    $("#composer-wrap").hidden = false;
    $("#conn-dot").hidden = false;
    $("#settings-btn").classList.remove("active");
    document.body.classList.add("has-composer");

    var main = $("#main");
    main.innerHTML =
      '<div class="chat-wrap"><div id="perm-banner"></div>' +
      '<div id="events" class="events"><div class="events-inner" id="events-inner">' +
      '<div class="empty"><span class="spinner"></span> 正在加载会话…</div>' +
      "</div></div></div>";
    setTop("会话", "");
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
      setTop(S.session.title || "会话", sessionSub(), sessionState());
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
        toast("会话不存在或已删除", true);
        go("#/");
        return;
      }
      toast(e.message, true);
      $("#events-inner").innerHTML = '<div class="empty">加载失败：' + esc(e.message) + "</div>";
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
    $("#prompt").placeholder = "给 " + (agentMeta(S.session.agent).label || S.session.agent) + " 发消息…";
    setTop(S.session.title || "会话", sessionSub(), sessionState());
    // Only show a hint while something is actually happening. The idle state
    // needs no caption: the top bar already shows agent, state and directory.
    var act = activeOnSession();
    var hint = "";
    if (act) {
      if (act.status === "waiting_input") hint = "Agent 正在等待审批，请处理上方请求";
      else if (act.status === "running") hint = "任务运行中 · 新消息将排队执行";
      else if (act.status === "pending") {
        // Only say "queued" when something really is ahead. A task that is
        // first in line is about to start, not waiting behind itself.
        hint = act.queue_ahead > 0
          ? "排队中 · 前面还有 " + act.queue_ahead + " 个任务，结束后自动执行"
          : "正在启动…";
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
  }

  function renderPermissionBanner() {
    var box = $("#perm-banner");
    if (!box) return;
    var pend = (S.sessionPerms || []).filter(function (p) { return p.status === "pending"; });
    if (!pend.length) { box.innerHTML = ""; return; }
    box.innerHTML = '<div class="permission-banner">' + pend.length +
      ' 项等待你处理 <button class="btn sm" type="button" data-scroll-perm="1">查看</button></div>';
  }
  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-scroll-perm]")) {
      var card = $("#events-inner [data-permission-id]");
      if (card) { card.scrollIntoView({ behavior: "smooth", block: "center" }); }
    }
  });

  function addOlderButton() {
    var inner = $("#events-inner");
    if (!inner || $("#load-older")) return;
    var b = document.createElement("button");
    b.id = "load-older";
    b.className = "btn sm ghost load-older";
    b.type = "button";
    b.textContent = "加载更早的消息";
    b.addEventListener("click", loadOlder);
    inner.prepend(b);
  }

  function loadOlder() {
    if (!S.session || !S.oldestSeq) return;
    var btn = $("#load-older");
    if (btn) { btn.disabled = true; btn.textContent = "加载中…"; }
    var inner = $("#events-inner");
    var prevH = inner.scrollHeight;
    var sid = S.session.id;
    api("/api/sessions/" + encodeURIComponent(sid) + "/events?before=" + S.oldestSeq +
      "&turns=" + S.olderTurns + "&limit=" + S.olderEvents)
      .then(function (d) {
        // Navigating to another session while this page is in flight must not
        // insert this session's history into the new one.
        if (!S.session || S.session.id !== sid || !inner.isConnected) return;
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
          btn.disabled = false; btn.textContent = "加载更早的消息";
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
      }).catch(function (e) {
        toast(e.message, true);
        if (btn) { btn.disabled = false; btn.textContent = "加载更早的消息"; }
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
      if (ev.seq > S.lastSeq) S.lastSeq = ev.seq;
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
    if (!opts.quiet && wasNear) scrollBottom(false);
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
      sum.textContent = "执行过程 · " + fold.length + " 步";
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
      if (s) s.textContent = "执行过程 · " + left + " 步";
    });
    // The trim note promises that the removed range can be loaded again. Make
    // that true: move the older cursor up to the first node still on the page
    // and forget the removed seqs, otherwise "加载更早的消息" would fetch
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
      n.textContent = "为保持流畅，较早的消息已从页面移除；可继续「加载更早的消息」查看服务端历史。";
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
        wrap.innerHTML = '<div class="msg assistant"><div class="bubble">' + md(p.text || "") + "</div></div>";
        break;
      case "thinking":
        wrap.innerHTML = '<details class="thinking"><summary>已思考</summary><div class="thinking-body" data-lazy-text></div></details>';
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
        wrap.innerHTML = '<details class="thinking tool-result"><summary>工具结果 · ' +
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
        wrap.innerHTML = '<div class="permission-card kind-plan codex-plan"><div class="permission-title"><span class="dot"></span>计划</div>' +
          '<div class="plan-body bubble">' + md(p.text || "") + "</div>" +
          '<div class="perm-opts"><button class="perm-opt primary" type="button" data-run-plan="1"><span class="perm-opt-label">按此计划执行</span>' +
          '<span class="perm-opt-detail">切换到「自动」模式并开始执行</span></button></div></div>';
        break;
      }
      case "status": {
        var st = p.status || "";
        wrap.innerHTML = '<div class="status-line">' + esc(statusLabel(st)) +
          (p.reason && p.reason !== "exited" ? " · " + esc(reasonLabel(p.reason)) : "") + "</div>";
        break;
      }
      case "notice":
        if (/^已为这个对话保存/.test(p.text || "")) refreshSessionSettings();
        wrap.innerHTML = '<div class="notice">' + esc(p.text || "") + "</div>";
        break;
      case "stderr":
        wrap.innerHTML = '<details class="notice stderr"><summary>Agent stderr</summary>' +
          "<div>" + esc(p.text || "") + "</div></details>";
        break;
      case "error":
        wrap.innerHTML = '<div class="error-block">' + esc(p.text || "错误") + "</div>";
        break;
      case "permission_denied":
        wrap.innerHTML = '<div class="notice">权限被拒绝：' + esc(p.tool_name || "") +
          (p.message ? " · " + esc(p.message) : "") + "</div>";
        break;
      case "usage": {
        var u = p.usage || {};
        var bits = [];
        if (u.input_tokens) bits.push("输入 " + u.input_tokens);
        if (u.output_tokens) bits.push("输出 " + u.output_tokens);
        if (u.cached_input_tokens) bits.push("缓存 " + u.cached_input_tokens);
        if (!bits.length) return null;
        wrap.innerHTML = '<div class="meta-line">' + esc(bits.join(" · ")) + "</div>";
        break;
      }
      case "result": {
        var meta = [];
        if (p.duration_ms) meta.push((p.duration_ms / 1000).toFixed(1) + "s");
        if (p.cost_usd !== undefined && p.cost_usd !== null) meta.push("$" + Number(p.cost_usd).toFixed(4));
        if (p.num_turns) meta.push(p.num_turns + " 轮");
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
    return ({ cancelled: "用户取消", signaled: "Agent 进程异常退出", exited: "正常退出", lost: "进程丢失" })[r] || r;
  }

  function toolNode(p) {
    var node = document.createElement("div");
    node.className = "tool";
    node.setAttribute("data-tool-id", p.id || "");
    var statusText = p.status === "running" || p.status === "in_progress" ? "运行中" :
      p.status === "failed" || p.status === "error" ? "失败" : p.status === "completed" ? "完成" : "";
    var isErr = statusText === "失败";
    var hasOut = p.output !== undefined && p.output !== null && p.output !== "";
    node.innerHTML =
      '<div class="tool-head"><span class="tool-name">' + esc(toolLabel(p.name)) + "</span>" +
      '<span class="tool-summary">' + esc(toolSummary(p)) + "</span>" +
      '<span class="tool-state' + (isErr ? " err" : "") + '">' + esc(statusText) + "</span></div>" +
      '<div class="tool-body">' +
      '<div class="tool-block"><div class="tool-label">输入</div><pre class="tool-input" data-lazy-text></pre></div>' +
      (hasOut
        ? '<div class="tool-block"><div class="tool-label">输出</div><pre class="tool-output" data-lazy-text></pre></div>'
        : '<div class="tool-block tool-out-block" hidden><div class="tool-label">输出</div><pre class="tool-output" data-lazy-text></pre></div>') +
      (p.exit_code !== undefined && p.exit_code !== null && p.exit_code !== 0
        ? '<div class="small" style="color:var(--red)">exit ' + esc(String(p.exit_code)) + "</div>" : "") +
      "</div>";
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
      state.textContent = p.status === "running" || p.status === "in_progress" ? "运行中" :
        failed ? "失败" : p.status === "completed" ? "完成" : "";
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
      el.textContent = c.preview + "\n…（完整内容加载失败：" + e.message + "）";
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
        return truncate("[" + v.length + " 项] " + first, n);
      }
      var keys = Object.keys(v);
      var bits = [];
      for (var i = 0; i < keys.length && bits.length < 3; i++) {
        var val = v[keys[i]];
        if (typeof val === "string" || typeof val === "number" || typeof val === "boolean") {
          bits.push(keys[i] + "=" + truncate(String(val), 40));
        }
      }
      return truncate(bits.length ? "{ " + bits.join(", ") + " }" : "{" + keys.length + " 个字段}", n);
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
    else setLazyText(out, pretty(p.output) || "(无输出)", true);
    var state = node.querySelector(".tool-state");
    if (state) {
      state.textContent = p.is_error ? "失败" : "完成";
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
      description: src.description || d.description || "",
      options: (src.options && src.options.length ? src.options : d.options) || null,
      questions: (src.questions && src.questions.length ? src.questions : d.questions) || [],
      plan: src.plan || d.plan || ""
    };
  }

  var DEFAULT_OPTIONS = [{ id: "allow", label: "允许" }, { id: "deny", label: "拒绝" }];

  function optionButton(o) {
    var cls = o.id === "allow" || o.id === "approve_accept_edits" ? "primary" :
      (o.id === "deny" || o.id === "deny_stop") ? "danger" : "";
    return '<button class="perm-opt ' + cls + '" type="button" data-perm-opt="' + esc(o.id) + '">' +
      '<span class="perm-opt-label">' + esc(o.label) + "</span>" +
      (o.detail ? '<span class="perm-opt-detail">' + esc(o.detail) + "</span>" : "") + "</button>";
  }

  function permissionNode(src) {
    var perm = permModel(src);
    var node = document.createElement("div");
    node.className = "permission-card kind-" + perm.kind;
    node.setAttribute("data-permission-id", perm.id);
    var body = "";
    if (perm.kind === "question") {
      body = '<div class="permission-title"><span class="dot"></span>Agent 在提问</div>' +
        perm.questions.map(function (q, qi) {
          return '<div class="q-block" data-q="' + qi + '">' +
            (q.header ? '<div class="q-header">' + esc(q.header) + "</div>" : "") +
            '<div class="q-text">' + esc(q.question) + (q.multi ? ' <span class="muted small">（可多选）</span>' : "") + "</div>" +
            '<div class="q-options">' + (q.options || []).map(function (o, oi) {
              return '<button type="button" class="q-opt" data-q-opt="' + oi + '">' +
                '<span class="perm-opt-label">' + esc(o.label) + "</span>" +
                (o.description ? '<span class="perm-opt-detail">' + esc(o.description) + "</span>" : "") + "</button>";
            }).join("") + "</div>" +
            (q.other ? '<input class="q-other" type="' + (q.secret ? "password" : "text") +
              '" placeholder="' + ((q.options || []).length ? "其它（自己输入）" : "输入你的回答") + '" maxlength="2000">' : "") +
            "</div>";
        }).join("") +
        '<div class="permission-actions"><button class="btn primary sm" type="button" data-perm="submit" disabled>提交回答</button>' +
        '<button class="btn sm ghost" type="button" data-perm="skip">跳过</button></div>';
    } else if (perm.kind === "plan") {
      body = '<div class="permission-title"><span class="dot"></span>计划待批准</div>' +
        '<div class="plan-body bubble">' + md(perm.plan || "") + "</div>" +
        '<div class="perm-opts">' + (perm.options || []).map(optionButton).join("") + "</div>" +
        '<div class="permission-reason" hidden><textarea rows="3" placeholder="希望怎么调整计划？（可选）" maxlength="2000"></textarea>' +
        '<div class="row" style="margin-top:7px;gap:7px"><button class="btn sm ghost grow" type="button" data-perm="reason-cancel">取消</button>' +
        '<button class="btn sm grow" type="button" data-perm="reason-confirm">继续规划</button></div></div>';
    } else {
      // A command reads best as the command itself (Bash, Codex shell).
      var input = perm.input && typeof perm.input.command === "string" ? perm.input.command : pretty(perm.input);
      body = '<div class="permission-title"><span class="dot"></span>需要批准：' + esc(toolLabel(perm.tool_name)) + "</div>" +
        (perm.description ? '<div class="permission-desc">' + esc(perm.description) + "</div>" : "") +
        (input ? '<div class="permission-input"><pre>' + esc(input) + "</pre></div>" : "") +
        '<div class="perm-opts">' + (perm.options || DEFAULT_OPTIONS).map(optionButton).join("") + "</div>" +
        '<div class="permission-reason" hidden><input type="text" placeholder="拒绝原因（可选，会告诉 Agent）" maxlength="300">' +
        '<div class="row" style="margin-top:7px;gap:7px"><button class="btn sm ghost grow" type="button" data-perm="reason-cancel">取消</button>' +
        '<button class="btn sm danger grow" type="button" data-perm="reason-confirm">确认拒绝</button></div></div>';
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
      toast(r.label || "已处理");
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
          res.textContent = (ok ? "✓ " : "✕ ") + info.label + (info.detail ? " · " + info.detail : "");
        } else {
          res.textContent = decision === "allowed" || decision === "allow" ? "✓ 已允许" :
            decision === "answered" ? "✓ 已回答" : decision === "expired" ? "已超时" : decision ? "✕ 已拒绝" : "已处理";
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
    var k = cacheKey(agent);
    if (S.models[k]) return Promise.resolve(S.models[k]);
    return api("/api/agents/" + encodeURIComponent(agent) + "/models").then(function (d) {
      S.models[k] = d;
      return d;
    });
  }

  function currentSettings() { return (S.session ? S.sessionSettings : S.draftSettings) || {}; }

  function syncModelBtn() {
    var st = currentSettings();
    var agent = currentAgentName();
    var d = S.models[cacheKey(agent)];
    var m = d && (d.models || []).filter(function (x) { return x.id === (st.model || ""); })[0];
    var label = st.model ? (m ? m.label : st.model) : "默认模型";
    $("#model-label").textContent = label + (st.effort ? " · " + st.effort : "");
  }

  function refreshSessionSettings() {
    if (!S.session) return;
    var sid = S.session.id;
    api("/api/sessions/" + encodeURIComponent(sid)).then(function (r) {
      if (S.session && S.session.id === sid) { S.sessionSettings = r.settings || {}; syncModelBtn(); }
    }).catch(function () {});
  }

  // Picking in the menu sets the conversation's settings directly, like the
  // CLI's /model picker; typing /model goes through the agent as in the CLI.
  function saveSettings(next) {
    if (!S.session) {
      S.draftSettings = next;
      syncModelBtn();
      return Promise.resolve();
    }
    return api("/api/sessions/" + encodeURIComponent(S.session.id), { method: "PATCH", body: { settings: next } })
      .then(function (r) { S.sessionSettings = r.settings || next; syncModelBtn(); });
  }

  function closeModelMenu() {
    $("#model-menu").hidden = true;
    $("#model-btn").setAttribute("aria-expanded", "false");
  }

  function openModelMenu() {
    var agent = currentAgentName(), menu = $("#model-menu");
    menu.innerHTML = '<div class="menu-item"><span class="spinner"></span> 读取模型列表…</div>';
    menu.hidden = false;
    $("#model-btn").setAttribute("aria-expanded", "true");
    loadModels(agent).then(function (d) {
      var st = currentSettings();
      var models = d.models || [];
      var cur = models.filter(function (m) { return m.id === (st.model || ""); })[0] ||
        models.filter(function (m) { return m.default; })[0];
      var efforts = (cur && cur.efforts && cur.efforts.length ? cur.efforts : d.efforts) || [];
      menu.innerHTML = '<div class="menu-section">模型</div>' + models.map(function (m) {
        var on = (st.model || "") === m.id || (!st.model && !m.id);
        return '<button type="button" class="menu-item' + (on ? " on" : "") + '" data-model="' + esc(m.id) + '">' +
          '<span class="menu-text"><span class="menu-label">' + esc(m.label) + (m.default && m.id ? "（默认）" : "") + "</span>" +
          (m.id ? '<span class="menu-detail mono">' + esc(m.id) + "</span>" : "") + "</span>" +
          '<svg class="ico sm menu-check" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg></button>';
      }).join("") + (efforts.length ? '<div class="menu-section">推理强度</div><div class="effort-row">' +
        ['<button type="button" class="chip' + (!st.effort ? " active" : "") + '" data-effort="">默认</button>']
          .concat(efforts.map(function (e) {
            return '<button type="button" class="chip' + (st.effort === e ? " active" : "") + '" data-effort="' + esc(e) + '">' + esc(e) + "</button>";
          })).join("") + "</div>" : "");
    }).catch(function (e) {
      menu.innerHTML = '<div class="menu-item">' + esc(e.message) + "</div>";
    });
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
      saveSettings(next).then(function () { toast("模型：" + $("#model-label").textContent); openModelMenu(); })
        .catch(function (err) { toast(err.message, true); });
      return;
    }
    var ef = e.target.closest("[data-effort]");
    if (ef) {
      saveSettings({ model: st.model || "", effort: ef.getAttribute("data-effort") })
        .then(function () { closeModelMenu(); toast("推理强度：" + (ef.getAttribute("data-effort") || "默认")); })
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
        (c.description ? '<span class="menu-detail">' + esc(c.description) + "</span>" : "") + "</span></button>";
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
    optimistic.title = "发送中…";
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
          toast("已开始新对话");
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
      if (d.queue_ahead > 0) toast("已排队，前面还有 " + d.queue_ahead + " 个任务");
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
    if (!agent || !agent.name || !agent.available) { toast("请先选择可用的 Agent", true); return; }
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
    $("#composer-hint").textContent = "正在创建对话…";

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
        if (d.queue_ahead > 0) toast("已排队，前面还有 " + d.queue_ahead + " 个任务");
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
    confirmSheet("停止任务？", "<p>将向 Agent 发送终止信号，正在执行的命令可能被中断。排在后面的消息会继续执行。</p>", "停止", true)
      .then(function (ok) {
        if (!ok) return;
        btn.disabled = true;
        api("/api/tasks/" + encodeURIComponent(act.id) + "/cancel", { method: "POST", body: {} })
          .then(function () { toast("正在停止…"); refreshActive().catch(function () {}); })
          .catch(function (err) { toast(err.message, true); btn.disabled = false; });
      });
  });

  $("#profile-select").addEventListener("change", function () {
    var agent = currentAgentName();
    var meta = profileMeta(agent, this.value);
    LS.set("profile." + agent, this.value);
    this.title = meta ? meta.detail : "";
    syncProfileBtn();
    if (meta && meta.dangerous) toast("Agent 将拥有完整权限，请谨慎使用", true);
  });

  // ------------------------------------------------------------ theme choice

  $("#main").addEventListener("click", function (e) {
    var themeChoice = e.target.closest("[data-theme-choice]");
    if (!themeChoice) return;
    applyTheme(themeChoice.getAttribute("data-theme-choice"));
    if (S.route === "settings") renderSettings();
  });

  // ----------------------------------------------------------------- settings

  function renderSettings() {
    S.route = "settings";
    $("#composer-wrap").hidden = true;
    document.body.classList.remove("has-composer");
    $("#conn-dot").hidden = true;
    $("#settings-btn").classList.add("active");
    setAgentPicker(false);
    setTop("系统设置", "");
    renderApprovalNotifications();
    $("#main").innerHTML = '<div class="pad"><div class="empty"><span class="spinner"></span> 加载中…</div></div>';
    Promise.all([api("/api/settings"), loadNodes()]).then(function (res) {
      var d = res[0];
      // The user may have navigated away while settings were loading; the
      // response must not repaint the page they are looking at now.
      if (S.route !== "settings") return;
      S.agents = d.agents || S.agents;
      S.config = d.config || S.config;
      renderSettingsData(d);
    }).catch(function (e) {
      if (S.route !== "settings") return;
      $("#main").innerHTML = '<div class="empty">加载失败：' + esc(e.message) + "</div>";
    });
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
      .map(function (x) { return x[1] + " " + c[x[0]]; });
    return out.join(" · ") || "暂无任务";
  }

  function renderSettingsData(d) {
    var cfg = d.config || {};
    // Agent rows expand in place; the summary line shows name, version and state.
    var agents = (d.agents || []).map(function (a) {
      var n = esc(a.name);
      var state = a.available ? (a.authenticated ? "可用" : "未认证") : "不可用";
      return '<details><summary>' +
        '<span class="agent-head">' + agentIcon(a.name) +
        '<span class="agent-name">' + esc(a.label || a.name) + "</span>" +
        '<span class="agent-ver">' + esc(shortVersion(a.version) || "—") + "</span></span>" +
        '<span class="badge ' + (a.available && a.authenticated ? "status completed" : a.available ? "status pending" : "status failed") + '">' +
        state + "</span></summary>" +
        '<div class="g-body"><dl class="agent-meta">' +
        "<dt>版本</dt><dd>" + esc(a.version || "—") + "</dd>" +
        "<dt>路径</dt><dd class=\"mono\">" + esc(a.binary || "—") + "</dd>" +
        "<dt>认证</dt><dd>" + esc(a.auth_detail || "—") + "</dd>" +
        "<dt>会话续接</dt><dd>" + (a.supports_resume ? "支持" : "不支持") + "</dd>" +
        "<dt>权限交互</dt><dd>" + (a.supports_permissions ? "支持网页审批" : "不支持（使用沙箱限制）") + "</dd>" +
        "<dt>说明</dt><dd>" + esc(a.permission_detail || "") + "</dd>" +
        "</dl>" +
        '<div class="agent-actions">' +
        '<button type="button" class="btn sm" data-agent-check="' + n + '" title="向 npm 查询最新版本">检查更新</button>' +
        '<button type="button" class="btn sm" data-agent-restart="' + n + '" title="重新检测配置，并重启该 Agent 正在运行的任务">重启 Agent</button>' +
        "</div>" +
        '<div class="agent-update small muted" data-agent-update="' + n + '"></div>' +
        "</div></details>";
    }).join("");

    var counts = d.counts || {};
    var addr = "http://" + (cfg.easytier_ip || location.hostname) + ":" + (cfg.port || "");

    var devices = '<div><div class="section-title">设备</div><div class="group">' + settingsDeviceRows() +
      '<button type="button" class="g-row link" data-add-device="1"><span class="g-label">添加服务器</span>' +
      '<span class="g-value">通过 SSH</span></button></div>' +
      '<div class="g-note">客户端通过 SSH 连接服务器并自动部署节点；服务器只需开放 SSH。' +
      (multiNode() ? "下面的 Agent、通用与服务设置属于当前设备「" + esc(nodeLabel(S.node)) + "」。" : "") + "</div></div>" +
      '<div><div class="section-title">密钥与迁移</div><div class="group">' +
      '<button type="button" class="g-row link" data-keys="1"><span class="g-label">密钥</span><span class="g-value">SSH 密钥库</span></button>' +
      '<button type="button" class="g-row link" data-export="1"><span class="g-label">导出设备</span><span class="g-value">加密文件</span></button>' +
      '<button type="button" class="g-row link" data-import="1"><span class="g-label">导入设备</span></button></div>' +
      '<div class="g-note">导出的文件用随机生成的解密码加密，可以把电脑上的服务器配置快速迁到手机。只包含 SSH 与节点凭据，不含任何 Agent 账号。</div></div>';
    $("#main").innerHTML = '<div class="pad stack settings">' + devices + (noDevice() ? "" :
      '<div><div class="section-title">Agent</div><div class="group">' + agents + "</div></div>") +

      '<div><div class="section-title">通用</div><div class="group">' +
      '<div class="g-row"><span class="g-label">外观</span><span class="seg">' +
      ["auto", "light", "dark"].map(function (t) {
        return '<button type="button" class="' + (S.theme === t ? "on" : "") + '" data-theme-choice="' + t + '">' +
          ({ auto: "自动", dark: "深色", light: "浅色" })[t] + "</button>";
      }).join("") + "</span></div>" + (noDevice() ? "</div></div>" :
      '<label class="g-row"><span class="g-label">默认工作目录</span>' +
      '<input id="set-cwd" class="g-input mono" value="' + esc(cfg.default_cwd || "") + '" spellcheck="false" autocomplete="off"></label>' +
      '<label class="g-row"><span class="g-label">最大并发任务</span>' +
      '<input id="set-max" class="g-input num" type="number" inputmode="numeric" min="1" max="32" value="' + esc(String(cfg.max_concurrent || "")) + '"></label>' +
      '</div><div class="g-note">修改后自动保存。并发上限 1–32。</div></div>' +

      '<div><div class="section-title">服务</div><div class="group">' +
      '<div class="g-row"><span class="g-label">访问地址</span><span class="g-value mono">' + esc(addr) + "</span></div>" +
      '<div class="g-row"><span class="g-label">版本</span><span class="g-value">' + esc(d.version || "") + " · 已运行 " + esc(dur(d.started_at)) + "</span></div>" +
      '<details><summary><span class="g-label">运行信息</span></summary><div class="g-body"><dl class="kv">' +
      "<dt>绑定</dt><dd class=\"mono\">" + esc((cfg.bind_addrs || []).join("  ")) + "</dd>" +
      "<dt>PID</dt><dd>" + esc(String((d.status || {}).pid || "")) + "</dd>" +
      "<dt>数据目录</dt><dd class=\"mono\">" + esc(cfg.data_dir || "") + "</dd>" +
      "<dt>数据库</dt><dd class=\"mono\">" + esc(cfg.db_path || "") + "</dd>" +
      "<dt>日志目录</dt><dd class=\"mono\">" + esc(cfg.log_dir || "") + "</dd>" +
      "<dt>允许根目录</dt><dd class=\"mono\">" + esc((cfg.allowed_roots || []).join("  ")) + "</dd>" +
      "<dt>任务</dt><dd>" + esc(countsSummary(counts)) + "</dd>" +
      "</dl></div></details>" +
      "</div></div>" +

      "") + (S.hubOnly ? "" :
      '<div><div class="section-title">账户</div><div class="group">' +
      '<details><summary><span class="g-label">修改密码</span></summary><div class="g-body">' +
      '<label class="field"><span>当前密码</span><input id="pw-old" type="password" autocomplete="current-password"></label>' +
      '<label class="field"><span>新密码</span><input id="pw-new" type="password" autocomplete="new-password"></label>' +
      '<label class="field"><span>确认新密码</span><input id="pw-new2" type="password" autocomplete="new-password"></label>' +
      '<button id="change-pw" class="btn primary block">修改密码</button>' +
      '<p class="small muted" style="margin:9px 0 0">修改后会轮换会话签名密钥，其它已登录设备需要重新登录。</p></div></details>' +
      '<button id="logout-btn" type="button" class="g-row danger">退出登录</button>' +
      "</div></div>") +

      '<div class="about">Agent Console ' + esc(d.version || "") +
      " · Agent 在各自的服务器上运行，关闭页面或应用不会中断任务；需要审批或回答的问题会出现在对话里。</div>" +
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
        box.innerHTML = '<span class="error-text">' + esc(u.error) + "</span>";
        return;
      }
      var head = "当前 " + (u.installed || "未安装") + " · 最新 " + (u.latest || "—");
      if (u.available) {
        box.innerHTML = head + ' · <span style="color:var(--amber)">有更新</span>' +
          '<div style="margin-top:8px"><button type="button" class="btn primary sm" data-agent-apply="' +
          esc(u.name) + '">' + (u.installed ? "更新到 " : "安装 ") + esc(u.latest || "") + "</button></div>" +
          (u.command ? '<div class="mono agent-cmd">' + esc(u.command) + "</div>" : "");
        var b = box.querySelector("[data-agent-apply]");
        if (b) b.addEventListener("click", function () { applyAgentUpdate(u, b); });
      } else {
        box.innerHTML = head + " · 已是最新" +
          (u.command ? '<div class="mono agent-cmd">' + esc(u.command) + "</div>" : "");
      }
    }

    function applyAgentUpdate(u, btn) {
      var label = agentLabels[u.name] || u.name;
      var cmd = u.command || "";
      confirmSheet("更新 " + label + "？",
        '<p>将执行：</p><pre class="mono agent-cmd">' + esc(cmd) + "</pre>" +
        "<p>安装期间不会中断正在运行的任务；新任务会使用新版本。</p>", "更新")
        .then(function (ok) {
          if (!ok) return;
          btn.disabled = true; btn.textContent = "更新中…";
          var box = agentUpdateBox(u.name);
          if (box) box.innerHTML = "<span>正在执行 " + esc(cmd) + "，请稍候…</span>";
          api("/api/agents/" + encodeURIComponent(u.name) + "/update", { method: "POST", body: {} })
            .then(function (r) {
              var v = (r.agent && r.agent.version) || r.version || "";
              toast(v ? "已更新：" + v : "更新完成");
              renderSettings();
            })
            .catch(function (e) {
              toast("更新失败", true);
              if (box) box.innerHTML = '<span class="error-text">' + esc(e.message) + "</span>";
              btn.disabled = false; btn.textContent = "重试更新";
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
          html = "<p>" + esc(label) + " 当前没有正在运行的任务。</p>" +
            "<p>点击后会重新检测版本、认证与模型。新消息本来就会读取最新的 config/settings，无需重启。</p>";
        } else {
          html = "<p>将结束并用相同内容重新运行 <b>" + running.length + "</b> 个正在运行或等待审批的任务，" +
            "新进程会读取最新的 config/settings。已经执行过的步骤可能重复执行。</p>" +
            (queued ? "<p>另有 " + queued + " 个排队任务不会被中断，它们启动时本就会使用新配置。</p>" : "");
        }
        return confirmSheet("重启 " + label + "？", html, "重启").then(function (ok) {
          if (!ok) return;
          btn.disabled = true; btn.textContent = "重启中…";
          return api("/api/agents/" + encodeURIComponent(name) + "/restart", { method: "POST", body: {} })
            .then(function (res) {
              if (res.restarted) toast("正在重启 " + res.restarted + " 个任务…");
              else toast("已重新检测 " + label + " 配置");
              renderSettings();
            });
        });
      }).catch(function (e) {
        toast(e.message, true);
      }).then(function () {
        btn.disabled = false; btn.textContent = "重启 Agent";
      });
    }

    Array.prototype.forEach.call(document.querySelectorAll("[data-agent-check]"), function (btn) {
      btn.addEventListener("click", function () {
        var name = btn.getAttribute("data-agent-check");
        var box = agentUpdateBox(name);
        btn.disabled = true; btn.textContent = "检查中…";
        if (box) box.innerHTML = "<span>正在向 npm 查询最新版本…</span>";
        api("/api/agents/updates").then(function (r) {
          var u = (r.updates || []).filter(function (x) { return x.name === name; })[0];
          if (u) renderAgentUpdate(u);
          else if (box) box.innerHTML = "未返回该 Agent 的版本信息";
        }).catch(function (e) {
          if (box) box.innerHTML = '<span class="error-text">' + esc(e.message) + "</span>";
        }).then(function () {
          btn.disabled = false; btn.textContent = "检查更新";
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
        toast("已保存");
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

    if (!S.hubOnly) $("#change-pw").addEventListener("click", function () {
      var oldp = $("#pw-old").value, np = $("#pw-new").value, np2 = $("#pw-new2").value;
      if (np !== np2) { toast("两次输入的新密码不一致", true); return; }
      if (!np) { toast("新密码不能为空", true); return; }
      var b = this; b.disabled = true; b.textContent = "修改中…";
      api("/api/password", { method: "POST", body: { old: oldp, new: np }, hub: true }).then(function (r) {
        S.csrf = r.csrf || S.csrf;
        toast("密码已修改，其它设备需重新登录");
        $("#pw-old").value = $("#pw-new").value = $("#pw-new2").value = "";
        b.disabled = false; b.textContent = "修改密码";
      }).catch(function (e) { toast(e.message, true); b.disabled = false; b.textContent = "修改密码"; });
    });

    if (!S.hubOnly) $("#logout-btn").addEventListener("click", function () {
      api("/api/logout", { method: "POST", body: {}, hub: true }).catch(function () {}).then(function () {
        LS.del("lastSession");
        showLogin();
      });
    });
  }

  // A restart is detected by a new started_at; an update additionally waits for
  // the new binary's build hash, so the page never reloads into the old process
  // (the old one answers for a moment before systemd stops it).
  // ------------------------------------------------------------- top actions

  $("#menu-btn").addEventListener("click", openSidebar);

  $("#settings-btn").addEventListener("click", function () {
    if (S.route === "settings") {
      var back = S.settingsReturn || "#/";
      S.settingsReturn = "";
      go(back);
    } else {
      S.settingsReturn = location.hash || "#/";
      go("#/settings");
    }
  });

  $("#approval-btn").addEventListener("click", function () {
    if ($("#approval-stack").hidden) showApprovalStack();
    else hideApprovalStack();
  });

  // On touch screens the native <select> opens the system's dated dialog; the
  // page's own picker replaces it, and the <select> stays the source of truth.
  $("#agent-select").addEventListener("mousedown", function (e) {
    if (!matchMedia("(pointer: coarse)").matches) return;
    e.preventDefault();
    var sel = this;
    pickList("Agent", $$("option", sel).map(function (o) {
      return { value: o.value, label: o.textContent, selected: o.value === sel.value, disabled: o.disabled };
    })).then(function (v) {
      if (v === null || v === sel.value) return;
      sel.value = v;
      sel.dispatchEvent(new Event("change"));
    });
  });

  $("#agent-select").addEventListener("change", function () {
    var name = this.value;
    if (!name) return;
    var a = agentMeta(name);
    S.draftAgent = name;
    LS.set("draftAgent", name);
    S.draftSettings = {}; // models differ per agent
    if (S.route !== "home") toast("已选择 " + (a.label || name) + "，开始新对话");
    newChat(name);
  });

  $("#sidebar-new").addEventListener("click", function () { newChat(); });
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
      if (!focusPermissionCard(pid)) toast("审批请求已失效");
    } else go("#/session/" + encodeURIComponent(sid));
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    if (!$("#profile-menu").hidden) closeProfileMenu();
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
    $("#prompt").value = "请按上面的计划执行。";
    b.disabled = true;
    sendMessage();
  });

  // Global click for code copy buttons.
  document.addEventListener("click", function (e) {
    var c = e.target.closest(".code-copy");
    if (!c) return;
    var block = c.closest(".code-block");
    var code = block ? block.querySelector("code") : null;
    if (!code) return;
    var text = code.textContent;
    var done = function () { c.textContent = "已复制"; setTimeout(function () { c.textContent = "复制"; }, 1400); };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done).catch(function () { fallbackCopy(text, done); });
    } else fallbackCopy(text, done);
  });
  function fallbackCopy(text, done) {
    var ta = document.createElement("textarea");
    ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
    document.body.appendChild(ta); ta.select();
    try { document.execCommand("copy"); done(); } catch (e) { toast("复制失败", true); }
    ta.remove();
  }

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
  fetch("/api/session", { credentials: "same-origin", cache: "no-store" })
    .then(function (r) { return r.json(); })
    .then(function (d) {
      if (d.authenticated) {
        S.csrf = d.csrf || "";
        if (d.must_change) { showForceChange(); return; }
        return boot();
      }
      S.defaultPassword = !!d.must_change;
      showLogin();
    })
    .catch(function () { showLogin(); });
})();
