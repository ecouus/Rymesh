/* Interface language. The Chinese text in the code is the message id;
 * /i18n/en.json maps it to English, and a message with no entry stays as
 * written. Formats use Go's verbs ("已导入 %d 台设备"), so the page and the
 * server share one catalog (internal/i18n does the same on the Go side).
 *
 * T(format, args...) translates the page's own text. TR(message) translates
 * a finished message from a server (an error, a notice, a deploy log line):
 * it matches the message against the catalog's formats and translates the
 * filled-in parts too. Servers keep their messages in Chinese, so text a
 * node stored long ago, or a node on another version sent, is translated
 * here as well. */
(function () {
  "use strict";

  var VERB = /%(?:\[(\d+)\])?[-+# 0]*\d*(?:\.(\d+))?([a-zA-Z%])/g;
  var HAN = /[　-〿㐀-鿿＀-￯]/;
  var cat = null;
  var patterns = null;
  var lang = "zh";

  function systemLang() {
    var l = window.acSystemLang || (navigator.languages && navigator.languages[0]) || navigator.language || "";
    return /^zh/i.test(l) ? "zh" : "en";
  }

  // pref is the Settings choice: "zh", "en" or "" (follow the system).
  function load(pref) {
    lang = pref === "zh" || pref === "en" ? pref : systemLang();
    document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
    if (lang === "zh") { cat = null; patterns = null; return Promise.resolve(lang); }
    if (cat) return Promise.resolve(lang);
    return fetch("/i18n/en.json", { credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
      .then(function (c) { cat = c; patterns = null; return lang; })
      .catch(function () { lang = "zh"; document.documentElement.lang = "zh-CN"; return lang; });
  }

  function sprintf(format, args) {
    var next = 0;
    return format.replace(VERB, function (m, idx, prec, verb) {
      if (verb === "%") return "%";
      var i = idx ? +idx - 1 : next;
      next = i + 1;
      if (i >= args.length) return m;
      var a = args[i];
      if (verb === "f") return prec ? Number(a).toFixed(+prec) : String(Number(a));
      if (verb === "d") return String(Math.trunc(Number(a)));
      if (verb === "q") return JSON.stringify(String(a));
      return String(a);
    });
  }

  function T(format) {
    var args = Array.prototype.slice.call(arguments, 1);
    var f = (cat && cat[format]) || format;
    return args.length ? sprintf(f, args) : f;
  }

  function escapeRe(s) { return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); }

  function compile() {
    patterns = [];
    Object.keys(cat).forEach(function (k) {
      if (!cat[k]) return;
      var re = "^", last = 0, literal = 0, n = 0, m;
      VERB.lastIndex = 0;
      while ((m = VERB.exec(k))) {
        var lit = k.slice(last, m.index);
        literal += lit.length;
        re += escapeRe(lit);
        last = VERB.lastIndex;
        if (m[3] === "%") { re += "%"; literal++; continue; }
        n++;
        re += m[3] === "d" ? "(-?\\d+)" : m[3] === "f" || m[3] === "g" ? "(-?[\\d.]+)" : "([\\s\\S]*?)";
      }
      if (!n) return;
      var tail = k.slice(last);
      literal += tail.length;
      patterns.push({ re: new RegExp(re + escapeRe(tail) + "$"), target: cat[k], literal: literal, key: k });
    });
    patterns.sort(function (a, b) { return b.literal - a.literal || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0); });
  }

  function fill(format, args) {
    var next = 0;
    return format.replace(VERB, function (m, idx) {
      if (m === "%%") return "%";
      var i = idx ? +idx - 1 : next;
      next = i + 1;
      return i < args.length ? args[i] : m;
    });
  }

  function translate(msg, depth) {
    if (!cat || !msg || typeof msg !== "string" || depth > 4 || !HAN.test(msg)) return msg;
    if (cat[msg]) return cat[msg];
    // Lines first: a format's "%s" would otherwise swallow the next lines.
    if (msg.indexOf("\n") >= 0) {
      return msg.split("\n").map(function (l) { return translate(l, depth); }).join("\n");
    }
    if (!patterns) compile();
    for (var i = 0; i < patterns.length; i++) {
      var m = patterns[i].re.exec(msg);
      if (!m) continue;
      return fill(patterns[i].target, m.slice(1).map(function (a) { return translate(a, depth + 1); }));
    }
    return msg;
  }

  function TR(msg) { return translate(msg, 0); }

  // The static markup in index.html: text and the labels people read.
  function localize(root) {
    if (!cat) return;
    Array.prototype.forEach.call(root.querySelectorAll("[data-i18n-html]"), function (el) {
      var k = el.innerHTML.trim();
      if (cat[k]) el.innerHTML = cat[k];
    });
    var walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    var node;
    while ((node = walker.nextNode())) {
      var s = node.nodeValue, k = s.trim();
      if (k && HAN.test(k) && cat[k]) node.nodeValue = s.replace(k, cat[k]);
    }
    Array.prototype.forEach.call(root.querySelectorAll("[placeholder],[title],[aria-label]"), function (el) {
      ["placeholder", "title", "aria-label"].forEach(function (a) {
        var v = el.getAttribute(a);
        if (v && cat[v]) el.setAttribute(a, cat[v]);
      });
    });
  }

  window.I18N = {
    load: load, T: T, TR: TR, localize: localize, systemLang: systemLang,
    lang: function () { return lang; }
  };
})();
