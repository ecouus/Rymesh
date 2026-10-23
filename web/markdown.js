/* Minimal, dependency-free Markdown renderer with a small syntax highlighter.
 * All input is HTML-escaped before formatting; URLs are restricted to http(s).
 * Exposes window.MD.render(text) -> HTML string.
 */
(function () {
  "use strict";

  // The page's language (i18n.js), for the few words this file writes.
  function T(s) { return window.I18N ? window.I18N.T(s) : s; }

  function esc(s) {
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  var LANGS = {
    js: "js", javascript: "js", ts: "js", typescript: "js", jsx: "js", tsx: "js",
    go: "go", golang: "go",
    py: "python", python: "python",
    json: "json",
    sh: "bash", bash: "bash", shell: "bash", zsh: "bash", console: "bash", terminal: "bash",
    sql: "sql",
    yaml: "yaml", yml: "yaml",
    css: "css", scss: "css",
    html: "html", xml: "html",
    c: "c", cpp: "c", "c++": "c", h: "c",
    rust: "rust", rs: "rust",
    java: "java", kotlin: "java", kt: "java",
    php: "php", ruby: "ruby", rb: "ruby"
  };

  var KEYWORDS = {
    js: "const let var function return if else for while do switch case break continue new class extends super this typeof instanceof in of try catch finally throw async await yield import export from default null undefined true false void delete static get set",
    go: "package import func var const type struct interface map chan go defer select case switch for range if else return break continue fallthrough nil true false int int64 int32 string bool byte rune error make new append len cap copy delete panic recover",
    python: "def class return if elif else for while in not and or is None True False import from as try except finally raise with lambda yield async await pass break continue global nonlocal del assert match case",
    json: "true false null",
    bash: "if then else elif fi for while do done case esac function return in export local readonly declare set unset echo cd pwd exit source alias sudo apt systemctl curl grep sed awk cat mkdir rm cp mv chmod chown ps kill sleep",
    sql: "select from where insert update delete create alter drop table index join left right inner outer on group by order desc asc limit offset values set into and or not null as union distinct having primary key foreign references default",
    yaml: "true false null yes no on off",
    css: "important media supports keyframes from to",
    rust: "fn let mut pub use mod struct enum impl trait for while loop if else match return self Self crate super where async await move ref dyn as const static unsafe type",
    java: "public private protected class interface extends implements static final void new return if else for while do switch case break continue try catch finally throw throws import package this super null true false int long double float boolean String",
    php: "function class public private protected static return if else elseif foreach for while do switch case break continue new echo require include namespace use null true false",
    ruby: "def class module end if elsif else unless while until for in do return yield begin rescue ensure raise require include attr_accessor nil true false self"
  };
  var KEYSET = {};
  Object.keys(KEYWORDS).forEach(function (k) {
    KEYSET[k] = Object.create(null);
    KEYWORDS[k].split(" ").forEach(function (w) { KEYSET[k][w] = true; });
  });

  function highlight(code, langRaw) {
    var lang = LANGS[(langRaw || "").toLowerCase()] || "";
    if (!lang || lang === "html") return esc(code);
    var kw = KEYSET[lang] || {};
    var out = "";
    var i = 0, n = code.length;
    var hashComment = lang === "python" || lang === "bash" || lang === "yaml" || lang === "ruby";
    var slashComment = lang === "js" || lang === "go" || lang === "c" || lang === "rust" ||
      lang === "java" || lang === "php" || lang === "css";
    var dashComment = lang === "sql";
    var backtick = lang === "js" || lang === "go";
    var triple = lang === "python";
    function isWord(c) { return /[A-Za-z0-9_$]/.test(c); }

    while (i < n) {
      var c = code[i];
      // block comments
      if (slashComment && c === "/" && code[i + 1] === "*") {
        var end = code.indexOf("*/", i + 2);
        end = end < 0 ? n : end + 2;
        out += '<span class="tok-com">' + esc(code.slice(i, end)) + "</span>";
        i = end;
        continue;
      }
      if (slashComment && c === "/" && code[i + 1] === "/") {
        var e2 = code.indexOf("\n", i);
        e2 = e2 < 0 ? n : e2;
        out += '<span class="tok-com">' + esc(code.slice(i, e2)) + "</span>";
        i = e2;
        continue;
      }
      if (hashComment && c === "#") {
        var e3 = code.indexOf("\n", i);
        e3 = e3 < 0 ? n : e3;
        out += '<span class="tok-com">' + esc(code.slice(i, e3)) + "</span>";
        i = e3;
        continue;
      }
      if (dashComment && c === "-" && code[i + 1] === "-") {
        var e4 = code.indexOf("\n", i);
        e4 = e4 < 0 ? n : e4;
        out += '<span class="tok-com">' + esc(code.slice(i, e4)) + "</span>";
        i = e4;
        continue;
      }
      // strings
      if (triple && code.substr(i, 3) === '"""') {
        var e5 = code.indexOf('"""', i + 3);
        e5 = e5 < 0 ? n : e5 + 3;
        out += '<span class="tok-str">' + esc(code.slice(i, e5)) + "</span>";
        i = e5;
        continue;
      }
      if (c === '"' || c === "'" || (c === "`" && backtick)) {
        var q = c, j = i + 1, buf = q;
        while (j < n) {
          var cc = code[j];
          buf += cc;
          if (cc === "\\" && j + 1 < n) { buf += code[j + 1]; j += 2; continue; }
          j++;
          if (cc === q) break;
        }
        out += '<span class="tok-str">' + esc(buf) + "</span>";
        i = j;
        continue;
      }
      // numbers
      if (/[0-9]/.test(c) && !(i > 0 && isWord(code[i - 1]))) {
        var j2 = i + 1;
        while (j2 < n && /[0-9a-fA-FxX._eE]/.test(code[j2])) j2++;
        out += '<span class="tok-num">' + esc(code.slice(i, j2)) + "</span>";
        i = j2;
        continue;
      }
      // words
      if (/[A-Za-z_$]/.test(c)) {
        var j3 = i + 1;
        while (j3 < n && isWord(code[j3])) j3++;
        var w = code.slice(i, j3);
        out += kw[w] ? '<span class="tok-kw">' + esc(w) + "</span>" : esc(w);
        i = j3;
        continue;
      }
      // everything else, escape character by character (runs are cheaper)
      var j4 = i + 1;
      while (j4 < n && !/[A-Za-z0-9_$"'`\/#-]/.test(code[j4])) j4++;
      out += esc(code.slice(i, j4));
      i = j4;
    }
    return out;
  }

  var PLACE = "\u0000";

  function inline(s) {
    var codeSpans = [];
    s = s.replace(/`([^`\n]+)`/g, function (_, code) {
      codeSpans.push("<code>" + code + "</code>");
      return PLACE + (codeSpans.length - 1) + PLACE;
    });
    s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, function (_, text, url) {
      return '<a href="' + url + '" target="_blank" rel="noopener noreferrer">' + text + "</a>";
    });
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/(^|[\s(])__([^_]+)__(?=$|[\s).,!?:;])/g, "$1<strong>$2</strong>");
    s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, "$1<em>$2</em>");
    // Intraword underscores (snake_case identifiers, UI_SMOKE_1) are literal.
    s = s.replace(/(^|[\s(])_([^_\n]+)_(?=$|[\s).,!?:;])/g, "$1<em>$2</em>");
    s = s.replace(/~~([^~]+)~~/g, "<del>$1</del>");
    s = s.replace(new RegExp(PLACE + "(\\d+)" + PLACE, "g"), function (_, idx) {
      return codeSpans[+idx];
    });
    return s;
  }

  function isTableSep(line) {
    return /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$/.test(line);
  }
  // A table starts at a pipe row whose next line is the separator. Detected as
  // a predicate so the paragraph collector can stop there too: otherwise a
  // table that follows a line of text (the common "label then table" shape)
  // is swallowed into the paragraph and rendered as literal pipes.
  function isTableStart(lines, i) {
    return i + 1 < lines.length && lines[i].indexOf("|") >= 0 && isTableSep(lines[i + 1]);
  }
  function splitRow(line) {
    var t = line.trim();
    if (t[0] === "|") t = t.slice(1);
    if (t[t.length - 1] === "|") t = t.slice(0, -1);
    return t.split("|").map(function (x) { return x.trim(); });
  }

  var LIST_ITEM = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
  var BLOCK_START = /^\s*(```|[-*+]\s|\d+[.)]\s|#{1,6}\s|>)/;

  function indentOf(s) {
    return s.match(/^[ \t]*/)[0].replace(/\t/g, "    ").length;
  }
  function dedent(s, n) {
    var k = 0, j = 0;
    while (j < s.length && k < n && (s[j] === " " || s[j] === "\t")) { k += s[j] === "\t" ? 4 : 1; j++; }
    return s.slice(j);
  }
  function sameKind(m, base, ordered) {
    return !!m && indentOf(m[1]) <= base + 1 && /\d/.test(m[2]) === ordered;
  }

  // parseList reads one list starting at lines[start]. An item owns every
  // following line indented past its marker (fenced code, nested lists, more
  // paragraphs), and blank lines between items keep the list going. Without
  // this a code block inside an item leaked out as literal backticks and the
  // next item restarted the numbering at "1.".
  function parseList(lines, start) {
    var first = lines[start].match(LIST_ITEM);
    var base = indentOf(first[1]);
    var ordered = /\d/.test(first[2]);
    var startNum = ordered ? parseInt(first[2], 10) : 1;
    var items = [];
    var i = start;
    while (i < lines.length) {
      var m = lines[i].match(LIST_ITEM);
      if (!sameKind(m, base, ordered)) break;
      var contentIndent = indentOf(m[1]) + m[2].length + 1;
      var body = [m[3]];
      var loose = false;
      var inFence = /^```/.test(m[3]);
      i++;
      while (i < lines.length) {
        var l = lines[i];
        if (inFence) {
          body.push(dedent(l, contentIndent));
          if (/^\s*```\s*$/.test(l)) inFence = false;
          i++;
          continue;
        }
        if (l.trim() === "") {
          // A blank line continues the item only if indented content follows.
          var j = i + 1;
          while (j < lines.length && lines[j].trim() === "") j++;
          if (j < lines.length && indentOf(lines[j]) >= base + 2) {
            for (; i < j; i++) body.push("");
            loose = true;
            continue;
          }
          break;
        }
        var ind = indentOf(l);
        if (ind < base + 2) break;
        var dl = dedent(l, Math.min(ind, contentIndent));
        if (/^```/.test(dl.trim())) inFence = true;
        body.push(dl);
        i++;
      }
      var simple = !loose && body.every(function (b) { return !BLOCK_START.test(b); });
      items.push("<li>" + (simple
        ? inline(esc(body.map(function (b) { return b.trim(); }).join(" ")))
        : render(body.join("\n"))) + "</li>");
      // Blank lines between two items of the same list do not end it.
      var k = i;
      while (k < lines.length && lines[k].trim() === "") k++;
      if (k > i && k < lines.length && sameKind(lines[k].match(LIST_ITEM), base, ordered)) i = k;
    }
    var open = ordered ? (startNum !== 1 ? '<ol start="' + startNum + '">' : "<ol>") : "<ul>";
    return { html: open + items.join("") + (ordered ? "</ol>" : "</ul>"), next: i };
  }

  function render(text) {
    if (!text) return "";
    text = String(text).replace(/\u0000/g, "").replace(/\r\n?/g, "\n");
    var lines = text.split("\n");
    var html = [];
    var i = 0;
    while (i < lines.length) {
      var line = lines[i];
      // fenced code
      var fence = line.match(/^\s*```\s*([A-Za-z0-9_+#.-]*)\s*$/);
      if (fence) {
        var lang = fence[1] || "";
        var buf = [];
        i++;
        while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) { buf.push(lines[i]); i++; }
        i++; // closing fence (or EOF)
        var code = buf.join("\n");
        html.push('<div class="code-block"><div class="code-head"><span>' + esc(lang || "text") +
          '</span><button class="code-copy" type="button" aria-label="' + esc(T("复制代码")) + '" title="' + esc(T("复制")) + '">' +
          '<svg class="ico i-copy" viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="11" height="11" rx="2.5"/><path d="M5 15V6.5A2.5 2.5 0 0 1 7.5 4H15"/></svg>' +
          '<svg class="ico i-done" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12.5 4.5 4.5L19 7.5"/></svg>' +
          '</button></div><pre><code>' +
          highlight(code, lang) + "</code></pre></div>");
        continue;
      }
      // heading
      var h = line.match(/^(#{1,6})\s+(.+?)\s*#*\s*$/);
      if (h) {
        var lv = h[1].length;
        html.push("<h" + lv + ">" + inline(esc(h[2])) + "</h" + lv + ">");
        i++;
        continue;
      }
      // hr
      if (/^\s*(?:---+|\*\*\*+|___+)\s*$/.test(line)) { html.push("<hr>"); i++; continue; }
      // blockquote
      if (/^\s*>\s?/.test(line)) {
        var q = [];
        while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
          q.push(lines[i].replace(/^\s*>\s?/, ""));
          i++;
        }
        html.push("<blockquote>" + render(q.join("\n")) + "</blockquote>");
        continue;
      }
      // table
      if (isTableStart(lines, i)) {
        var head = splitRow(line);
        i += 2;
        var rows = [];
        while (i < lines.length && lines[i].indexOf("|") >= 0 && lines[i].trim() !== "") {
          rows.push(splitRow(lines[i]));
          i++;
        }
        var t = "<table><thead><tr>";
        head.forEach(function (c) { t += "<th>" + inline(esc(c)) + "</th>"; });
        t += "</tr></thead><tbody>";
        rows.forEach(function (r) {
          t += "<tr>";
          for (var k = 0; k < head.length; k++) t += "<td>" + inline(esc(r[k] || "")) + "</td>";
          t += "</tr>";
        });
        html.push(t + "</tbody></table>");
        continue;
      }
      // lists
      var lm = line.match(LIST_ITEM);
      if (lm) {
        var list = parseList(lines, i);
        html.push(list.html);
        i = list.next;
        continue;
      }
      if (line.trim() === "") { i++; continue; }
      // paragraph
      var para = [];
      while (i < lines.length && lines[i].trim() !== "" &&
        !/^\s*```/.test(lines[i]) && !/^(#{1,6})\s+/.test(lines[i]) &&
        !/^\s*>\s?/.test(lines[i]) &&
        !/^\s*([-*+]|\d+[.)])\s+/.test(lines[i]) &&
        !/^\s*(?:---+|\*\*\*+|___+)\s*$/.test(lines[i]) &&
        !isTableStart(lines, i)) {
        para.push(lines[i]);
        i++;
      }
      html.push("<p>" + para.map(function (p) { return inline(esc(p)); }).join("<br>") + "</p>");
    }
    return html.join("");
  }

  window.MD = { render: render, highlight: highlight, esc: esc };
})();
