/*
 * Translation 即时翻译 —— 引擎管理
 * ==========================================================================
 * 核心设计：表单完全由侧车下发的 schema 驱动，本文件【不硬编码任何引擎的字段】。
 *
 * 这是「新增引擎前端零改动」承诺的落地点：
 *   侧车 engine.Register(kind, factory, schema) 注册新引擎后，
 *   translate/schema 会自动带上它的字段定义，本文件的通用渲染器直接就能画出表单。
 *   如果这里出现 `if (kind === "baidu") { ... }` 这类分支，架构承诺就破产了。
 *
 * 安全约束（对应 AGENTS.md 铁律 9）：
 *   - 密钥字段（field.secret === true）用 password 输入框，且【只在用户主动输入时】
 *     才把值发给侧车；编辑已有引擎时输入框留空表示「保持原密钥不变」。
 *   - 侧车返回的引擎对象只有 hasSecret 与 fingerprint，前端【永远拿不到】明文，
 *     因此也不存在「不小心把密钥写进 DOM 或日志」的可能。
 *
 * DOM 安全（对应 AGENTS.md 铁律 7/8）：
 *   一律用 click()/on()/val()/setVal() 安全绑定，绝不对 $("id") 的结果直接取属性。
 *   动态生成的节点用 h() 构造，译文与用户输入一律走 textContent，绝不 innerHTML
 *   （上游返回的内容是不可信数据，innerHTML 会造成 XSS）。
 */
(function () {
  "use strict";

  var E = window.Engines = window.Engines || {};
  var S = window.TranslationStore;

  /* ============ 状态 ============ */
  var state = {
    schema: null,        // translate/schema 的结果
    kinds: [],           // 引擎类型列表
    kindMap: {},         // kind -> schema
    languages: [],       // 规范语言表
    engines: [],         // 引擎列表
    rev: 0,
    editing: null,       // 正在编辑的引擎（null 表示新建）
    editingKind: "",
    kindLocked: false,     // 编辑已有引擎时类型锁定，不可切换
    kindMenuOpen: false,   // 自定义类型下拉是否展开
     // 弹框里当前选中的 kind
    secretTouched: false,// 用户是否动过密钥输入框
    // nameTouched 记录用户是否【手动改过】引擎名称。
    //
    // 这是「切换类型时自动换默认名」功能的关键：
    //   - 未改过 → 名称仍等于当前类型的默认名，切类型时应跟着换
    //   - 改过   → 用户有自己的命名意图（例如「阿里云-生产」），
    //              切类型时【绝不能】覆盖，否则用户输入凭空消失
    //
    // 判定不用布尔标志单独维护，而是每次比较「当前值是否等于默认名」，
    // 这样用户把名字改回默认值时会自动恢复跟随行为，符合直觉。
    nameTouched: false,
    testing: false,
    onChanged: null      // 引擎变更后的回调（app.js 用来刷新下拉框）
  };

  /* ============ 安全 DOM 助手 ============
   * 这些助手是强制的：元素缺失时只告警不抛错。
   * 直接写 $("id").xxx = ... 的话，一旦 id 拼错就是
   * 「对 null 赋值」→ TypeError → 而它通常在绑定阶段，
   * 会把整个界面连坐搞死（所有按钮失效）。
   */
  function $(id) { return document.getElementById(id); }
  function val(id) { var e = $(id); return e ? e.value : ""; }
  function setVal(id, v) { var e = $(id); if (e) { e.value = (v == null ? "" : v); } }
  function txt(id, v) { var e = $(id); if (e) { e.textContent = (v == null ? "" : String(v)); } }
  function show(id, yes) { var e = $(id); if (e) { e.hidden = !yes; } }
  function click(id, fn) {
    var e = $(id);
    if (!e) { console.warn("[engines] 缺少 #" + id); return; }
    e.addEventListener("click", function (ev) {
      try { fn(ev); } catch (err) { reportErr("点击 #" + id, err); }
    });
  }
  function on(id, ev, fn) {
    var e = $(id);
    if (!e) { console.warn("[engines] 缺少 #" + id); return; }
    e.addEventListener(ev, function (e2) {
      try { fn(e2); } catch (err) { reportErr("事件 #" + id + " " + ev, err); }
    });
  }

  // h 构造 DOM 节点。children 里的字符串一律走 createTextNode，
  // 这是防 XSS 的关键：用户输入与上游返回的内容都不会被当成 HTML 解析。
  function h(tag, props, children) {
    var e = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (k) {
        var v = props[k];
        if (v === null || v === undefined || v === false) { return; }
        if (k === "class") { e.className = v; }
        else if (k === "style") { e.setAttribute("style", v); }
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
    console.error("[engines] " + where + ":", err);
    if (window.App && typeof window.App.toast === "function") {
      window.App.toast(t("genericFail", { msg: (err && err.message) || String(err) }), "error");
    }
  }

  function t(key, vars) {
    return (window.TranslationI18N && window.TranslationI18N.t)
      ? window.TranslationI18N.t(key, vars) : key;
  }
  function isZh() {
    return !(window.TranslationI18N && window.TranslationI18N.locale() === "en");
  }
  // label 按当前语言取中/英文案，缺失时回退到中文。
  function label(obj, zhKey, enKey) {
    if (isZh()) { return obj[zhKey] || obj[enKey] || ""; }
    return obj[enKey] || obj[zhKey] || "";
  }

  /* ============ schema 加载 ============ */

  function loadSchema() {
    return S.getSchema().then(function (res) {
      state.schema = res || {};
      state.kinds = res.kinds || [];
      state.languages = res.languages || [];
      state.kindMap = {};
      state.kinds.forEach(function (k) { state.kindMap[k.kind] = k; });
      S.log("加载引擎 schema", true,
        state.kinds.length + " 种引擎: " + state.kinds.map(function (k) { return k.kind; }).join(","));
      return res;
    }).catch(function (err) {
      S.log("加载引擎 schema", false, (err && err.message) || String(err));
      throw err;
    });
  }

  function loadEngines() {
    return S.listEngines().then(function (res) {
      state.engines = (res && res.engines) || [];
      state.rev = (res && res.rev) || 0;
      renderList();
      notifyChanged();
      return state.engines;
    }).catch(function (err) {
      S.log("加载引擎列表", false, (err && err.message) || String(err));
      state.engines = [];
      renderList();
      throw err;
    });
  }

  function notifyChanged() {
    if (typeof state.onChanged === "function") {
      try { state.onChanged(state.engines); } catch (e) { /* 忽略 */ }
    }
  }

  /* ============ 内置引擎 ============ */

  // isBuiltinRow 判断一条引擎记录是不是内置引擎。
  //
  // 判据来自侧车下发的 builtin 字段（存储层的 builtin 列），
  // 【不是】前端按 kind 名硬编码：那样等于在前端维护第二份「谁是内置」的名单，
  // 以后再加一个内置引擎就得改前端 —— 而「新增引擎前端零改动」是本项目的架构承诺。
  function isBuiltinRow(e) {
    return !!(e && e.builtin);
  }

  // selectableKinds 返回【可手动添加】的引擎类型：内置类型被排除。
  //
  // 内置引擎侧车在安装时就已经播种，再让用户「添加」一个，
  // 只会得到一个配置改不了、又删不掉的重复记录。
  function selectableKinds() {
    return state.kinds.filter(function (k) { return !k.builtin; });
  }

  // displayName 返回引擎行的显示名。
  //
  // 内置引擎必须走 schema 的 label / labelEn，而不是存储记录里的 name：
  // 记录里存的是侧车按中文常量写死的名字，英文界面下显示「免费翻译服务」很奇怪。
  // 普通引擎的名字是用户自己起的，必须照他的输入原样显示。
  function displayName(e) {
    if (!e) { return ""; }
    if (isBuiltinRow(e)) {
      var k = state.kindMap[e.kind] || {};
      return label(k, "label", "labelEn") || e.name || e.kind;
    }
    return e.name || e.kind;
  }

  /* ============ 列表渲染 ============ */

  function renderList() {
    var box = $("engine-list");
    if (!box) { return; }
    box.textContent = "";
    show("engine-empty", state.engines.length === 0);

    state.engines.forEach(function (e, index) {
      box.appendChild(renderCard(e, index));
    });
  }

  function renderCard(e, index) {
    var kindInfo = state.kindMap[e.kind] || {};
    var cfg = e.config || {};
    var builtin = isBuiltinRow(e);

    // 副标题：显示端点或模型，让用户不点开也能分辨同类引擎
    var sub = cfg.endpoint || cfg.model || cfg.cliPath || "";
    if (cfg.endpoint && cfg.model) { sub = cfg.model + " · " + cfg.endpoint; }
    // 内置引擎没有用户可填的端点概念（用户也不该关心域名），副标题改成说清它的性质
    if (builtin) { sub = t("builtinSub"); }

    var badges = [];
    if (e.isDefault) { badges.push(h("span", { class: "badge primary", text: t("defaultBadge") })); }
    if (!e.enabled) { badges.push(h("span", { class: "badge muted", text: t("disabledBadge") })); }
    if (kindInfo.streaming) { badges.push(h("span", { class: "badge ok", text: "流式" })); }
    if (builtin) { badges.push(h("span", { class: "badge", text: t("builtinBadge") })); }

    var nameRow = h("div", { class: "engine-name" }, [
      // 图标放在最前面，读不到时 engineIcon 自己退成首字母色块，宽度不变、行高不抖
      S.engineIcon(kindInfo, displayName(e)),
      h("span", { text: displayName(e) })
    ].concat(builtin ? [] : [
      // 非内置行才显示「类型」徽标：内置行的名称本身就是类型名，
      // 再挂一个同样的文字徽标是重复信息。
      h("span", { class: "badge", text: label(kindInfo, "label", "labelEn") || e.kind })
    ]).concat(badges));

    // 列表行不再展示 🔑+指纹：它只能证明"存了一把密钥"，信息量低且让列表显得乱。
    // 密钥是否已配置，用"编辑弹框里的密码框是否留空"就能判断，不必在列表里显示。
    var subRow = h("div", { class: "engine-sub" }, [
      sub ? h("span", { text: sub }) : null
    ]);

    var actions = h("div", { class: "engine-actions" }, [
      h("button", {
        class: "ghost sm", type: "button", title: t("setDefault"),
        onclick: function () { actSetDefault(e); },
        // 已是默认时禁用，避免无意义的写操作
        disabled: e.isDefault === true
      }, t("setDefault")),
      h("button", {
        class: "ghost sm", type: "button",
        onclick: function () { actToggle(e); }
      }, e.enabled ? t("disable") : t("enable")),
      h("button", {
        class: "ghost sm", type: "button",
        onclick: function () { openEditor(e); }
        // 内置引擎弹框是只读说明，按钮文案随之改成「说明」
      }, builtin ? t("builtinInfoBtn") : t("edit"))
    ].concat(builtin ? [] : [
      // 【内置引擎没有删除入口】：它是插件能力的一部分，不是用户数据。
      // 前端不给按钮只是体验，真正的防线在侧车（engines/delete 会拒绝），
      // 因为手工构造的 RPC 与 data/import 都不经过这里。
      h("button", {
        class: "ghost sm danger-text", type: "button",
        onclick: function () { actDelete(e); }
      }, t("delete"))
    ]));

    var card = h("div", {
      class: "engine-card" + (e.isDefault ? " is-default" : "") + (e.enabled ? "" : " is-disabled"),
      dataset: { id: e.id, index: String(index) }
    }, [
      // 【所有】引擎的把手都可拖，包括默认引擎：
      // 顺序只能由用户自己拖出来，「设为默认」不是重新排版的指令。
      h("span", {
        class: "drag-handle",
        title: isZh() ? "按住拖动排序" : "Press and drag to reorder"
      }, "⠿"),
      h("div", { class: "engine-main" }, [nameRow, subRow]),
      actions
    ]);

    bindGrip(card, e);
    return card;
  }

  /* ============ 拖拽排序（指针事件版） ============
   * 沙箱 iframe 里原生 HTML5 drag & drop 经常不触发（连 dragstart 都不发），
   * 所以改用 pointerdown / pointermove / pointerup 手写拖拽 —— 不依赖 dataTransfer，
   * 在任何宿主里都能工作。排序结果经 engines/reorder 持久化。
   */
  // pdrag 记录一次拖动的状态。moved 用来区分「只是点了一下把手」与「真的拖了」：
  // 前者不该发出任何写请求（白白 bumpRev，还会让别的窗口遇到一次乐观锁冲突）。
  var pdrag = { id: "", el: null, moved: false };

  function boxCards() {
    var box = $("engine-list");
    if (!box) { return []; }
    return Array.prototype.slice.call(box.querySelectorAll(".engine-card"));
  }

  // bindGrip 给所有引擎（含默认引擎）的 grip 挂拖拽。
  //
  // 这里曾经有一行 `if (e.isDefault) { return; }`（默认引擎固定首位所以不许拖），
  // 它与「设默认即插队」是同一个被推翻的设计的两半：只删插队不删这里，
  // 默认引擎就会变成一个「既不能插队、又不能被挪动」的死位置。
  function bindGrip(card, e) {
    var grip = card.querySelector(".drag-handle");
    if (!grip) { return; }

    grip.addEventListener("pointerdown", function (ev) {
      // 只认主键：右键/中键按下不该进入拖拽态
      if (ev.button !== undefined && ev.button !== 0) { return; }
      ev.preventDefault();
      pdrag.id = e.id;
      pdrag.el = card;
      pdrag.moved = false;
      card.classList.add("dragging");
      // 被拖的这一行对命中测试透明：否则它跟着光标走，命中判定永远命中它自己，
      // 看起来就是「拖不动」（cardAt 里也再显式跳过一次，双保险）。
      card.style.pointerEvents = "none";
      // 拖动期间全局禁选：沙箱 iframe 里一旦起动原生文字选区，指针事件会被选区流程吃掉。
      document.body.classList.add("is-reordering");

      // 关键：move / up 一律挂在 window 上，【不依赖 setPointerCapture】。
      // 旧实现把 move 挂在把手自身、指望 capture 把后续事件重定向过来，
      // 而那句 setPointerCapture 被包在 try/catch 里静默失败过 ——
      // 光标一离开这 15px 宽的把手，pointermove 就再也不到达，
      // 症状正是用户报的「行变灰了但拖不动」。挂 window 才与捕获成败彻底解耦。
      window.addEventListener("pointermove", onDragMove);
      window.addEventListener("pointerup", onDragEnd);
      window.addEventListener("pointercancel", onDragEnd);
    });
  }

  function onDragMove(ev) {
    if (!pdrag.el) { return; }
    ev.preventDefault();
    var box = $("engine-list");
    if (!box) { return; }
    var target = cardAt(ev.clientY);
    if (!target || target === pdrag.el) { return; }
    var r = target.getBoundingClientRect();
    if (ev.clientY < r.top + r.height / 2) { box.insertBefore(pdrag.el, target); }
    else { box.insertBefore(pdrag.el, target.nextSibling); }
    pdrag.moved = true;
  }

  function onDragEnd() {
    window.removeEventListener("pointermove", onDragMove);
    window.removeEventListener("pointerup", onDragEnd);
    window.removeEventListener("pointercancel", onDragEnd);
    document.body.classList.remove("is-reordering");
    var card = pdrag.el;
    var moved = pdrag.moved;
    pdrag.el = null;
    pdrag.id = "";
    pdrag.moved = false;
    if (!card) { return; }
    card.classList.remove("dragging");
    card.style.pointerEvents = "";
    if (!moved) { return; }   // 只点了一下把手：不发多余的写请求
    commitOrder();
  }

  function cardAt(clientY) {
    var cards = boxCards();
    for (var i = 0; i < cards.length; i++) {
      if (cards[i] === pdrag.el) { continue; }   // 跳过被拖的那一行本身
      var r = cards[i].getBoundingClientRect();
      if (clientY >= r.top && clientY <= r.bottom) { return cards[i]; }
    }
    return null;
  }

  // commitOrder 读取 DOM 的实际顺序并原样持久化 —— 用户拖成什么样就是什么样。
  //
  // 这里原本有一段「把默认引擎 id 提到最前」的修正，是「默认置顶」契约的第三处
  // 实现（前两处是 ListEngines 的 ORDER BY is_default DESC 与前端 orderedEngineList）：
  // 表现就是无论把默认引擎拖到哪里，松手后它都会自己弹回第一位 ——
  // 用户以为「拖动没生效」，实际上是代码在违背他的操作。
  // 契约变更后（顺序只由拖动决定、设为默认不插队），这段修正必须删掉；
  // 三处只要漏一处，就会出现「列表按拖的顺序、下拉却按默认插队」这种两处不一致。
  function commitOrder() {
    var cards = boxCards();
    var ids = [];
    for (var i = 0; i < cards.length; i++) {
      var id = cards[i].getAttribute("data-id");
      if (id) { ids.push(id); }
    }
    applyReorderIds(ids);
  }

  function applyReorderIds(ids) {
    var snapshot = state.engines.slice();
    state.engines = ids.map(function (id) {
      for (var i = 0; i < snapshot.length; i++) { if (snapshot[i].id === id) { return snapshot[i]; } }
      return null;
    }).filter(Boolean);
    // 必须同时把 ord 写回每条记录：数组顺序换了但【ord 字段还是旧值】的话，
    // 任何按 ord 排序的消费方（翻译页的引擎下拉 orderedEngineList）
    // 都会拿到拖之前的顺序 —— 表现为「引擎页已经拖好了，下拉一点没变」。
    // 这里按最终 DOM 顺序重编号，与 engines/reorder 落到侧车的 ord 保持一致。
    for (var i = 0; i < state.engines.length; i++) {
      state.engines[i].ord = i;
    }
    renderList();
    // 必须通知翻译页：loadEngines() 会 notifyChanged()，而这条本地路径以前漏了，
    // 于是下拉要等到下次重新加载引擎列表才可能刷新（用户看不到任何报错）。
    notifyChanged();

    S.reorderEngines(ids).then(function (res) {
      state.rev = (res && res.rev) || state.rev;
    }).catch(function (err) {
      // 失败要整体回滚：数组顺序、ord、以及两个消费方的视图都得回到拖之前，
      // 否则前端显示的顺序与库里的 ord 从此不一致，重启插件后顺序会「莫名跳回去」。
      reportErr("排序", err);
      state.engines = snapshot;
      renderList();
      notifyChanged();
    });
  }

  /* ============ 列表操作 ============ */

  function actSetDefault(e) {
    S.setDefaultEngine(e.id).then(function (res) {
      state.rev = (res && res.rev) || state.rev;
      return loadEngines();
    }).catch(function (err) { reportErr("设为默认", err); });
  }

  function actToggle(e) {
    S.toggleEngine(e.id, !e.enabled).then(function (res) {
      state.rev = (res && res.rev) || state.rev;
      return loadEngines();
    }).catch(function (err) { reportErr("切换启用状态", err); });
  }

  function actDelete(e) {
    // 沙箱里没有原生 confirm，必须用自己的弹框
    if (!window.App || typeof window.App.confirm !== "function") {
      return;
    }
    window.App.confirm({
      title: t("delete"),
      lines: [
        { text: t("deleteEngineConfirm", { name: e.name || e.kind }) },
        { text: t("deleteEngineNote"), cls: "note" }
      ],
      danger: true
    }).then(function (yes) {
      if (!yes) { return; }
      S.deleteEngine(e.id).then(function (res) {
        state.rev = (res && res.rev) || state.rev;
        if (window.App) { window.App.toast(t("engineDeleted"), "ok"); }
        return loadEngines();
      }).catch(function (err) { reportErr("删除引擎", err); });
    });
  }

  /* ============ 编辑弹框 ============ */

  // openEditor 打开引擎编辑弹框。
  //
  // e 为 null 表示新建；否则为编辑已有引擎。
  // 编辑时【不回填密钥】—— 前端根本拿不到明文，输入框留空表示「保持不变」。
  function openEditor(e) {
    state.editing = e || null;
    state.secretTouched = false;
    state.nameTouched = false;

    // 内置引擎走【只读说明】形态：没有可改字段、没有删除、保存按钮变成「知道了」。
    // 之所以复用同一个弹框而不是再写一个：它的信息本来就只有 schema 的 description，
    // 再做一个弹框就是两处渲染同一份文案，迟早漂移。
    state.editingBuiltin = !!(e && e.builtin);
    var isB = state.editingBuiltin;

    var titleEl = $("engine-modal-title");
    if (titleEl) { titleEl.textContent = isB ? t("builtinEngineTitle") : (e ? t("editEngine") : t("addEngine")); }

    show("btn-engine-delete", !!e && !isB);
    var saveBtn = $("btn-engine-save");
    if (saveBtn) { saveBtn.textContent = isB ? t("builtinDone") : t("save"); }
    var cancelBtn = $("btn-engine-cancel");
    if (cancelBtn) { cancelBtn.hidden = isB; }
    var nameInput = $("engine-name");
    if (nameInput) { nameInput.disabled = isB; }
    var nameHint = $("engine-name-hint");
    if (nameHint) { nameHint.textContent = isB ? t("builtinLocked") : t("engineNameHint"); }
    clearFormError();

    // 新建时默认选第一个引擎类型（openai 覆盖面最广）
    // 新建时默认选第一个【可添加】类型（openai 覆盖面最广）；
    // 必须跳过内置类型，否则新建出来的第一条就是那个改不了配置的重复记录。
    var selectable = selectableKinds();
    state.editingKind = e ? e.kind : (selectable[0] ? selectable[0].kind : "");
    // 编辑时锁定类型：换类型等于换一套完全不同的配置，应该删掉重建
    state.kindLocked = !!e;
    closeKindMenu();
    renderKindSelect();

    // 名称框：编辑时回填已有名称并标记为「用户已定名」（不再自动跟随类型），
    // 新建时填入所选类型的默认名。
    if (e) {
      // 内置引擎不可改名，名称框里放的是 displayName()（随界面语言）而非存储记录里的
      // 中文名 —— 英文界面下弹框里出现「免费翻译服务」会让人以为是没翻译的漏网文案。
      setVal("engine-name", isB ? displayName(e) : (e.name || ""));
      // 编辑已有引擎时，名称是用户之前定的，切类型也不该动它
      // （何况编辑时类型选择框已被禁用，本来也切不了）
      state.nameTouched = true;
    } else {
      setVal("engine-name", defaultNameFor(state.editingKind));
    }

    renderKindDesc();
    renderFields();
    show("engine-modal", true);
    focusFirstField();
  }

  function closeEditor() {
    show("engine-modal", false);
    state.editing = null;
    // 复位只读态：弹框是复用的，上一次是内置引擎留下的 disabled/hidden
    // 如果不清掉，下次打开普通引擎就会看到一个禁用的名称框和一个消失的测试按钮。
    state.editingBuiltin = false;
    var nameInput = $("engine-name");
    if (nameInput) { nameInput.disabled = false; }
    var cancelBtn = $("btn-engine-cancel");
    if (cancelBtn) { cancelBtn.hidden = false; }
    var saveBtn = $("btn-engine-save");
    if (saveBtn) { saveBtn.textContent = t("save"); }
    var nameHint = $("engine-name-hint");
    if (nameHint) { nameHint.textContent = t("engineNameHint"); }
    state.editingKind = "";
    state.secretTouched = false;
    state.nameTouched = false;
    var box = $("engine-fields");
    if (box) { box.textContent = ""; }
    clearFormError();
  }

  // defaultNameFor 返回某引擎类型的默认名称（按当前界面语言）。
  //
  // 来源是侧车下发的 schema.defaultName / defaultNameEn —— 默认名属于
  // 引擎自身的知识（「阿里云翻译」而不是「aliyun」），放在 schema 里
  // 才能保证「新增引擎前端零改动」。
  //
  // 兜底顺序：schema.defaultName → schema.label → kind 本身。
  function defaultNameFor(kind) {
    var k = state.kindMap[kind];
    if (!k) { return kind || ""; }
    var d = isZh() ? (k.defaultName || k.label) : (k.defaultNameEn || k.labelEn || k.defaultName || k.label);
    return d || kind || "";
  }

  // nameIsDefault 判断名称框当前内容是否「仍等于某类型的默认名」。
  //
  // 空值也算默认（用户没填，留空时后端会用 defaultEngineName 兜底）。
  function nameIsDefault(kind) {
    var cur = val("engine-name").trim();
    if (cur === "") { return true; }
    if (cur === defaultNameFor(kind)) { return true; }
    // 也认另一种语言的默认名：用户可能在中文界面下填了英文名，
    // 之后切换界面语言，此时不应把它当成「用户自定义名」而拒绝跟随。
    var k = state.kindMap[kind];
    if (k && (cur === k.defaultName || cur === k.defaultNameEn)) { return true; }
    return false;
  }

  // syncNameToKind 在切换引擎类型时同步名称。
  //
  // 只在「名称仍等于旧类型默认名」时才替换 —— 这是脏值跟踪，
  // 保证用户手动起的名字（例如「阿里云-生产环境」）在切换类型后不被冲掉。
  function syncNameToKind(newKind) {
    if (state.nameTouched) { return; }
    setVal("engine-name", defaultNameFor(newKind));
  }

  function kindLabel(k) { return label(k, "label", "labelEn") || k.kind; }

  // renderKindSelect 重建自定义下拉的选项列表，并同步按钮文案/图标。
  // 原生 <select> 的 <option> 无法渲染图片（浏览器硬限制），
  // 所以这里用 button + ul 自绘：每个选项 = 图标 + 文案，点击即选中。
  function renderKindSelect() {
    var menu = $("engine-kind-menu");
    if (!menu) { return; }
    menu.textContent = "";
    selectableKinds().forEach(function (k) {
      var item = h("li", {
        class: "kind-item" + (k.kind === state.editingKind ? " is-active" : ""),
        role: "option",
        "aria-selected": k.kind === state.editingKind ? "true" : "false"
      }, [
        S.engineIcon(k, kindLabel(k)),
        h("span", { class: "kind-item-label", text: kindLabel(k) })
      ]);
      item.addEventListener("click", function () { selectKind(k.kind); });
      menu.appendChild(item);
    });
    updateKindButton();
    updateKindIcon();
  }

  function renderKindDesc() {
    var k = state.kindMap[state.editingKind];
    txt("engine-kind-desc", k ? label(k, "description", "descriptionEn") : "");
  }

  // selectKind 是「选类型」的唯一入口：点下拉选项走这里。
  // 编辑已有引擎时 kindLocked=true，任何来源的切换都被挡在门外。
  function selectKind(kind) {
    if (state.kindLocked) { closeKindMenu(); return; }
    if (kind === state.editingKind) { closeKindMenu(); return; }
    state.editingKind = kind;
    state.secretTouched = false;
    syncNameToKind(kind);
    renderKindDesc();
    renderFields();
    clearFormError();
    updateKindButton();
    updateKindIcon();
    closeKindMenu();
  }

  function updateKindButton() {
    var lbl = $("engine-kind-btn-label");
    if (lbl) { lbl.textContent = kindLabel(state.kindMap[state.editingKind] || {}) || "—"; }
    var btn = $("engine-kind-btn");
    if (btn) { btn.disabled = state.kindLocked; }
  }

  function updateKindIcon() {
    var box = $("engine-kind-btn-icon");
    if (!box) { return; }
    box.textContent = "";
    var k = state.kindMap[state.editingKind];
    if (k) { box.appendChild(S.engineIcon(k, kindLabel(k))); }
  }

  function openKindMenu() {
    if (state.kindLocked) { return; }
    var menu = $("engine-kind-menu");
    var anchor = $("engine-kind");
    if (menu) {
      menu.hidden = false;
      // 与翻译页共用同一个自适应实现，避免两处行为漂移
      if (S && S.fitDropdownMenu) { S.fitDropdownMenu(menu, anchor); }
    }
    state.kindMenuOpen = true;
    var btn = $("engine-kind-btn");
    if (btn) { btn.setAttribute("aria-expanded", "true"); }
  }

  function closeKindMenu() {
    var menu = $("engine-kind-menu");
    if (menu) { menu.hidden = true; }
    state.kindMenuOpen = false;
    var btn = $("engine-kind-btn");
    if (btn) { btn.setAttribute("aria-expanded", "false"); }
  }

  // renderFields 按 schema 动态渲染表单字段。
  //
  // 这是「新增引擎前端零改动」的核心：字段类型、必填、默认值、选项、
  // 条件显隐全部来自侧车下发的 schema，本函数只做通用渲染。
  function renderFields() {
    var box = $("engine-fields");
    if (!box) { return; }
    box.textContent = "";

    var k = state.kindMap[state.editingKind];
    if (!k || !k.fields) {
      box.appendChild(h("p", { class: "field-desc", text: "该引擎类型没有可配置字段。" }));
      return;
    }

    var existing = state.editing ? (state.editing.config || {}) : {};

    k.fields.forEach(function (f) {
      var node = renderField(f, existing);
      if (node) { box.appendChild(node); }
    });

    // 密钥提示：编辑已有引擎时告知「留空表示不变」
    if (state.editing && state.editing.hasSecret) {
      box.appendChild(h("p", {
        class: "secret-hint",
        text: "🔑 已保存 API Key（" + (state.editing.fingerprint || "****") + "）。" + t("secretKept")
      }));
    }

    applyVisibility();
  }

  // renderField 渲染单个字段，返回 DOM 节点。
  function renderField(f, existing) {
    var id = "ef-" + f.key;
    var labelText = label(f, "label", "labelEn") || f.key;
    var descText = label(f, "description", "descriptionEn");

    var labelEl = h("span", { class: "field-label" }, [
      labelText,
      f.required ? h("em", { text: " *" }) : null
    ]);

    var input;
    var current = existing[f.key];

    switch (f.type) {
      case "select":
        input = h("select", { id: id, class: "select", dataset: { key: f.key } });
        (f.options || []).forEach(function (o) {
          input.appendChild(h("option", { value: o.value, text: o.label }));
        });
        break;

      case "radio":
        // 单选组用容器包裹，不用 select
        input = h("div", { class: "field-inline", id: id, dataset: { key: f.key, type: "radio" } });
        (f.options || []).forEach(function (o, i) {
          var rid = id + "-" + i;
          input.appendChild(h("label", { class: "field-check" }, [
            h("input", {
              type: "radio", name: id, id: rid, value: o.value,
              checked: (current === o.value) || (current === undefined && i === 0)
            }),
            h("span", { text: o.label })
          ]));
        });
        break;

      case "boolean":
        input = h("label", { class: "field-check", id: id, dataset: { key: f.key, type: "boolean" } }, [
          h("input", { type: "checkbox", checked: current === true || (current === undefined && f.default === true) }),
          h("span", { text: labelText })
        ]);
        // boolean 字段的标签已经在 checkbox 旁边，不需要上面的 labelEl
        labelEl = null;
        break;

      case "number":
        input = h("input", {
          id: id, class: "input", type: "number", dataset: { key: f.key },
          value: (current !== undefined && current !== null) ? current : (f.default !== undefined ? f.default : ""),
          placeholder: f.placeholder || ""
        });
        if (f.min !== undefined && f.min !== null) { input.setAttribute("min", String(f.min)); }
        if (f.max !== undefined && f.max !== null) { input.setAttribute("max", String(f.max)); }
        if (f.placeholder) { input.setAttribute("placeholder", f.placeholder); }
        break;

      case "textarea":
        input = h("textarea", {
          id: id, class: "input", rows: String(f.multiline || 4),
          dataset: { key: f.key },
          placeholder: f.placeholder || ""
        });
        input.value = (current !== undefined && current !== null) ? String(current)
          : (f.default !== undefined ? String(f.default) : "");
        break;

      case "password":
        // 密钥字段：编辑时【留空】，绝不回填（前端也拿不到明文）
        input = h("input", {
          id: id, class: "input", type: "password", dataset: { key: f.key, secret: "1" },
          placeholder: f.placeholder || "",
          autocomplete: "new-password"   // 阻止浏览器自动填充旧密码
        });
        // 记录「用户动过密钥框」：只有动过才把值发给侧车，
        // 否则发空串会被侧车理解为「清除密钥」。
        input.addEventListener("input", function () { state.secretTouched = true; });
        break;

      default: // text
        input = h("input", {
          id: id, class: "input", type: "text", dataset: { key: f.key },
          placeholder: f.placeholder || "",
          autocomplete: "off"
        });
        input.value = (current !== undefined && current !== null) ? String(current)
          : (f.default !== undefined ? String(f.default) : "");
        break;
    }

    var wrap = h("div", { class: "field", dataset: { fieldKey: f.key } }, [
      labelEl,
      input,
      descText ? h("p", { class: "field-desc", text: descText }) : null
    ]);

    // 条件显隐：字段值变化时重新求值
    input.addEventListener && input.addEventListener("change", applyVisibility);
    input.addEventListener && input.addEventListener("input", applyVisibility);

    return wrap;
  }

  // applyVisibility 求值所有字段的 visible_when，隐藏不满足条件的字段。
  //
  // 语义对齐 DBX 官方表单：
  //   - 子句 {field, one_of}：引用字段的值在 one_of 里则命中（按字符串比较）
  //   - all_of / any_of / not 组合
  //   - 【级联】：子句引用的字段自身处于隐藏状态时，该子句不参与判定
  //   - 未填写或空值永不命中
  function applyVisibility() {
    var k = state.kindMap[state.editingKind];
    if (!k || !k.fields) { return; }
    var values = collectValues();

    // 先算出每个字段的可见性（不考虑级联），再迭代收敛处理级联
    var visible = {};
    k.fields.forEach(function (f) { visible[f.key] = true; });

    // 最多迭代 8 层（对齐 DBX 的组合树深度上限），防止循环依赖导致死循环
    for (var pass = 0; pass < 8; pass++) {
      var changed = false;
      k.fields.forEach(function (f) {
        if (!f.visible_when) { return; }
        var v = evalWhen(f.visible_when, values, visible);
        if (v !== visible[f.key]) { visible[f.key] = v; changed = true; }
      });
      if (!changed) { break; }
    }

    k.fields.forEach(function (f) {
      var wrap = document.querySelector('[data-field-key="' + cssEscape(f.key) + '"]');
      if (wrap) { wrap.hidden = !visible[f.key]; }
    });
  }

  // cssEscape 转义属性选择器里的特殊字符。
  // 字段 key 理论上只含字母数字下划线，但转义一层更稳妥。
  function cssEscape(s) {
    return String(s).replace(/["\\]/g, "\\$&");
  }

  // evalWhen 求值一条 visible_when 规则。
  function evalWhen(rule, values, visible) {
    if (!rule) { return true; }

    if (rule.all_of && rule.all_of.length) {
      for (var i = 0; i < rule.all_of.length; i++) {
        if (!evalWhen(rule.all_of[i], values, visible)) { return false; }
      }
      return true;
    }
    if (rule.any_of && rule.any_of.length) {
      var any = false;
      for (var j = 0; j < rule.any_of.length; j++) {
        if (evalWhen(rule.any_of[j], values, visible)) { any = true; break; }
      }
      return any;
    }
    if (rule.not) { return !evalWhen(rule.not, values, visible); }

    if (rule.field) {
      // 级联：引用字段自身隐藏时，该子句不参与判定（视为不命中）
      if (visible && visible[rule.field] === false) { return false; }
      var cur = values[rule.field];
      // 未填写或空值永不命中
      if (cur === undefined || cur === null || cur === "") { return false; }
      var list = rule.one_of || [];
      for (var m = 0; m < list.length; m++) {
        // 按字符串比较：这样 [false] 与 ["false"] 都能匹配布尔 false
        if (String(cur) === String(list[m])) { return true; }
      }
      return false;
    }
    return true;
  }

  // collectValues 收集当前表单的所有字段值（用于条件显隐求值）。
  function collectValues() {
    var out = {};
    var k = state.kindMap[state.editingKind];
    if (!k || !k.fields) { return out; }
    k.fields.forEach(function (f) {
      out[f.key] = readFieldValue(f);
    });
    return out;
  }

  // readFieldValue 按字段类型读取值，并做类型转换。
  function readFieldValue(f) {
    var id = "ef-" + f.key;
    var el = $(id);
    if (!el) { return undefined; }

    if (f.type === "boolean") {
      var cb = el.querySelector("input[type=checkbox]");
      return cb ? cb.checked : false;
    }
    if (f.type === "radio") {
      var checked = el.querySelector("input[type=radio]:checked");
      return checked ? checked.value : "";
    }
    if (f.type === "number") {
      var raw = el.value;
      if (raw === "" || raw === null) { return undefined; }
      var n = Number(raw);
      return isNaN(n) ? undefined : n;
    }
    return el.value;
  }

  function focusFirstField() {
    var box = $("engine-fields");
    if (!box) { return; }
    var first = box.querySelector("input:not([type=hidden]), select, textarea");
    if (first) { try { first.focus(); } catch (e) { /* 忽略 */ } }
  }

  /* ============ 表单校验与提交 ============ */

  function clearFormError() {
    var el = $("engine-form-error");
    if (el) { el.textContent = ""; el.hidden = true; el.className = "form-error"; }
  }

  function showFormError(msg, isWarn) {
    var el = $("engine-form-error");
    if (!el) { return; }
    el.textContent = msg;
    el.className = isWarn ? "form-warn" : "form-error";
    el.hidden = false;
  }

  // validateForm 做前端侧的必填校验。
  //
  // 侧车也会校验，但前端先拦一道能给出更精准的提示
  // （能指出是哪个字段的 label，而不只是字段 key）。
  function validateForm() {
    var k = state.kindMap[state.editingKind];
    if (!k) { return t("unknownError"); }

    // 名称校验先于字段校验：名称是用户区分同类多实例的唯一手段，
    // 缺了它列表里会出现两行一模一样的「阿里云翻译」，无法分辨。
    // 但允许留空 —— 留空时用默认名兜底（见 buildPayload），不算错误。
    var nameVal = val("engine-name").trim();
    if (nameVal === "" && defaultNameFor(state.editingKind) === "") {
      return t("engineNameRequired");
    }

    var values = collectValues();

    for (var i = 0; i < k.fields.length; i++) {
      var f = k.fields[i];
      // 隐藏的字段不参与校验（对齐 DBX：条件会级联，隐藏字段的默认值不该点亮孙级）
      var wrap = document.querySelector('[data-field-key="' + cssEscape(f.key) + '"]');
      if (wrap && wrap.hidden) { continue; }

      if (!f.required) { continue; }
      var v = values[f.key];
      var empty = (v === undefined || v === null || v === "" ||
                   (typeof v === "string" && v.trim() === ""));

      // 密钥字段的特殊规则：编辑已有引擎且已存密钥时，留空表示「保持不变」，不算缺失
      if (empty && f.secret && state.editing && state.editing.hasSecret && !state.secretTouched) {
        continue;
      }
      if (empty) {
        return t("fieldRequired", { label: label(f, "label", "labelEn") || f.key });
      }
    }
    return "";
  }

  // buildPayload 组装 engines/save 的请求体。
  function buildPayload() {
    var k = state.kindMap[state.editingKind];
    var values = collectValues();
    var config = {};
    var secrets = null;

    (k.fields || []).forEach(function (f) {
      var v = values[f.key];
      if (f.secret) {
        // 密钥单独走 secrets 通道，绝不混进 config
        // （config 是明文存储的 external_config）
        if (v !== undefined && v !== null && String(v).trim() !== "") {
          secrets = secrets || {};
          secrets[f.key] = String(v).trim();
        }
        return;
      }
      if (v !== undefined) { config[f.key] = v; }
    });

    // 名称来自独立的名称输入框（不再是 schema 里的 displayName 字段）。
    // 留空时交给侧车用 defaultEngineName(kind) 兜底，保证列表里不会出现空白行。
    var nameVal = val("engine-name").trim();
    // 【不传 rev】：rev 由 storage.js 的 withRevRetry 统一填充。
    //
    // 之前这里传了 state.rev（engines.js 自己缓存的值），而 withRevRetry
    // 只在 `body.rev === undefined` 时才覆盖，导致冲突重试时仍用陈旧 rev，
    // 无限复现同一个冲突。现在 storage.js 无条件以最新 rev 为准，
    // 上层模块不再自己缓存 rev —— 单一权威来源，不会出现两份 rev 打架。
    var payload = {
      kind: state.editingKind,
      name: nameVal || defaultNameFor(state.editingKind),
      config: config
    };
    if (state.editing) { payload.id = state.editing.id; }
    // 只有用户动过密钥框才传 secrets：
    //   不传   → 侧车保持原密钥不变
    //   传空串 → 侧车清除密钥
    if (secrets) { payload.secrets = secrets; }
    else if (state.secretTouched) { payload.secrets = {}; }  // 用户清空了输入框 → 清除密钥

    return payload;
  }

  function actSave() {
    // 内置引擎的「知道了」就是关闭。不发 engines/save：
    // 后端虽有兜底（会忽略改写），但白写一次会 bumpRev，
    // 另一个 DBX 窗口手里的 rev 凭空过期，用户会遇到一次莫名的冲突重试。
    if (state.editingBuiltin) { closeEditor(); return; }
    clearFormError();
    var verr = validateForm();
    if (verr) { showFormError(verr); return; }

    var payload = buildPayload();
    var btn = $("btn-engine-save");
    if (btn) { btn.disabled = true; }

    S.saveEngine(payload).then(function (res) {
      if (btn) { btn.disabled = false; }
      if (res && res.conflict) {
        showFormError(t("conflictRetry"));
        return;
      }
      state.rev = (res && res.rev) || state.rev;
      if (res && res.warning) {
        // 非致命警告（例如用了非加密 HTTP、开启了跳过证书校验）
        if (window.App) { window.App.toast(res.warning, "warn", 6000); }
      }
      if (window.App) { window.App.toast(t("engineSaved"), "ok"); }
      closeEditor();
      return loadEngines();
    }).catch(function (err) {
      if (btn) { btn.disabled = false; }
      showFormError((err && err.message) || String(err));
    });
  }

  // actTest 测试连通性。
  //
  // 支持「保存前测试」：把当前表单里的配置（含未保存的密钥）直接发给侧车试连，
  // 这样用户不必先保存一个可能连不上的引擎。
  function actTest() {
    clearFormError();
    if (state.testing) { return; }

    var payload;
    if (state.editing && !state.secretTouched) {
      // 编辑已有引擎且没改密钥 → 用 id 测试（侧车自己解密）
      payload = { id: state.editing.id };
    } else {
      // 新建，或改了密钥 → 把表单当前值发过去
      var k = state.kindMap[state.editingKind];
      var values = collectValues();
      var config = {}, secrets = {};
      (k.fields || []).forEach(function (f) {
        var v = values[f.key];
        if (v === undefined) { return; }
        if (f.secret) { secrets[f.key] = String(v).trim(); }
        else { config[f.key] = v; }
      });
      payload = { kind: state.editingKind, config: config };
      if (Object.keys(secrets).length) { payload.secrets = secrets; }
    }

    state.testing = true;
    var btn = $("btn-engine-test");
    if (btn) { btn.disabled = true; btn.textContent = t("testing"); }

    S.testEngine(payload).then(function (res) {
      state.testing = false;
      if (btn) { btn.disabled = false; btn.textContent = t("testConnection"); }
      if (!res) { showFormError(t("unknownError")); return; }
      if (res.ok) {
        var extra = [];
        if (res.streaming) { extra.push("支持流式"); }
        if (res.maxChars) { extra.push("单次上限 " + res.maxChars + " 字符"); }
        showFormError(t("testOk", { ms: res.elapsedMs }) +
          (extra.length ? "（" + extra.join("，") + "）" : ""), true);
      } else {
        var msg = res.message || t("unknownError");
        if (res.detail) { msg += "\n" + res.detail; }
        showFormError(msg);
      }
    }).catch(function (err) {
      state.testing = false;
      if (btn) { btn.disabled = false; btn.textContent = t("testConnection"); }
      showFormError((err && err.message) || String(err));
    });
  }

  /* ============ 事件绑定 ============ */

  function bind() {
    click("btn-add-engine", function () { openEditor(null); });
    click("btn-add-first-engine", function () {
      switchView("engines");
      openEditor(null);
    });
    click("btn-manage-engines", function () { switchView("engines"); });
    click("btn-close-engine-modal", closeEditor);
    click("btn-engine-cancel", closeEditor);
    click("btn-engine-save", actSave);
    click("btn-engine-test", actTest);
    click("btn-engine-delete", function () {
      if (state.editing) { actDelete(state.editing); closeEditor(); }
    });

    // 类型选择已从原生 <select> 换成自定义下拉：
    // 点击按钮开合菜单，点击选项走 selectKind()（逻辑在渲染区定义）。
    click("engine-kind-btn", function () {
      if (state.kindMenuOpen) { closeKindMenu(); } else { openKindMenu(); }
    });
    // 点击下拉以外的区域自动收起
    document.addEventListener("click", function (ev) {
      var sel = $("engine-kind");
      if (sel && !sel.contains(ev.target)) { closeKindMenu(); }
    });
    // 按 Esc 收起
    document.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape") { closeKindMenu(); }
    });

    // 名称框：用户一旦手动输入就标记为「已定名」，之后切类型不再覆盖。
    //
    // 用 input 事件而非 change：change 只在失焦时触发，用户改完名字
    // 直接点「保存」的话 change 可能还没触发，nameTouched 仍是 false。
    // 更稳的判定是「当前值是否还等于默认名」—— 用户把名字改回默认值时
    // 会自动恢复跟随行为，这比单纯的布尔标志更符合直觉。
    on("engine-name", "input", function () {
      // 输入即限长：超过单位上限就当场裁掉（含粘贴 —— paste 后也会触发 input）。
      // 为什么在前端就裁：后端也裁（engines/save 校验），但那种情况下用户
      // 会看到「保存失败：名称过长」这种马后炮；当场限制才是他预期的行为。
      var el = $("engine-name");
      if (el && S && S.truncateToUnits) {
        var cut = S.truncateToUnits(el.value, S.maxNameUnits || 32);
        if (cut !== el.value) { el.value = cut; }
      }
      state.nameTouched = !nameIsDefault(state.editingKind);
    });

    // 点背景关闭弹框
    var modal = $("engine-modal");
    if (modal) {
      modal.addEventListener("click", function (ev) {
        if (ev.target && ev.target.getAttribute && ev.target.getAttribute("data-close") === "1") {
          closeEditor();
        }
      });
    }

    // Esc 关闭弹框
    document.addEventListener("keydown", function (ev) {
      if (ev.key !== "Escape") { return; }
      var m = $("engine-modal");
      if (m && !m.hidden) { closeEditor(); }
    });
  }

  // switchView 由 app.js 提供；这里做容错，避免加载顺序问题导致报错
  function switchView(name) {
    if (window.App && typeof window.App.switchView === "function") {
      window.App.switchView(name);
    }
  }

  /* ============ 对外 API ============ */

  E.init = function (onChanged) {
    state.onChanged = onChanged || null;
    bind();
    return loadSchema().then(loadEngines);
  };
  E.reload = loadEngines;
  E.list = function () { return state.engines; };
  E.rev = function () { return state.rev; };
  E.kinds = function () { return state.kinds; };
  E.languages = function () { return state.languages; };
  E.kindInfo = function (kind) { return state.kindMap[kind] || null; };
  E.displayName = displayName;
  E.isBuiltin = isBuiltinRow;
  E.openEditor = openEditor;
  E.defaultEngine = function () {
    for (var i = 0; i < state.engines.length; i++) {
      if (state.engines[i].isDefault && state.engines[i].enabled) { return state.engines[i]; }
    }
    for (var j = 0; j < state.engines.length; j++) {
      if (state.engines[j].enabled) { return state.engines[j]; }
    }
    return null;
  };
  E.enabledEngines = function () {
    return state.engines.filter(function (e) { return e.enabled; });
  };
  E.findById = function (id) {
    for (var i = 0; i < state.engines.length; i++) {
      if (state.engines[i].id === id) { return state.engines[i]; }
    }
    return null;
  };
})();
