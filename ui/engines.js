/*
 * 翻译助手 —— 引擎管理
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
    editingKind: "",     // 弹框里当前选中的 kind
    secretTouched: false,// 用户是否动过密钥输入框
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

    // 副标题：显示端点或模型，让用户不点开也能分辨同类引擎
    var sub = cfg.endpoint || cfg.model || cfg.cliPath || "";
    if (cfg.endpoint && cfg.model) { sub = cfg.model + " · " + cfg.endpoint; }

    var badges = [];
    if (e.isDefault) { badges.push(h("span", { class: "badge primary", text: t("defaultBadge") })); }
    if (!e.enabled) { badges.push(h("span", { class: "badge muted", text: t("disabledBadge") })); }
    if (kindInfo.streaming) { badges.push(h("span", { class: "badge ok", text: "流式" })); }

    var nameRow = h("div", { class: "engine-name" }, [
      h("span", { text: e.name || e.kind }),
      h("span", { class: "badge", text: label(kindInfo, "label", "labelEn") || e.kind })
    ].concat(badges));

    var subRow = h("div", { class: "engine-sub" }, [
      sub ? h("span", { text: sub }) : null,
      e.hasSecret ? h("span", { class: "fingerprint", text: "  🔑 " + (e.fingerprint || "") }) : null
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
      }, t("edit")),
      h("button", {
        class: "ghost sm danger-text", type: "button",
        onclick: function () { actDelete(e); }
      }, t("delete"))
    ]);

    var card = h("div", {
      class: "engine-card" + (e.isDefault ? " is-default" : "") + (e.enabled ? "" : " is-disabled"),
      dataset: { id: e.id, index: String(index) },
      draggable: "true"
    }, [
      h("span", { class: "drag-handle", title: "拖动排序" }, "⠿"),
      h("div", { class: "engine-main" }, [nameRow, subRow]),
      actions
    ]);

    bindDrag(card, e, index);
    return card;
  }

  /* ============ 拖拽排序 ============
   * 用原生 HTML5 drag & drop，不引第三方库。
   * 排序结果通过 engines/reorder 持久化，侧车会把未列出的引擎排到后面
   * （而不是丢弃），所以即使拖拽逻辑漏了某个引擎也不会导致它消失。
   */
  var dragIndex = -1;

  function bindDrag(card, e, index) {
    card.addEventListener("dragstart", function (ev) {
      dragIndex = index;
      card.classList.add("dragging");
      try { ev.dataTransfer.setData("text/plain", e.id); } catch (err) { /* 某些宿主不允许 */ }
      if (ev.dataTransfer) { ev.dataTransfer.effectAllowed = "move"; }
    });
    card.addEventListener("dragend", function () {
      dragIndex = -1;
      card.classList.remove("dragging");
      clearDragMarks();
    });
    card.addEventListener("dragover", function (ev) {
      ev.preventDefault();
      if (ev.dataTransfer) { ev.dataTransfer.dropEffect = "move"; }
    });
    card.addEventListener("dragenter", function () {
      if (dragIndex >= 0 && dragIndex !== index) { card.classList.add("drag-over"); }
    });
    card.addEventListener("dragleave", function () { card.classList.remove("drag-over"); });
    card.addEventListener("drop", function (ev) {
      ev.preventDefault();
      card.classList.remove("drag-over");
      if (dragIndex < 0 || dragIndex === index) { return; }
      applyReorder(dragIndex, index);
    });
  }

  function clearDragMarks() {
    var box = $("engine-list");
    if (!box) { return; }
    var marks = box.querySelectorAll(".drag-over");
    for (var i = 0; i < marks.length; i++) { marks[i].classList.remove("drag-over"); }
  }

  function applyReorder(from, to) {
    var ids = state.engines.map(function (e) { return e.id; });
    var moved = ids.splice(from, 1)[0];
    ids.splice(to, 0, moved);
    // 乐观更新：先改界面，失败再回滚，避免用户等待
    var snapshot = state.engines.slice();
    state.engines = ids.map(function (id) {
      for (var i = 0; i < snapshot.length; i++) { if (snapshot[i].id === id) { return snapshot[i]; } }
      return null;
    }).filter(Boolean);
    renderList();

    S.reorderEngines(ids).then(function (res) {
      state.rev = (res && res.rev) || state.rev;
    }).catch(function (err) {
      reportErr("排序", err);
      state.engines = snapshot;   // 回滚
      renderList();
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

    var titleEl = $("engine-modal-title");
    if (titleEl) { titleEl.textContent = e ? t("editEngine") : t("addEngine"); }

    show("btn-engine-delete", !!e);
    clearFormError();

    renderKindSelect(e);
    // 新建时默认选第一个引擎类型（openai 覆盖面最广）
    state.editingKind = e ? e.kind : (state.kinds[0] ? state.kinds[0].kind : "");
    setVal("engine-kind", state.editingKind);
    // 编辑时锁定类型：换类型等于换一套完全不同的配置，应该删掉重建
    var kindSel = $("engine-kind");
    if (kindSel) { kindSel.disabled = !!e; }

    renderKindDesc();
    renderFields();
    show("engine-modal", true);
    focusFirstField();
  }

  function closeEditor() {
    show("engine-modal", false);
    state.editing = null;
    state.editingKind = "";
    state.secretTouched = false;
    var box = $("engine-fields");
    if (box) { box.textContent = ""; }
    clearFormError();
  }

  function renderKindSelect(e) {
    var sel = $("engine-kind");
    if (!sel) { return; }
    sel.textContent = "";
    state.kinds.forEach(function (k) {
      sel.appendChild(h("option", { value: k.kind, text: label(k, "label", "labelEn") || k.kind }));
    });
  }

  function renderKindDesc() {
    var k = state.kindMap[state.editingKind];
    txt("engine-kind-desc", k ? label(k, "description", "descriptionEn") : "");
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

    var payload = {
      kind: state.editingKind,
      name: (config.displayName || config.name || "").trim(),
      config: config,
      rev: state.rev
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

    on("engine-kind", "change", function () {
      state.editingKind = val("engine-kind");
      state.secretTouched = false;
      renderKindDesc();
      renderFields();
      clearFormError();
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
