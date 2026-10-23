/*
 * Translation 即时翻译 —— 主应用
 * ==========================================================================
 * 设计铁律（来自 dbx-todo 的事故沉淀，详见 AGENTS.md 第 4 章）：
 *
 *   1. 绝不用 $("id").属性 = ... 直接赋值。元素缺失时那是「对 null 赋值」
 *      → TypeError，而它通常位于绑定/boot 开头，会把整个界面连坐搞死
 *      （真实事故：所有按钮失效 + 数据从不落盘，根因是一个 id 拼错）。
 *      一律走 click(id,fn) / on(id,ev,fn) 安全绑定 + 页面级 error 兜底。
 *
 *   2. 永远不要把 $("字面量") 直接接属性访问 —— 打包脚本会扫描并拒绝。
 *      读取用 val(id)，写入用 setVal(id,v)，它们内部把 id 当变量传。
 *
 *   3. 所有数据落盘只走 window.TranslationStore（storage.js → 侧车 → SQLite）。
 *      绝不碰 localStorage（opaque origin 会抛 SecurityError），
 *      也绝不静默退化到内存而不告知用户。
 *
 *   4. 译文与用户输入一律 textContent 渲染，绝不 innerHTML（防 XSS）。
 *
 * 流式翻译的竞态防护（本文件最关键的正确性保证）：
 *   前端维护 currentTaskId，收到事件时若 taskId 不匹配则【直接丢弃】。
 *   没有这道过滤，快速连续输入时会出现「旧响应晚到覆盖新结果」——
 *   用户输入 "hello world" 的过程中，"hello" 的译文可能在
 *   "hello world" 的译文之后到达，把正确结果覆盖掉。
 */
(function () {
  "use strict";

  var App = window.App = window.App || {};
  var S = window.TranslationStore;
  var E = window.Engines;
  var H = window.History;

  /* ============ 功能开关（必须与 index.html 一致，打包脚本会校验）============ */
  var SHOW_DIAG_BAR = false;   // 置顶诊断条（默认关，诊断走菜单里的按钮）
  var ENABLE_IMPORT = true;    // 导入功能（默认开）

  /* ============ 状态 ============ */
  var state = {
    view: "translate",
    prefs: {},
    engines: [],
    currentEngineId: "",
    sourceLang: "auto",
    targetLang: "zh",
    // 流式任务
    taskId: "",            // 当前任务 ID，事件竞态过滤的依据
    streaming: false,
    streamBuffer: "",
    streamStart: 0,
    // 即时翻译
    instantTimer: null,
    lastInstantText: "",
    // 译文区自动滚动：用户手动上滚后暂停，避免"抢滚动条"
    autoScroll: true,
    bootDone: false
  };

  /* ============ 安全 DOM 助手 ============ */
  function $(id) { return document.getElementById(id); }
  function val(id) { var e = $(id); return e ? e.value : ""; }
  function setVal(id, v) { var e = $(id); if (e) { e.value = (v == null ? "" : v); } }
  function txt(id, v) { var e = $(id); if (e) { e.textContent = (v == null ? "" : String(v)); } }
  function show(id, yes) { var e = $(id); if (e) { e.hidden = !yes; } }
  function setDisabled(id, yes) { var e = $(id); if (e) { e.disabled = !!yes; } }
  function setChecked(id, yes) { var e = $(id); if (e) { e.checked = !!yes; } }
  function isChecked(id) { var e = $(id); return e ? !!e.checked : false; }

  function click(id, fn) {
    var e = $(id);
    if (!e) { console.warn("[app] 缺少 #" + id); return; }
    e.addEventListener("click", function (ev) {
      try { fn(ev); } catch (err) { reportErr("点击 #" + id, err); }
    });
  }
  function on(id, ev, fn) {
    var e = $(id);
    if (!e) { console.warn("[app] 缺少 #" + id); return; }
    e.addEventListener(ev, function (e2) {
      try { fn(e2); } catch (err) { reportErr("事件 #" + id + " " + ev, err); }
    });
  }
  function h(tag, props, children) {
    var e = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (k) {
        var v = props[k];
        if (v === null || v === undefined || v === false) { return; }
        if (k === "class") { e.className = v; }
        else if (k === "dataset") { Object.keys(v).forEach(function (d) { e.dataset[d] = v[d]; }); }
        else if (k === "text") { e.textContent = String(v); }
        else if (k.indexOf("on") === 0 && typeof v === "function") {
          e.addEventListener(k.slice(2).toLowerCase(), v);
        } else if (v === true) { e.setAttribute(k, ""); }
        else { e.setAttribute(k, String(v)); }
      });
    }
    if (children != null) {
      var arr = Array.isArray(children) ? children : [children];
      arr.forEach(function (c) {
        if (c == null || c === false) { return; }
        e.appendChild(typeof c === "object" ? c : document.createTextNode(String(c)));
      });
    }
    return e;
  }

  function t(key, vars) {
    return (window.TranslationI18N && window.TranslationI18N.t)
      ? window.TranslationI18N.t(key, vars) : key;
  }

  /* ============ 全局错误兜底 ============
   * 页面级异常如果不接住，用户只会看到一片空白的界面，
   * 完全不知道该反馈什么。接住后至少能在诊断报告里留下证据。
   */
  function reportErr(where, err) {
    console.error("[Translation] " + where + ":", err);
    toast(t("genericFail", { msg: (err && err.message) || String(err) }), "error");
  }
  window.addEventListener("error", function (e) {
    console.error("[Translation] 页面错误:", e.error || e.message);
    if (S && S.log) { S.log("页面级异常", false, String(e.message || (e.error && e.error.message) || "")); }
  });
  window.addEventListener("unhandledrejection", function (e) {
    console.error("[Translation] 未捕获 Promise:", e.reason);
    if (S && S.log) { S.log("未捕获 Promise", false, String((e.reason && e.reason.message) || e.reason || "")); }
  });

  /* ============ 轻提示 ============ */
  var toastTimer = null;
  function toast(msg, kind, ms) {
    var el = $("toast");
    if (!el) { return; }
    el.textContent = String(msg == null ? "" : msg);
    el.className = "toast" + (kind ? " " + kind : "");
    el.hidden = false;
    if (toastTimer) { clearTimeout(toastTimer); }
    toastTimer = setTimeout(function () { el.hidden = true; }, ms || 2600);
  }

  /* ============ 通用确认弹框 ============
   * 沙箱 iframe 里原生 confirm() 不可用（会被宿主拦截或直接返回 false），
   * 必须自己实现。返回 Promise<boolean>。
   */
  var confirmResolve = null;
  function confirmDialog(opts) {
    return new Promise(function (resolve) {
      var titleEl = $("confirm-title");
      var linesEl = $("confirm-lines");
      var yesBtn = $("btn-confirm-yes");
      if (!titleEl || !linesEl || !yesBtn) { resolve(false); return; }

      titleEl.textContent = (opts && opts.title) || t("confirm");
      linesEl.textContent = "";
      ((opts && opts.lines) || []).forEach(function (ln) {
        if (typeof ln === "string") { linesEl.appendChild(h("p", { text: ln })); }
        else if (ln && ln.text) { linesEl.appendChild(h("p", { class: ln.cls || "", text: ln.text })); }
      });
      // 选项（用于导入模式、导出是否含密钥这类二选一）
      if (opts && opts.options && opts.options.length) {
        var wrap = h("div", { class: "confirm-options" });
        opts.options.forEach(function (o, i) {
          wrap.appendChild(h("label", {}, [
            h("input", { type: "radio", name: "confirm-opt", value: o.value, checked: i === 0 }),
            h("span", { text: o.label })
          ]));
        });
        linesEl.appendChild(wrap);
      }
      yesBtn.className = (opts && opts.danger) ? "danger" : "primary";
      yesBtn.textContent = (opts && opts.yesText) || t("confirm");

      confirmResolve = resolve;
      show("confirm-modal", true);
    });
  }

  function closeConfirm(result) {
    show("confirm-modal", false);
    var r = confirmResolve;
    confirmResolve = null;
    if (r) { r(result); }
  }

  // 读取确认弹框里选中的选项值
  function confirmChoice() {
    var checked = document.querySelector('input[name="confirm-opt"]:checked');
    return checked ? checked.value : "";
  }

  /* ============ 主题与字体：完整跟随宿主 DBX ============
   *
   * 设计目标：DBX 切换主题、调色板、UI 字体、编辑器字体、圆角风格时，
   * 本插件界面【实时】跟着变，无需重开工作台。参考 dbx-plugin-toolbox 的
   * src/lib/host.js applyTheme 实现。
   *
   * 官方文档「主题负载携带什么」明确了投递矩阵：
   *   浅色/深色/跟随系统 → theme.appearance
   *   调色板 + 自定义配色 → --color-* tokens
   *   圆角风格           → --radius-* tokens
   *   UI 字体            → --font-sans token
   *   编辑器（等宽）字体  → --font-mono token + theme.editor.fontFamily
   *   编辑器字号         → theme.editor.fontSize
   *
   * 两层机制配合：
   *   1. 自带完整明暗两套 CSS 变量作为【兜底】—— 开发宿主不投递 tokens 时
   *      界面仍有正确的明暗配色，不会变成一片白或一片黑。
   *   2. 宿主投递了 tokens 就用 setProperty 覆盖到文档根元素，实现精确跟随。
   *      tokens 缺失时不覆盖，兜底变量继续生效。
   */

  // 宿主可能只投递 --font-sans，而组件里用的是 --font-family（或反之）。
  // 建立别名对，任一存在就补齐另一个，避免「宿主给了字体但组件读不到」。
  // 这几对直接对齐 dbx-plugin-toolbox 的 ALIAS_PAIRS。
  var TOKEN_ALIAS_PAIRS = [
    ["--font-sans", "--font-family"],
    ["--font-mono", "--dbx-editor-font-family"],
    ["--dbx-editor-font-size", "--editor-font-size"]
  ];

  // 宿主 token → 本项目语义变量的映射。
  //
  // 为什么需要这一层：本项目 styles.css 用的是自己的语义变量名
  // （--bg / --panel / --text / --primary …），而宿主投递的是
  // DBX/shadcn 体系的 --color-background / --color-card / --color-foreground …。
  // 两套名字对不上，宿主投递的调色板就【完全不会生效】——
  // 只有明暗切换生效（因为 CSS 里有 body[data-theme="dark"] 两套兜底值），
  // 用户在 DBX 里自定义的主色、边框色、圆角都传不进来。
  //
  // 映射后：宿主投递了什么就覆盖什么，没投递的保持 CSS 里的兜底值。
  // 这样「跟随宿主」才是完整的（调色板 + 圆角 + 字体），而不只是明暗。
  //
  // 注意映射方向是【宿主 → 本项目】，且只在宿主确实投递了该 token 时才覆盖，
  // 否则会把兜底值冲成 undefined。
  var HOST_TOKEN_MAP = [
    // 背景与面板
    ["--color-background", "--bg"],
    ["--color-card", "--panel"],
    ["--color-muted", "--panel-2"],
    ["--color-accent", "--panel-3"],
    // 边框
    ["--color-border", "--border"],
    ["--color-input", "--border-strong"],
    // 文字
    ["--color-foreground", "--text"],
    ["--color-muted-foreground", "--text-2"],
    // 主色
    ["--color-primary", "--primary"],
    ["--color-primary-foreground", "--primary-fg"],
    // 语义色
    ["--color-destructive", "--danger"],
    ["--color-destructive-foreground", "--danger-fg"],
    ["--color-success", "--ok"],
    ["--color-warning", "--warn"],
    // 圆角
    ["--radius-md", "--radius"],
    ["--radius-sm", "--radius-sm"]
  ];

  // appliedTokens 记录上一次应用到根元素的 token 名，
  // 用于在主题切换后移除「这次不再投递」的 token，避免旧值残留。
  var appliedTokens = {};

  // flattenTokens 把可能嵌套的 tokens 对象拍平成 { "--css-name": "value" }。
  //
  // 宿主的 tokens 可能是 { color: { primary: "#fff" } } 这种嵌套结构，
  // 也可能是扁平的 { "--color-primary": "#fff" }。两种都要能处理。
  function flattenTokens(tokens, prefix, out) {
    out = out || {};
    if (!tokens || typeof tokens !== "object") { return out; }
    var keys = Object.keys(tokens);
    for (var i = 0; i < keys.length; i++) {
      var key = keys[i];
      var value = tokens[key];
      var name = prefix ? (prefix + "-" + key) : key;
      if (value && typeof value === "object" && !Array.isArray(value)) {
        // 嵌套对象：若其值全是标量则递归拍平，否则跳过（不认识的深层结构不强解）
        flattenTokens(value, name.replace(/^--+/, ""), out);
        continue;
      }
      var cssName = toCssVarName(name);
      var cssValue = toCssValue(value);
      if (cssName && cssValue) { out[cssName] = cssValue; }
    }
    return out;
  }

  // toCssVarName 把 token 名规范成合法的 CSS 变量名（--xxx-yyy）。
  // 非法名（含空格、以数字开头等）返回空串，调用方据此跳过 —— 防止
  // setProperty 抛异常或注入畸形样式。
  function toCssVarName(name) {
    var raw = String(name == null ? "" : name).trim();
    if (!raw) { return ""; }
    var dashed = raw.indexOf("--") === 0 ? raw : ("--" + raw);
    return /^--[a-zA-Z_][a-zA-Z0-9-]*$/.test(dashed) ? dashed : "";
  }

  // toCssValue 只接受有限的字符串与数字，其余一律拒绝。
  // 这是防注入的关键：token 值来自宿主，虽然可信，但仍不应把
  // 任意字符串塞进 style（例如含 "}" 或 ";" 的值会破坏样式表）。
  function toCssValue(value) {
    if (typeof value === "number" && isFinite(value)) { return String(value); }
    if (typeof value === "string") {
      var v = value.trim();
      // 拒绝含 CSS 语法字符的值，避免样式注入
      if (v && !/[;{}<>]/.test(v)) { return v; }
    }
    return "";
  }

  function applyTheme() {
    var theme = (S && typeof S.themeObject === "function")
      ? S.themeObject()
      : { appearance: (S && S.theme ? S.theme() : "light"), tokens: {}, editor: null };

    var appearance = theme.appearance === "dark" ? "dark" : "light";
    var root = document.documentElement;

    // 1) 外观：同时设置多个约定位置，兼容不同 CSS 选择器写法
    //    （本项目用 body[data-theme]，但也设 root 的 dataset 与 class 以防万一）
    document.body.dataset.theme = appearance;
    root.dataset.dbxTheme = appearance;
    if (root.classList) {
      if (appearance === "dark") { root.classList.add("dark"); }
      else { root.classList.remove("dark"); }
    }
    // colorScheme 让原生控件（滚动条、表单）也跟随明暗
    try { root.style.colorScheme = appearance; } catch (e) { /* 老浏览器忽略 */ }

    // 2) tokens：把宿主投递的颜色/字体/圆角变量应用到根元素
    var flat = flattenTokens(theme.tokens, "", {});

    // 编辑器字体/字号可能不在 tokens 里，而在 theme.editor 结构里，单独提取
    if (theme.editor) {
      if (theme.editor.fontFamily) {
        var ef = toCssValue(theme.editor.fontFamily);
        if (ef) { flat["--font-mono"] = ef; }
      }
      if (theme.editor.fontSize) {
        var es = toCssValue(theme.editor.fontSize);
        // fontSize 可能是纯数字（px）或带单位，纯数字补 px
        if (es) { flat["--editor-font-size"] = /^\d+(\.\d+)?$/.test(es) ? (es + "px") : es; }
      }
    }

    // 别名补齐：--font-sans ↔ --font-family 等
    for (var a = 0; a < TOKEN_ALIAS_PAIRS.length; a++) {
      var left = TOKEN_ALIAS_PAIRS[a][0], right = TOKEN_ALIAS_PAIRS[a][1];
      if (flat[left] && !flat[right]) { flat[right] = flat[left]; }
      else if (flat[right] && !flat[left]) { flat[left] = flat[right]; }
    }

    // 宿主 token → 本项目语义变量。
    // 只在宿主确实投递了该 token 时才映射，避免把 CSS 兜底值冲掉。
    var mapped = 0;
    for (var m = 0; m < HOST_TOKEN_MAP.length; m++) {
      var hostName = HOST_TOKEN_MAP[m][0], localName = HOST_TOKEN_MAP[m][1];
      if (flat[hostName]) { flat[localName] = flat[hostName]; mapped++; }
    }

    // 应用本次的 token
    var names = Object.keys(flat);
    for (var n = 0; n < names.length; n++) {
      try { root.style.setProperty(names[n], flat[names[n]]); } catch (e) { /* 单个失败不影响其余 */ }
    }
    // 移除上一次应用过、这次不再投递的 token（主题切换后避免旧值残留）
    for (var prev in appliedTokens) {
      if (Object.prototype.hasOwnProperty.call(appliedTokens, prev) && !(prev in flat)) {
        try { root.style.removeProperty(prev); } catch (e) { /* 忽略 */ }
      }
    }
    appliedTokens = flat;

    // 记录到诊断，便于排查「主题没跟随」时确认宿主到底投递了什么
    if (S && S.log) {
      S.log("应用宿主主题", true,
        "appearance=" + appearance + " tokens=" + names.length +
        " 映射到语义变量=" + mapped +
        (flat["--font-sans"] ? " font-sans=有" : " font-sans=无(用兜底)") +
        (flat["--font-mono"] ? " font-mono=有" : " font-mono=无(用兜底)"));
    }
  }

  // renderAppearanceInfo 在设置页只读展示当前跟随到的外观信息。
  //
  // 主题与语言一律跟随宿主，不提供手动覆盖入口（用户要求）。
  // 但仍展示「当前跟随到什么」，让用户确认插件确实跟上了 DBX 的设置，
  // 也便于排查「为什么我的插件是深色而 DBX 是浅色」这类问题。
  function renderAppearanceInfo() {
    var theme = (S && typeof S.themeObject === "function") ? S.themeObject() : null;
    var appearance = theme ? theme.appearance : (S && S.theme ? S.theme() : "light");
    var tokenCount = theme ? Object.keys(flattenTokens(theme.tokens, "", {})).length : 0;
    var fontSans = theme ? (flattenTokens(theme.tokens, "", {})["--font-sans"] || "") : "";
    var locale = (window.TranslationI18N && window.TranslationI18N.locale)
      ? window.TranslationI18N.locale() : "zh-CN";

    txt("info-theme", (appearance === "dark" ? t("themeDark") : t("themeLight")) +
      "  ·  " + t("followHost"));
    txt("info-locale", locale === "en" ? "English" : "简体中文");
    txt("info-font", fontSans || t("fontFollowHost"));
    txt("info-token-count", String(tokenCount));
  }

  /* ============ 视图切换 ============ */

  var VIEWS = ["translate", "engines", "history", "settings"];

  function switchView(name) {
    if (VIEWS.indexOf(name) < 0) { name = "translate"; }
    state.view = name;
    VIEWS.forEach(function (v) {
      show("view-" + v, v === name);
      var btn = $("btn-view-" + v);
      if (btn) {
        if (v === name) { btn.classList.add("active"); btn.setAttribute("aria-selected", "true"); }
        else { btn.classList.remove("active"); btn.setAttribute("aria-selected", "false"); }
      }
    });
    // 切到历史页时刷新（可能在别的页面翻译过）
    if (name === "history" && H) { H.load(true); }
    if (name === "engines" && E) { E.reload(); }
    if (name === "settings") { renderSettings(); }
    // 记住上次视图，重开工作台时恢复
    if (S && S.isPersistent() && state.prefs.lastView !== name) {
      state.prefs.lastView = name;
      S.setPrefs({ lastView: name }).catch(function () { /* 偏好保存失败不影响使用 */ });
    }
  }

  /* ============ 语言下拉 ============ */

  function renderLangSelects() {
    var langs = (E && E.languages) ? E.languages() : [];
    var kind = currentEngineKind();

    fillLangSelect("sel-source-lang", langs, kind, true, state.sourceLang);
    fillLangSelect("sel-target-lang", langs, kind, false, state.targetLang);
    fillLangSelect("set-default-source", langs, kind, true, state.prefs.defaultSourceLang || "auto");
    fillLangSelect("set-default-target", langs, kind, false, state.prefs.defaultTargetLang || "zh");
  }

  // fillLangSelect 填充语言下拉框。
  //
  // 当前引擎不支持的语言会被【禁用】而不是隐藏 —— 隐藏会让用户以为
  // 「这个插件不支持日语」，禁用并标注原因才能引导他去换引擎。
  function fillLangSelect(id, langs, kind, allowAuto, selected) {
    var sel = $(id);
    if (!sel) { return; }
    sel.textContent = "";
    var supported = {};
    if (kind && E && E.kindInfo) {
      var info = E.kindInfo(kind);
      (info && info.languages ? info.languages : []).forEach(function (c) { supported[c] = true; });
    } else {
      // 没有引擎时全部可选（新建引擎后才会真正校验）
      langs.forEach(function (l) { supported[l.code] = true; });
    }

    langs.forEach(function (l) {
      if (l.code === "auto" && !allowAuto) { return; }
      var ok = supported[l.code] !== false;
      var name = isZh() ? l.zh : l.en;
      sel.appendChild(h("option", {
        value: l.code,
        text: ok ? name : (name + "（当前引擎不支持）"),
        disabled: !ok,
        selected: l.code === selected
      }));
    });

    // 选中的语言若不被支持，回退到第一个可用项
    if (selected && supported[selected] === false) {
      for (var i = 0; i < langs.length; i++) {
        var c = langs[i].code;
        if (c === "auto" && !allowAuto) { continue; }
        if (supported[c] !== false) { sel.value = c; onLangChanged(id, c); break; }
      }
    }
  }

  function isZh() {
    return !(window.TranslationI18N && window.TranslationI18N.locale() === "en");
  }

  function onLangChanged(id, code) {
    if (id === "sel-source-lang") { state.sourceLang = code; }
    else if (id === "sel-target-lang") { state.targetLang = code; }
  }

  function currentEngineKind() {
    var e = currentEngine();
    return e ? e.kind : "";
  }

  function currentEngine() {
    if (state.currentEngineId && E) {
      var found = E.findById(state.currentEngineId);
      if (found) { return found; }
    }
    return (E && E.defaultEngine) ? E.defaultEngine() : null;
  }

  /* ============ 引擎下拉 ============ */

  function renderEngineSelect() {
    var sel = $("sel-engine");
    if (!sel) { return; }
    var list = (E && E.enabledEngines) ? E.enabledEngines() : [];
    sel.textContent = "";

    if (!list.length) {
      sel.appendChild(h("option", { value: "", text: "（无可用引擎）" }));
      sel.disabled = true;
      state.currentEngineId = "";
      updateEngineBadge();
      updateNoEngineState();
      return;
    }
    sel.disabled = false;

    list.forEach(function (e) {
      sel.appendChild(h("option", {
        value: e.id,
        text: e.name + (e.isDefault ? " ★" : "")
      }));
    });

    // 保持用户之前的选择；若已不存在则回退到默认引擎
    var stillThere = false;
    for (var i = 0; i < list.length; i++) {
      if (list[i].id === state.currentEngineId) { stillThere = true; break; }
    }
    if (!stillThere) {
      var def = (E && E.defaultEngine) ? E.defaultEngine() : list[0];
      state.currentEngineId = def ? def.id : list[0].id;
    }
    sel.value = state.currentEngineId;
    updateEngineBadge();
    updateNoEngineState();
  }

  function updateEngineBadge() {
    var e = currentEngine();
    txt("engine-badge", e ? e.name : "—");
  }

  function updateNoEngineState() {
    var has = ((E && E.enabledEngines) ? E.enabledEngines() : []).length > 0;
    show("no-engine", !has);
    var panes = document.querySelector(".translate-panes");
    if (panes) { panes.hidden = !has; }
    var toolbar = document.querySelector(".translate-toolbar");
    if (toolbar) { toolbar.hidden = !has; }
    var actions = document.querySelector(".translate-actions");
    if (actions) { actions.hidden = !has; }
  }

  /* ============ 翻译执行 ============ */

  // doTranslate 执行一次翻译。
  //
  // instant=true 表示由「输入即译」触发：此时会先取消上一个在飞任务，
  // 并且长度超限时不报错、只在提示区说明（用户还在打字，弹错误太吵）。
  function doTranslate(instant) {
    var text = val("src-text");
    if (!text || !text.trim()) {
      if (!instant) { toast(t("emptySource"), "warn"); }
      return;
    }
    var eng = currentEngine();
    if (!eng) {
      if (!instant) { toast(t("noEngineTitle"), "warn"); }
      return;
    }
    if (!S.isSidecarAlive()) {
      toast(t("statusNoSidecar"), "error");
      return;
    }

    // 即时翻译的长度上限：超长文本自动翻译会打爆配额，
    // 而且用户还在打字，翻译到一半就被下一次输入取消了，纯属浪费。
    var maxAuto = Number(state.prefs.instantMaxLength || 2000);
    if (instant && text.length > maxAuto) {
      setHint(t("tooLongForInstant", { n: text.length }), "warn");
      return;
    }

    // 取消上一个在飞任务：这是即时翻译不浪费配额的关键。
    // 不取消的话，用户打一句话会触发十几次翻译请求，全部跑完。
    cancelCurrentTask(true);

    var useStream = isChecked("chk-stream") && state.prefs.streamEnabled !== false;
    setTranslating(true);
    resetTarget();
    state.streamBuffer = "";
    state.streamStart = Date.now();

    var payload = {
      engineId: eng.id,
      text: text,
      sourceLang: state.sourceLang || "auto",
      targetLang: state.targetLang || "zh"
    };

    if (useStream) {
      S.translateStart(payload).then(function (res) {
        if (!res || res.ok === false) {
          var msg = (res && res.message) || t("unknownError");
          setTranslating(false);
          setHint(msg, "error");
          return;
        }
        // 记录 taskId：后续所有事件都靠它做竞态过滤
        state.taskId = res.taskId || "";
        S.log("translate/start", true, "taskId=" + state.taskId);
      }).catch(function (err) {
        setTranslating(false);
        setHint((err && err.message) || String(err), "error");
      });
    } else {
      // 非流式：一次性拿完整结果
      state.taskId = "";
      S.translateRun(payload).then(function (res) {
        setTranslating(false);
        if (!res || res.ok === false) {
          setHint((res && res.message) || t("unknownError"), "error");
          return;
        }
        appendTarget(res.target || "", true);
        showMeta(res);
        setHint("", "");
        refreshHistoryIfVisible();
      }).catch(function (err) {
        setTranslating(false);
        setHint((err && err.message) || String(err), "error");
      });
    }
  }

  // cancelCurrentTask 取消当前在飞任务。
  //
  // silent=true 时不弹提示（用于「新翻译开始前取消旧的」这种内部场景，
  // 用户并没有主动点取消，弹提示会很吵）。
  function cancelCurrentTask(silent) {
    if (!state.taskId) { return; }
    var id = state.taskId;
    state.taskId = "";   // 立即清空，让后续事件被竞态过滤丢弃
    S.translateCancel(id).catch(function () { /* 取消失败不影响新任务 */ });
    if (!silent) {
      setTranslating(false);
      setHint(t("cancelled"), "warn");
    }
  }

  /* ============ 流式事件处理 ============
   * 这是整个前端最关键的正确性保证。
   *
   * 竞态场景：用户输入 "hello world"，防抖触发了两次翻译
   * （"hello" 和 "hello world"）。如果 "hello" 的响应晚到，
   * 没有 taskId 过滤就会把 "hello world" 的正确译文覆盖掉。
   *
   * 因此每个事件都先比对 taskId，不匹配直接丢弃。
   */
  function onSidecarEvent(method, params) {
    if (!params) { return; }
    // 竞态过滤：不属于当前任务的事件一律丢弃
    if (state.taskId && params.taskId && params.taskId !== state.taskId) {
      return;
    }

    if (method === "translate/chunk") {
      appendTarget(params.text || "", false);
      return;
    }

    if (method === "translate/end") {
      // 任务结束：清空 taskId，之后该任务的迟到事件会被上面的过滤丢弃
      state.taskId = "";
      setTranslating(false);
      // 用 end 事件里的完整译文覆盖增量拼接结果：
      // 侧车做过 cleanTranslation 清洗（剥离「译文：」前缀、包裹引号等），
      // 而增量拼接的原始流没有清洗过。以清洗后的为准。
      if (params.target) { setTarget(params.target); }
      showMeta(params);
      setHint("", "");
      refreshHistoryIfVisible();
      return;
    }

    if (method === "translate/error") {
      state.taskId = "";
      setTranslating(false);
      // -32034 是用户主动取消，不算错误，不弹红色提示
      if (params.code === -32034) {
        setHint(t("cancelled"), "warn");
      } else {
        setHint(params.message || t("unknownError"), "error");
      }
      return;
    }
  }

  /* ============ 译文区渲染 ============ */

  function resetTarget() {
    var el = $("dst-text");
    if (el) { el.textContent = ""; }
    show("dst-empty", true);
    txt("dst-count", "0");
    txt("translate-meta", "");
    state.autoScroll = true;
  }

  // appendTarget 增量追加译文。
  //
  // 用 textContent 拼接而非 innerHTML：译文来自不可信的上游模型，
  // 用 innerHTML 会让「模型返回 <img onerror=...>」变成 XSS。
  function appendTarget(text, isFinal) {
    var el = $("dst-text");
    if (!el) { return; }
    if (!text) { return; }
    show("dst-empty", false);
    state.streamBuffer += text;
    el.textContent = state.streamBuffer;
    txt("dst-count", String(state.streamBuffer.length));
    showCursor(!isFinal && state.streaming);
    if (state.autoScroll) { scrollDstToBottom(); }
  }

  function setTarget(text) {
    var el = $("dst-text");
    if (!el) { return; }
    state.streamBuffer = text || "";
    el.textContent = state.streamBuffer;
    txt("dst-count", String(state.streamBuffer.length));
    show("dst-empty", !state.streamBuffer);
    showCursor(false);
  }

  function showCursor(yes) {
    state.streaming = !!yes;
    show("dst-cursor", !!yes);
  }

  function scrollDstToBottom() {
    var wrap = $("dst-wrap");
    if (wrap) { wrap.scrollTop = wrap.scrollHeight; }
  }

  function showMeta(res) {
    if (!res) { return; }
    var parts = [];
    if (res.elapsedMs) { parts.push(res.elapsedMs + " ms"); }
    if (res.chunks && res.chunks > 1) { parts.push(res.chunks + " 段"); }
    if (res.engine) { parts.push(res.engine); }
    if (res.model) { parts.push(res.model); }
    txt("translate-meta", parts.join(" · "));
  }

  function setTranslating(yes) {
    setDisabled("btn-translate", yes);
    var btn = $("btn-translate");
    if (btn) { btn.textContent = yes ? t("translating") : t("translate"); }
    show("btn-cancel", yes);
    if (yes) { showCursor(true); }
  }

  function setHint(msg, kind) {
    var el = $("translate-hint");
    if (!el) { return; }
    el.textContent = msg || "";
    el.className = "hint" + (kind ? " " + kind : "");
  }

  function refreshHistoryIfVisible() {
    if (state.view === "history" && H) { H.load(); }
  }

  /* ============ 即时翻译 ============
   * 防抖 + 最小长度 + 长度上限三重控制。
   *
   * 为什么需要最小长度：用户刚打第一个字符就发请求，
   * 翻译 "h" 毫无意义，还白白消耗一次配额。
   */
  function onSourceInput() {
    var text = val("src-text");
    txt("src-count", String(text.length));

    if (!isChecked("chk-instant") || state.prefs.instantEnabled === false) { return; }

    var minChars = Number(state.prefs.instantMinChars || 2);
    var debounce = Number(state.prefs.instantDebounceMs || 600);

    if (state.instantTimer) { clearTimeout(state.instantTimer); state.instantTimer = null; }

    // 与上次自动翻译的文本相同就不重复触发
    // （例如用户只是移动了光标，内容没变）
    if (text === state.lastInstantText) { return; }
    if (text.trim().length < minChars) { return; }

    state.instantTimer = setTimeout(function () {
      state.instantTimer = null;
      var cur = val("src-text");
      if (cur === state.lastInstantText) { return; }
      state.lastInstantText = cur;
      doTranslate(true);
    }, debounce);
  }

  /* ============ 设置页 ============ */

  function renderSettings() {
    var st = S.status();
    txt("info-datadir", st.dataDir || "—");
    txt("info-dbpath", st.dbPath || "—");
    txt("info-version", st.sidecarVersion || "—");

    var scheme = st.secretScheme || "—";
    // 文案必须走 i18n：硬编码中文会让英文界面下仍显示中文，
    // 而这类问题在中文环境里测不出来，只有切到英文才暴露。
    var secure = st.secretSecure ? t("secretSecureYes") : t("secretSecureNo");
    txt("info-secret", scheme + " " + secure);

    setChecked("set-instant-enabled", state.prefs.instantEnabled !== false);
    setVal("set-instant-debounce", state.prefs.instantDebounceMs || 600);
    setVal("set-instant-minchars", state.prefs.instantMinChars || 2);
    setVal("set-instant-maxlength", state.prefs.instantMaxLength || 2000);
    setChecked("set-stream-enabled", state.prefs.streamEnabled !== false);
    setVal("set-chunk-delay", state.prefs.chunkDelayMs === undefined ? 1100 : state.prefs.chunkDelayMs);
    setVal("set-history-limit", state.prefs.historyLimit || 500);
    setVal("set-default-source", state.prefs.defaultSourceLang || "auto");
    setVal("set-default-target", state.prefs.defaultTargetLang || "zh");

    // 外观信息（只读展示，不可修改 —— 主题与语言一律跟随宿主 DBX）
    renderAppearanceInfo();
  }

  function saveSettings() {
    var payload = {
      instantEnabled: isChecked("set-instant-enabled"),
      instantDebounceMs: Number(val("set-instant-debounce")) || 600,
      instantMinChars: Number(val("set-instant-minchars")) || 2,
      instantMaxLength: Number(val("set-instant-maxlength")) || 2000,
      streamEnabled: isChecked("set-stream-enabled"),
      chunkDelayMs: Number(val("set-chunk-delay")) || 0,
      historyLimit: Number(val("set-history-limit")) || 500,
      defaultSourceLang: val("set-default-source") || "auto",
      defaultTargetLang: val("set-default-target") || "zh"
    };
    txt("settings-status", "");
    S.setPrefs(payload).then(function (res) {
      if (res && res.prefs) { state.prefs = res.prefs; }
      applyPrefsToUI();
      txt("settings-status", t("settingsSaved"));
      toast(t("settingsSaved"), "ok");
    }).catch(function (err) {
      txt("settings-status", (err && err.message) || String(err));
      reportErr("保存设置", err);
    });
  }

  // applyPrefsToUI 把偏好同步到翻译页的控件上。
  function applyPrefsToUI() {
    setChecked("chk-instant", state.prefs.instantEnabled !== false);
    setChecked("chk-stream", state.prefs.streamEnabled !== false);
    if (!state.sourceLang) { state.sourceLang = state.prefs.defaultSourceLang || "auto"; }
    if (!state.targetLang) { state.targetLang = state.prefs.defaultTargetLang || "zh"; }
  }

  /* ============ 状态条与警示条 ============ */

  function renderStatus() {
    var st = S.status();
    var dot = $("status-dot");
    var text = $("status-text");

    if (!st.sidecarAvailable) {
      if (dot) { dot.className = "status-dot err"; }
      if (text) { text.textContent = st.sidecarError || t("statusNoSidecar"); }
    } else if (!st.dbOk) {
      if (dot) { dot.className = "status-dot warn"; }
      if (text) { text.textContent = t("statusNoDb"); }
    } else {
      if (dot) { dot.className = "status-dot ok"; }
      if (text) { text.textContent = t("statusReady"); }
    }
    txt("status-version", "v" + (st.sidecarVersion || S.VERSION));

    // 未持久化警示条：数据只在内存里，刷新即丢。必须醒目告知。
    show("banner-not-persistent", !st.persistent);
    if (!st.persistent) {
      txt("banner-not-persistent-text",
        st.sidecarAvailable ? t("statusNoDb") : (st.sidecarError || t("statusNoSidecar")));
    }

    // 密钥仅混淆保护的警示条（secretSecure=false）
    show("banner-insecure-secret", st.sidecarAvailable === true && st.secretSecure === false);
    if (st.secretSecure === false && st.sidecarAvailable) {
      txt("banner-insecure-secret-text", t("insecureSecretBanner"));
    }
  }

  /* ============ 导出 / 导入 ============ */

  function actExport() {
    confirmDialog({
      title: t("exportTitle"),
      lines: [
        { text: t("exportQuestion") },
        { text: t("exportSecretWarning"), cls: "warn" }
      ],
      options: [
        { value: "no", label: t("exportWithoutSecrets") },
        { value: "yes", label: t("exportWithSecrets") }
      ],
      yesText: t("exportData"),
      danger: false
    }).then(function (yes) {
      if (!yes) { return; }
      var includeSecrets = confirmChoice() === "yes";
      S.exportData(includeSecrets).then(function (res) {
        if (!res || !res.ok) {
          toast((res && res.message) || t("unknownError"), "error");
          return;
        }
        return saveExported(res);
      }).catch(function (err) { reportErr("导出", err); });
    });
  }

  // saveExported 把导出内容交给用户。
  //
  // 优先走宿主的 saveFile（原生保存对话框）；开发宿主不支持它，
  // 此时降级为「复制到剪贴板」并明确告知用户要自己粘贴保存 ——
  // 绝不静默失败让用户以为导出成功了。
  function saveExported(res) {
    var bytes = base64ToBytes(res.dataBase64 || "");
    if (S.hasHostSave()) {
      return S.saveFile(res.fileName, "application/json", bytes).then(function (r) {
        if (r && r.ok) { toast(t("exportDone", { path: r.path || res.fileName }), "ok", 4000); }
        else if (r && r.canceled) { /* 用户取消，不提示 */ }
        else { toast((r && r.error) || t("unknownError"), "error"); }
      });
    }
    // 降级：复制到剪贴板
    return S.copyText(new TextDecoder("utf-8").decode(bytes)).then(function (ok) {
      toast(ok ? t("exportCopied") : t("copyFailed"), ok ? "warn" : "error", 6000);
    });
  }

  function base64ToBytes(b64) {
    var bin = atob(String(b64 || "").replace(/\s+/g, ""));
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i) & 0xff; }
    return out;
  }

  function actImport() {
    var input = $("file-input");
    if (!input) { return; }
    // 重置 value，否则连续选同一个文件不会触发 change
    input.value = "";
    input.click();
  }

  function onFileChosen(ev) {
    var input = ev.target;
    var file = input && input.files && input.files[0];
    if (!file) { return; }
    // 侧车限制导入数据 1.5MB，前端先拦一道，避免读完才发现太大
    if (file.size > S.LIMITS.maxUpstreamBytes) {
      toast("文件过大（" + Math.round(file.size / 1024) + " KB），超过 1.5 MB 上限", "error", 5000);
      return;
    }
    var reader = new FileReader();
    reader.onload = function () {
      var text = String(reader.result || "");
      var b64;
      try { b64 = arrayBufferToBase64(text); } catch (e) { reportErr("读取文件", e); return; }
      askImportMode(b64);
    };
    reader.onerror = function () { toast("读取文件失败", "error"); };
    reader.readAsText(file, "utf-8");
  }

  function arrayBufferToBase64(text) {
    // FileReader.readAsText 给的是字符串，直接对 UTF-8 字节做 base64
    var bytes = new TextEncoder().encode(text);
    var bin = "";
    for (var i = 0; i < bytes.length; i++) { bin += String.fromCharCode(bytes[i]); }
    return btoa(bin);
  }

  function askImportMode(b64) {
    confirmDialog({
      title: t("importTitle"),
      lines: [{ text: t("importQuestion") }],
      options: [
        { value: "merge", label: t("importMerge") },
        { value: "replace", label: t("importReplace") }
      ],
      yesText: t("importData"),
      danger: true
    }).then(function (yes) {
      if (!yes) { return; }
      var mode = confirmChoice() || "merge";
      S.importData(b64, mode).then(function (res) {
        if (!res || !res.ok) { toast((res && res.message) || t("unknownError"), "error"); return; }
        var c = res.counts || {};
        toast(t("importDone", {
          engines: c.engines || 0, history: c.history || 0, secrets: c.secrets || 0
        }), "ok", 5000);
        // 导入后所有缓存都失效了，全部重载
        return reloadAll();
      }).catch(function (err) { reportErr("导入", err); });
    });
  }

  /* ============ 诊断报告 ============ */

  function actDiag() {
    var el = $("diag-content");
    if (el) { el.textContent = S.report(); }
    show("diag-modal", true);
  }

  function actCopyDiag() {
    S.copyText(S.report()).then(function (ok) {
      toast(ok ? t("copied") : t("copyFailed"), ok ? "ok" : "error");
    });
  }

  /* ============ 重连 ============ */

  // reconnect 重新与侧车握手。
  //
  // 侧车可能被宿主重启（崩溃、更新、用户手动重启后端），
  // 此时前端的桥接对象还在但侧车是新进程，需要重新 ping 并刷新状态。
  function reconnect() {
    toast(t("reconnecting"), "warn");
    return S.ping().then(function () {
      var st = S.status();
      renderStatus();
      if (st.sidecarAvailable) {
        toast(t("reconnected"), "ok");
        return reloadAll();
      }
      toast(t("reconnectFailed"), "error");
    }).catch(function () {
      renderStatus();
      toast(t("reconnectFailed"), "error");
    });
  }

  function reloadAll() {
    return Promise.all([
      S.getPrefs().then(function (p) { state.prefs = p || {}; applyPrefsToUI(); }),
      (E ? E.reload() : Promise.resolve())
    ]).then(function () {
      renderEngineSelect();
      renderLangSelects();
      renderSettings();
      renderStatus();
      if (state.view === "history" && H) { H.load(true); }
    }).catch(function (err) { reportErr("重载数据", err); });
  }

  /* ============ 事件绑定 ============ */

  function bindEvents() {
    // 视图切换
    click("btn-view-translate", function () { switchView("translate"); });
    click("btn-view-engines", function () { switchView("engines"); });
    click("btn-view-history", function () { switchView("history"); });
    click("btn-view-settings", function () { switchView("settings"); });

    // 「更多」菜单
    click("btn-more", function (ev) {
      var menu = $("more-menu");
      var btn = $("btn-more");
      if (!menu || !btn) { return; }
      var willShow = menu.hidden;
      menu.hidden = !willShow;
      btn.setAttribute("aria-expanded", willShow ? "true" : "false");
      if (ev && ev.stopPropagation) { ev.stopPropagation(); }
    });
    // 点击页面其他地方收起菜单
    document.addEventListener("click", function (ev) {
      var menu = $("more-menu");
      if (!menu || menu.hidden) { return; }
      var wrap = document.querySelector(".more-wrap");
      if (wrap && wrap.contains(ev.target)) { return; }
      menu.hidden = true;
      var btn = $("btn-more");
      if (btn) { btn.setAttribute("aria-expanded", "false"); }
    });
    click("btn-swap-lang", function () { closeMore(); swapLang(); });
    click("btn-export", function () { closeMore(); actExport(); });
    click("btn-import", function () { closeMore(); actImport(); });
    click("btn-diag", function () { closeMore(); actDiag(); });

    function closeMore() {
      show("more-menu", false);
      var btn = $("btn-more");
      if (btn) { btn.setAttribute("aria-expanded", "false"); }
    }

    // 翻译页
    click("btn-translate", function () { doTranslate(false); });
    click("btn-cancel", function () { cancelCurrentTask(false); });
    click("btn-swap", swapLang);
    click("btn-clear-src", function () {
      setVal("src-text", "");
      txt("src-count", "0");
      state.lastInstantText = "";
      var el = $("src-text");
      if (el) { el.focus(); }
    });
    click("btn-clear-dst", function () { resetTarget(); setHint("", ""); });
    click("btn-copy-dst", function () {
      var text = state.streamBuffer || "";
      if (!text) { return; }
      S.copyText(text).then(function (ok) {
        toast(ok ? t("copied") : t("copyFailed"), ok ? "ok" : "error");
      });
    });
    click("btn-paste", actPaste);

    on("src-text", "input", onSourceInput);
    on("sel-source-lang", "change", function () {
      state.sourceLang = val("sel-source-lang");
    });
    on("sel-target-lang", "change", function () {
      state.targetLang = val("sel-target-lang");
    });
    on("sel-engine", "change", function () {
      state.currentEngineId = val("sel-engine");
      updateEngineBadge();
      // 换引擎后要重新算语言支持范围（不同引擎支持的语言不同）
      renderLangSelects();
    });
    on("chk-instant", "change", function () {
      var v = isChecked("chk-instant");
      state.prefs.instantEnabled = v;
      S.setPrefs({ instantEnabled: v }).catch(function () { /* 忽略 */ });
    });
    on("chk-stream", "change", function () {
      var v = isChecked("chk-stream");
      state.prefs.streamEnabled = v;
      S.setPrefs({ streamEnabled: v }).catch(function () { /* 忽略 */ });
    });

    // 译文区滚动：用户手动上滚后暂停自动滚动，避免"抢滚动条"
    on("dst-wrap", "scroll", function () {
      var wrap = $("dst-wrap");
      if (!wrap) { return; }
      var atBottom = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 40;
      state.autoScroll = atBottom;
    });

    // 设置页
    click("btn-save-settings", saveSettings);

    click("btn-reconnect", reconnect);
    click("btn-banner-retry", reconnect);
    click("btn-settings-export", actExport);
    click("btn-settings-import", actImport);
    click("btn-settings-diag", actDiag);

    // 诊断弹框
    click("btn-close-diag", function () { show("diag-modal", false); });
    click("btn-diag-copy", actCopyDiag);
    var diagModal = $("diag-modal");
    if (diagModal) {
      diagModal.addEventListener("click", function (ev) {
        if (ev.target && ev.target.getAttribute && ev.target.getAttribute("data-close") === "1") {
          show("diag-modal", false);
        }
      });
    }

    // 确认弹框
    click("btn-confirm-yes", function () { closeConfirm(true); });
    click("btn-confirm-no", function () { closeConfirm(false); });

    // 导入文件选择
    on("file-input", "change", onFileChosen);

    // 快捷键：Ctrl/Cmd+Enter 翻译，Esc 取消
    document.addEventListener("keydown", function (ev) {
      if ((ev.ctrlKey || ev.metaKey) && ev.key === "Enter") {
        ev.preventDefault();
        if (state.view === "translate") { doTranslate(false); }
        return;
      }
      if (ev.key === "Escape") {
        if (state.taskId) { cancelCurrentTask(false); return; }
        show("diag-modal", false);
      }
    });

    // 关闭工作台前把在飞任务取消掉，避免侧车白跑
    window.addEventListener("beforeunload", function () {
      if (state.taskId) {
        try { S.translateCancel(state.taskId); } catch (e) { /* 忽略 */ }
      }
    });

    // 宿主环境变化（主题 / 调色板 / 字体 / 语言切换）
    //
    // 【必须用 capture=true，且 window 与 document 都注册】——
    // 这是从 dbx-plugin-toolbox 的 src/lib/host.js 学来的关键细节，
    // 它的注释写得很明确：
    //
    //   "DBX env events target document and do not bubble;
    //    capture supports both current and older window-based hosts."
    //
    // 也就是说：新版宿主把 dbx-plugin-env 事件 target 在 document 上，
    // 而 document 是 window 的【祖先】，事件不冒泡的话，
    // 注册在 window 上的【冒泡阶段】监听器永远收不到。
    // 只有 capture 阶段（从 window 向下传播到 document）才能截获。
    //
    // 老宿主则是 target 在 window 上，此时 window 的 capture 与冒泡都能收到。
    // 所以两个目标 + capture 的组合能同时兼容新旧宿主。
    //
    // 漏掉这一点的症状是「DBX 切了主题，插件界面纹丝不动」，
    // 而且开发宿主可能行为不同，导致本地测不出来、装进桌面端才暴露。
    var onEnv = function (ev) {
      try {
        // 语言：重新探测宿主 locale 并刷新全部文案
        if (window.TranslationI18N) { window.TranslationI18N.detect(); }
        applyI18N();
        // 主题与字体：applyTheme 会重新读取完整 theme 负载（含 tokens）
        applyTheme();
        // 语言变了会影响动态生成的下拉选项文案，需要重绘
        renderLangSelects();
        renderEngineSelect();
        renderStatus();
        renderAppearanceInfo();
        var hostLoc = (window.TranslationI18N && window.TranslationI18N.hostLocale)
          ? window.TranslationI18N.hostLocale() : "?";
        S.log("宿主环境变化", true,
          "已刷新主题/字体/语言（宿主 locale=" + hostLoc +
          "，事件 target=" + (ev && ev.target ? (ev.target === document ? "document" : "window") : "?") + "）");
      } catch (e) { reportErr("响应环境变化", e); }
    };
    // capture=true：截获 target 在 document 上且不冒泡的事件
    window.addEventListener("dbx-plugin-env", onEnv, true);
    document.addEventListener("dbx-plugin-env", onEnv, true);

    // onInit 兜底：某些宿主在桥接就绪时只发 init 不发 env，
    // 此时也要应用一次主题，否则首屏可能用错外观。
    if (S && typeof S.onInit === "function") {
      try {
        S.onInit(function () {
          applyTheme();
          if (window.TranslationI18N) { window.TranslationI18N.detect(); }
          applyI18N();
        });
      } catch (e) { /* 老宿主无 onInit，忽略 */ }
    }
  }

  // swapLang 交换源语言与目标语言。
  //
  // 交换后若源语言变成 auto 是不允许的（目标语言不能是 auto），
  // 因此当目标是 auto 时改为用「上一次的源语言」，避免产生非法组合。
  function swapLang() {
    var src = state.sourceLang, dst = state.targetLang;
    if (src === "auto") {
      // 自动检测时无法交换：不知道源语言是什么，交换后目标就成了 auto
      toast("源语言为「自动检测」时无法交换，请先指定源语言", "warn", 3500);
      return;
    }
    state.sourceLang = dst;
    state.targetLang = src;
    setVal("sel-source-lang", state.sourceLang);
    setVal("sel-target-lang", state.targetLang);
    // 交换后若已有译文，把它搬回原文框，方便"回译校验"
    if (state.streamBuffer) {
      setVal("src-text", state.streamBuffer);
      txt("src-count", String(state.streamBuffer.length));
      resetTarget();
    }
  }

  // actPaste 从系统剪贴板粘贴。
  //
  // 沙箱里 navigator.clipboard 不可用（需要 host.clipboard:read 权限，
  // 本插件刻意没申请，保持权限最小化）。因此这里只能提示用户用 Ctrl+V ——
  // textarea 的原生粘贴事件在沙箱里是正常工作的。
  function actPaste() {
    var el = $("src-text");
    if (!el) { return; }
    el.focus();
    toast("请按 Ctrl+V（macOS 为 Cmd+V）粘贴", "warn", 3000);
  }

  // applyI18N 刷新所有带 data-i18n 的节点，并同步动态文本。
  function applyI18N() {
    if (window.TranslationI18N) { window.TranslationI18N.apply(document); }
    // 动态生成的文本不在 data-i18n 覆盖范围内，需要单独刷新
    var btn = $("btn-translate");
    if (btn && !state.streaming) { btn.textContent = t("translate"); }
    renderStatus();
  }

  /* ============ 启动 ============ */

  // boot 是启动入口。
  //
  // 【不用 dbxPlugin.ready.then(boot) 卡住启动】：如果宿主因为某种原因
  // 不发 init 消息，整个界面会永远停在空白状态，用户什么也看不到。
  // 正确做法是先渲染静态界面，再异步等桥接 —— 即使桥接失败，
  // 用户至少能看到界面和明确的错误提示。
  function boot() {
    S.log("=== 应用启动 ===", null, "UI 版本 " + S.VERSION);

    // 1. 先渲染静态界面（不依赖任何异步结果）
    if (window.TranslationI18N) { window.TranslationI18N.detect(); }
    applyI18N();
    applyTheme();
    bindEvents();
    renderStatus();

    if (E) { E.init(onEnginesChanged); }
    if (H) { H.init(onReuseHistory); }

    // 2. 异步初始化桥接与数据
    S.init().then(function () {
      renderStatus();
      applyTheme();

      // 桥接不可用：停在降级状态，界面已经渲染出来了，
      // 用户能看到红色警示条与诊断入口，不至于面对一片空白。
      if (!S.isSidecarAlive()) {
        S.log("侧车不可用，进入降级模式", false, S.status().sidecarError);
        state.bootDone = true;
        updateNoEngineState();
        return;
      }

      // 3. 加载偏好与引擎
      return S.getPrefs().then(function (prefs) {
        state.prefs = prefs || {};
        applyPrefsToUI();
        state.sourceLang = state.prefs.defaultSourceLang || "auto";
        state.targetLang = state.prefs.defaultTargetLang || "zh";
        return (E ? E.reload() : Promise.resolve());
      }).then(function () {
        renderEngineSelect();
        renderLangSelects();
        renderSettings();
        renderStatus();
        // 恢复上次视图
        var last = state.prefs.lastView;
        switchView(last && VIEWS.indexOf(last) >= 0 ? last : "translate");
        state.bootDone = true;
        S.log("=== 启动完成 ===", true, "视图=" + state.view);
        // 首屏聚焦原文框，用户打开就能直接打字
        var src = $("src-text");
        if (src && state.view === "translate") { try { src.focus(); } catch (e) { /* 忽略 */ } }
      });
    }).catch(function (err) {
      state.bootDone = true;
      S.log("启动失败", false, (err && err.message) || String(err));
      renderStatus();
    });

    // 4. 订阅侧车事件（流式翻译的通道）
    //    即使桥接失败也注册：注册本身不会报错，且桥接恢复后能立即生效。
    S.onSidecarEvent(onSidecarEvent);
  }

  function onEnginesChanged(engines) {
    state.engines = engines || [];
    renderEngineSelect();
    renderLangSelects();
    updateNoEngineState();
  }

  // onReuseHistory 处理「用这条历史重新翻译」：
  // 把原文与语言对填回翻译页并切过去。
  function onReuseHistory(item) {
    if (!item) { return; }
    setVal("src-text", item.source || "");
    txt("src-count", String((item.source || "").length));
    if (item.srcLang) { state.sourceLang = item.srcLang; setVal("sel-source-lang", item.srcLang); }
    if (item.dstLang) { state.targetLang = item.dstLang; setVal("sel-target-lang", item.dstLang); }
    if (item.engineId && E && E.findById(item.engineId)) {
      state.currentEngineId = item.engineId;
      setVal("sel-engine", item.engineId);
    }
    switchView("translate");
    resetTarget();
    var src = $("src-text");
    if (src) { try { src.focus(); } catch (e) { /* 忽略 */ } }
  }

  /* ============ 对外 API ============ */

  App.boot = boot;
  App.switchView = switchView;
  App.toast = toast;
  App.confirm = confirmDialog;
  App.translate = function () { doTranslate(false); };
  App.state = function () { return state; };
  App.reconnect = reconnect;

  // DOM 就绪后启动。
  //
  // 不用 dbxPlugin.ready 卡启动（理由见 boot 注释）。
  // 兼容「脚本在 DOM 解析完成后才注入」的情况（宿主会把入口脚本内联）。
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
