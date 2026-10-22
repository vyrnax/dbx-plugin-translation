/*
 * 翻译助手 —— 翻译历史
 * ==========================================================================
 * 分页、搜索、收藏、删除、清空。数据全部来自侧车 SQLite，前端不缓存整表
 * （历史可能有几千条，全量拉进内存既慢又占空间）。
 *
 * 渲染安全：原文与译文都是【不可信数据】（可能来自任意上游模型的输出），
 * 一律用 textContent / createTextNode 渲染，绝不 innerHTML —— 否则
 * 一个返回了 <img onerror=...> 的恶意上游就能在插件沙箱里执行脚本。
 */
(function () {
  "use strict";

  var H = window.History = window.History || {};
  var S = window.TranslationStore;

  var PAGE_SIZE = 30;

  var state = {
    items: [],
    total: 0,
    page: 0,          // 从 0 开始的页号
    q: "",
    starredOnly: false,
    loading: false,
    searchTimer: null,
    onReuse: null     // 「用这条重新翻译」的回调，由 app.js 注入
  };

  /* ============ 安全 DOM 助手（与 engines.js 同规则）============ */
  function $(id) { return document.getElementById(id); }
  function val(id) { var e = $(id); return e ? e.value : ""; }
  function txt(id, v) { var e = $(id); if (e) { e.textContent = (v == null ? "" : String(v)); } }
  function show(id, yes) { var e = $(id); if (e) { e.hidden = !yes; } }
  function click(id, fn) {
    var e = $(id);
    if (!e) { console.warn("[history] 缺少 #" + id); return; }
    e.addEventListener("click", function (ev) {
      try { fn(ev); } catch (err) { reportErr("点击 #" + id, err); }
    });
  }
  function on(id, ev, fn) {
    var e = $(id);
    if (!e) { console.warn("[history] 缺少 #" + id); return; }
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
  function reportErr(where, err) {
    console.error("[history] " + where + ":", err);
    if (window.App && typeof window.App.toast === "function") {
      window.App.toast(t("genericFail", { msg: (err && err.message) || String(err) }), "error");
    }
  }
  function t(key, vars) {
    return (window.TranslationI18N && window.TranslationI18N.t)
      ? window.TranslationI18N.t(key, vars) : key;
  }

  /* ============ 加载 ============ */

  function load(resetPage) {
    if (resetPage) { state.page = 0; }
    if (state.loading) { return Promise.resolve(); }
    state.loading = true;

    var q = {
      limit: PAGE_SIZE,
      offset: state.page * PAGE_SIZE
    };
    if (state.q) { q.q = state.q; }
    if (state.starredOnly) { q.starred = true; }

    return S.listHistory(q).then(function (res) {
      state.loading = false;
      state.items = (res && res.items) || [];
      state.total = (res && res.total) || 0;
      render();
    }).catch(function (err) {
      state.loading = false;
      state.items = [];
      state.total = 0;
      render();
      reportErr("加载历史", err);
    });
  }

  /* ============ 渲染 ============ */

  function render() {
    var box = $("history-list");
    if (!box) { return; }
    box.textContent = "";
    show("history-empty", state.items.length === 0);

    state.items.forEach(function (it) { box.appendChild(renderCard(it)); });

    // 分页信息
    var pages = Math.max(1, Math.ceil(state.total / PAGE_SIZE));
    var cur = state.page + 1;
    txt("history-count", t("pageInfo", { cur: cur, total: pages, n: state.total }));
    show("history-pager", state.total > PAGE_SIZE);
    var prev = $("btn-history-prev"), next = $("btn-history-next");
    if (prev) { prev.disabled = state.page <= 0; }
    if (next) { next.disabled = cur >= pages; }
  }

  function renderCard(it) {
    var head = h("div", { class: "history-head" }, [
      h("span", { class: "history-time", text: formatTime(it.createdAt) }),
      it.engineName ? h("span", { class: "badge", text: it.engineName }) : null,
      it.srcLang ? h("span", { class: "badge muted", text: langName(it.srcLang) + " → " + langName(it.dstLang) }) : null,
      it.chunks > 1 ? h("span", { class: "badge muted", text: it.chunks + " 段" }) : null,
      it.elapsedMs ? h("span", { class: "badge muted", text: it.elapsedMs + " ms" }) : null,
      h("div", { class: "history-actions" }, [
        h("button", {
          class: "icon-btn", type: "button",
          title: it.starred ? t("unstar") : t("star"),
          text: it.starred ? "★" : "☆",
          onclick: function () { actStar(it); }
        }),
        h("button", {
          class: "icon-btn", type: "button", title: t("reuse"), text: "↺",
          onclick: function () { actReuse(it); }
        }),
        h("button", {
          class: "icon-btn", type: "button", title: t("copy"), text: "⧉",
          onclick: function () { actCopy(it); }
        }),
        h("button", {
          class: "icon-btn danger-text", type: "button", title: t("delete"), text: "✕",
          onclick: function () { actDelete(it); }
        })
      ])
    ]);

    // 原文与译文一律 textContent，防 XSS
    var row = h("div", { class: "history-row" }, [
      h("div", { class: "history-src", text: it.source || "" }),
      h("span", { class: "history-arrow", text: "→" }),
      h("div", { class: "history-dst", text: it.target || "" })
    ]);

    return h("div", { class: "history-card", dataset: { id: it.id } }, [head, row]);
  }

  function langName(code) {
    if (!code) { return ""; }
    if (code === "auto") { return t("all") === "全部" ? "自动" : "Auto"; }
    var list = (window.Engines && window.Engines.languages) ? window.Engines.languages() : [];
    for (var i = 0; i < list.length; i++) {
      if (list[i].code === code) {
        return (window.TranslationI18N && window.TranslationI18N.locale() === "en")
          ? list[i].en : list[i].zh;
      }
    }
    return code;
  }

  function formatTime(iso) {
    if (!iso) { return ""; }
    var d = new Date(iso);
    if (isNaN(d.getTime())) { return String(iso).slice(0, 19).replace("T", " "); }
    function pad(n) { return (n < 10 ? "0" : "") + n; }
    var now = new Date();
    var sameDay = d.getFullYear() === now.getFullYear() &&
                  d.getMonth() === now.getMonth() &&
                  d.getDate() === now.getDate();
    var hm = pad(d.getHours()) + ":" + pad(d.getMinutes());
    if (sameDay) { return hm; }
    return (d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + hm;
  }

  /* ============ 操作 ============ */

  function actStar(it) {
    S.starHistory(it.id, !it.starred).then(function () {
      it.starred = !it.starred;
      render();
    }).catch(function (err) { reportErr("收藏", err); });
  }

  function actCopy(it) {
    S.copyText(it.target || "").then(function (ok) {
      if (window.App) { window.App.toast(ok ? t("copied") : t("copyFailed"), ok ? "ok" : "error"); }
    });
  }

  function actReuse(it) {
    if (typeof state.onReuse === "function") {
      try { state.onReuse(it); } catch (e) { reportErr("重用", e); }
    }
  }

  function actDelete(it) {
    S.deleteHistory([it.id]).then(function (res) {
      if (window.App) { window.App.toast(t("historyDeleted", { n: (res && res.deleted) || 1 }), "ok"); }
      // 当前页删空了要回退一页，否则会显示空白页
      if (state.items.length === 1 && state.page > 0) { state.page--; }
      return load();
    }).catch(function (err) { reportErr("删除历史", err); });
  }

  function actClear() {
    if (!window.App || typeof window.App.confirm !== "function") { return; }
    window.App.confirm({
      title: t("clearHistory"),
      lines: [
        { text: t("clearHistoryConfirm") },
        { text: t("clearHistoryNote"), cls: "note" }
      ],
      danger: true
    }).then(function (yes) {
      if (!yes) { return; }
      S.clearHistory(true).then(function (res) {
        if (window.App) { window.App.toast(t("historyCleared", { n: (res && res.deleted) || 0 }), "ok"); }
        state.page = 0;
        return load();
      }).catch(function (err) { reportErr("清空历史", err); });
    });
  }

  /* ============ 绑定 ============ */

  function bind() {
    click("btn-clear-history", actClear);
    click("btn-history-prev", function () {
      if (state.page > 0) { state.page--; load(); }
    });
    click("btn-history-next", function () {
      var pages = Math.ceil(state.total / PAGE_SIZE);
      if (state.page + 1 < pages) { state.page++; load(); }
    });

    // 搜索防抖：每次按键都查库会让 SQLite 忙于无用查询
    on("history-search", "input", function () {
      if (state.searchTimer) { clearTimeout(state.searchTimer); }
      state.searchTimer = setTimeout(function () {
        state.q = val("history-search").trim();
        load(true);
      }, 300);
    });

    // 收藏筛选切换
    var chips = document.querySelectorAll("#history-chips .chip");
    for (var i = 0; i < chips.length; i++) {
      (function (chip) {
        chip.addEventListener("click", function () {
          for (var j = 0; j < chips.length; j++) { chips[j].classList.remove("active"); }
          chip.classList.add("active");
          state.starredOnly = chip.getAttribute("data-starred") === "yes";
          load(true);
        });
      })(chips[i]);
    }
  }

  /* ============ 对外 API ============ */

  H.init = function (onReuse) {
    state.onReuse = onReuse || null;
    bind();
  };
  H.load = load;
  H.refresh = function () { return load(true); };
})();
