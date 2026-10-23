/*
 * Translation 即时翻译 —— 桥接层（严格按 DBX 官方插件 SDK 的桥接语义实现）
 * ==========================================================================
 *
 * 官方 SDK 定义的 window.dbxPlugin 形状：
 *
 *   window.dbxPlugin = {
 *     ready,                    // Promise<工作台上下文>  ← resolve 的是【纯数据】，不是 API！
 *     get context(),            // 同步读工作台上下文
 *     get locale(), get theme(), get capabilities(),
 *     invoke(method, params, { timeoutMs }),   // 调【侧车】（后端）
 *     request(method, params),                 // 调【宿主】（host.getContext ...）
 *     onEvent(fn), onContext(fn), onInit(fn),  // 事件订阅
 *     copy(text),                              // 写系统剪贴板（无需权限）
 *     notify(...), ...
 *   }
 *
 * 四条硬规则（违反即静默失败，详见 AGENTS.md 第 4 章）：
 *   1. API 对象永远是 window.dbxPlugin 本身。绝不能把 ready 的结果当宿主对象用。
 *   2. 侧车 RPC 走 dbxPlugin.invoke(method, params, { timeoutMs })；
 *      宿主方法走 dbxPlugin.request(method, params)。两者不可混用。
 *   3. 前端不碰磁盘（sandbox iframe 是 opaque origin，localStorage 一碰就抛
 *      SecurityError）。唯一落盘通道是 invoke() → Go 侧车 → translation.db。
 *      【绝不在 localStorage 静默退化】—— 那会让用户以为存上了，重启就丢。
 *   4. 流式翻译靠 onEvent 接收侧车事件（translate/chunk|end|error），
 *      需要 manifest 声明 host.events 权限，否则宿主不会转发。
 *
 * 为什么不用宿主的 stream() 方法：
 *   stream() 的 closeMethod 默认值是 "filesystem/stream/close"，说明它是为
 *   文件流设计的，桌面端对非文件方法的行为没有文档保证。onEvent + 自定义事件
 *   只依赖 host.events（官方文档明确说明「转发后端事件需要 host.events」），
 *   可移植性更可靠。
 */
(function () {
  "use strict";

  var TS = window.TranslationStore = window.TranslationStore || {};

  /* ============ 超时常量 ============
   * 宿主会把 invoke 的 timeoutMs 夹到 1–120000ms（默认 30000），
   * 因此这里的值都必须落在这个区间内，否则会被静默改写。
   */
  var RPC_TIMEOUT = 30000;      // 普通 RPC
  var READY_TIMEOUT = 8000;     // 等 dbxPlugin.ready
  var CTX_TIMEOUT = 2500;       // 读工作台上下文
  var PING_TIMEOUT = 6000;      // 侧车握手
  var BRIDGE_TIMEOUT = 8000;    // 等 window.dbxPlugin 注入
  var TRANSLATE_TIMEOUT = 120000; // 非流式翻译（长文分段可能很久）
  var TEST_TIMEOUT = 30000;     // 引擎连通性测试
  var WATCHDOG_TIMEOUT = 15000; // 启动看门狗

  // 宿主桥接单次 JSON 上限 2 MiB，留余量
  var BRIDGE_PAYLOAD_LIMIT = 1.9 * 1024 * 1024;
  // 导出走 base64，再留一层余量（base64 会膨胀 4/3）
  var MAX_UPSTREAM_BYTES = 1500 * 1000;

  /* ============ 版本号（打包脚本会校验它与 manifest.version 一致）============ */
  var UI_VERSION = "0.1.0";

  /* ============ 诊断日志 ============
   * 安装后白屏 / 存不上时，这份报告是唯一的排查证据。
   * 宿主开发页面只保留内存中最近 500 条日志，且我们自己的分步日志更贴合业务，
   * 因此自建一套（上限 150 条，环形覆盖）。
   */
  var T0 = now();
  function now() {
    return (window.performance && window.performance.now) ? window.performance.now() : Date.now();
  }
  function since() { return now() - T0; }

  var diag = [];
  var diagListeners = [];

  function note(step, ok, detail) {
    diag.push({
      at: Date.now(),
      t: since(),
      ok: (ok === true || ok === false) ? ok : null,
      step: step,
      detail: detail ? String(detail).slice(0, 500) : ""
    });
    if (diag.length > 150) { diag.shift(); }
    for (var i = 0; i < diagListeners.length; i++) {
      try { diagListeners[i](); } catch (e) { /* 监听器异常不能影响主流程 */ }
    }
  }

  function onDiag(fn) { if (typeof fn === "function") { diagListeners.push(fn); } }

  function diagLines() {
    return diag.map(function (d) {
      var mark = d.ok === true ? "[ OK ]" : (d.ok === false ? "[FAIL]" : "[ .. ]");
      return "+" + (d.t / 1000).toFixed(3) + "s " + mark + " " + d.step +
        (d.detail ? "  ->  " + d.detail : "");
    });
  }

  function errText(e) {
    if (!e) { return "未知错误"; }
    if (typeof e === "string") { return e; }
    if (e.message) { return String(e.message); }
    if (e.error) { return String(e.error); }
    try { return JSON.stringify(e).slice(0, 300); } catch (e2) { return String(e); }
  }

  /* ============ 环境探测 ============ */

  function isSandboxed() {
    // 沙箱 iframe 是 opaque origin：window.origin 为字符串 "null"；
    // 个别宿主直接抛异常，那更说明在受限环境里。
    try { return String(window.origin) === "null"; } catch (e) { return true; }
  }

  function envSnapshot() {
    var a = null, keys = [], origin = "?";
    try { a = window.dbxPlugin; } catch (e) { a = null; }
    try { origin = String(window.origin); } catch (e) { origin = "?"; }
    try { keys = a ? Object.keys(a).slice(0, 30) : []; } catch (e) { keys = []; }
    return "origin=" + origin +
      " · sandboxed=" + isSandboxed() +
      " · typeof window.dbxPlugin=" + (a ? typeof a : "undefined") +
      " · invoke=" + (a ? typeof a.invoke : "-") +
      " · request=" + (a ? typeof a.request : "-") +
      " · onEvent=" + (a ? typeof a.onEvent : "-") +
      " · ready=" + (a && a.ready ? typeof a.ready.then : "-") +
      " · keys=[" + keys.join(",") + "]" +
      " · readyState=" + document.readyState;
  }

  function report() {
    var st = status;
    var L = [];
    L.push("=== DBX Translation 即时翻译 · 诊断报告 ===");
    L.push("时间：" + new Date().toLocaleString("zh-CN"));
    L.push("UI 版本：" + UI_VERSION);
    L.push("页面：" + (function () { try { return String(location.href); } catch (e) { return "?"; } })());
    L.push("UA：" + navigator.userAgent);
    L.push("");
    L.push("--- 环境 ---");
    L.push(envSnapshot());
    L.push("");
    L.push("--- 状态 ---");
    L.push("backend=" + st.backend + " · persistent=" + st.persistent + " · ok=" + st.ok);
    L.push("hostAvailable=" + st.hostAvailable + " · sidecarAvailable=" + st.sidecarAvailable);
    L.push("sidecarVersion=" + (st.sidecarVersion || "(未知)"));
    L.push("dataDir=" + (st.dataDir || "(空)"));
    L.push("dbOk=" + st.dbOk + " · dbPath=" + (st.dbPath || "(空)"));
    L.push("secretScheme=" + (st.secretScheme || "(空)") + " · secretSecure=" + st.secretSecure);
    L.push("inflight=" + st.inflight + " · rev=" + st.rev);
    L.push("connectionId=" + (st.connectionId || "(空)"));
    L.push("sidecarError=" + (st.sidecarError || "(无)"));
    L.push("lastError=" + (st.lastError || "(无)"));
    L.push("");
    L.push("--- 诊断日志（" + diag.length + " 条，最早在上）---");
    L.push(diagLines().join("\n") || "(无)");
    return L.join("\n");
  }

  note("桥接模块加载完成（等待 UI 调用 init）", null, "UI 版本 " + UI_VERSION);

  /* ============ 状态 ============ */

  var listeners = [];
  var status = {
    backend: "unknown",       // sidecar | memory | none
    persistent: false,        // 是否真的落盘了
    available: false,
    ok: true,
    lastError: "",
    hostAvailable: false,
    sidecarAvailable: false,
    sidecarError: "",
    sidecarVersion: "",
    dataDir: "",
    dbOk: false,
    dbPath: "",
    secretScheme: "",
    secretSecure: false,      // false 时 UI 必须显示「仅混淆保护」警示条
    inflight: 0,
    rev: 0,
    connectionId: "",
    engineKinds: []
  };

  function emit() {
    for (var i = 0; i < listeners.length; i++) {
      try { listeners[i](status); } catch (e) { /* 忽略 */ }
    }
  }
  function setStatus(patch) {
    for (var k in patch) {
      if (Object.prototype.hasOwnProperty.call(patch, k)) { status[k] = patch[k]; }
    }
    emit();
  }

  /* ============ 小工具 ============ */

  function delay(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  function withTimeout(promise, ms, msg) {
    var timer = null;
    var gate = new Promise(function (_, reject) {
      timer = setTimeout(function () { reject(new Error(msg + "（" + ms + "ms 未响应）")); }, ms);
    });
    function clear() { if (timer) { clearTimeout(timer); timer = null; } }
    return Promise.race([Promise.resolve(promise), gate]).then(
      function (v) { clear(); return v; },
      function (e) { clear(); throw e; }
    );
  }

  // settle 把 rejection 转成 {ok,value}/{ok,error}，避免层层 catch。
  function settle(promise) {
    return Promise.resolve(promise).then(
      function (v) { return { ok: true, value: v }; },
      function (e) { return { ok: false, error: e }; }
    );
  }

  function nonEmptyStr(v) {
    return (typeof v === "string" && v.trim()) ? v.trim() : "";
  }

  function jsonBytes(v) {
    try { return new Blob([JSON.stringify(v)]).size; } catch (e) { return 0; }
  }

  /* ============ 宿主桥接获取 ============ */

  var api = null;
  var apiReady = false;

  // waitBridge 轮询等待 window.dbxPlugin 注入。
  //
  // 为什么用轮询而不是直接读：宿主注入桥接对象的时机不确定，
  // 脚本可能在注入之前就执行了。25ms 的间隔在「响应快」与「不空转」之间平衡。
  function waitBridge(ms) {
    if (api) { return Promise.resolve(api); }
    var deadline = Date.now() + ms;
    return (function poll() {
      if (window.dbxPlugin && typeof window.dbxPlugin === "object") {
        api = window.dbxPlugin;
        return Promise.resolve(api);
      }
      if (Date.now() >= deadline) { return Promise.resolve(null); }
      return delay(25).then(poll);
    })();
  }

  function waitReady(a) {
    if (apiReady) { return Promise.resolve(true); }
    var ready = (a && a.ready && typeof a.ready.then === "function") ? a.ready : Promise.resolve();
    return settle(withTimeout(ready, READY_TIMEOUT, "等待宿主 dbxPlugin.ready")).then(function (r) {
      apiReady = r.ok;
      note("宿主桥接 ready（dbxPlugin.ready）", r.ok,
        r.ok ? "已收到宿主 init 消息" : errText(r.error));
      return r.ok;
    });
  }

  // readContext 读工作台上下文：同步属性优先，request 兜底。
  function readContext(a) {
    var sync = null;
    try { sync = a && a.context; } catch (e) { sync = null; }
    if (sync && typeof sync === "object" && Object.keys(sync).length) {
      note("工作台上下文（同步 dbxPlugin.context）", true, describeCtx(sync));
      return Promise.resolve(sync);
    }
    if (a && typeof a.request === "function") {
      return settle(withTimeout(a.request("host.getContext", {}), CTX_TIMEOUT, "请求 host.getContext"))
        .then(function (r) {
          var ctx = (r.ok && r.value && typeof r.value === "object") ? r.value : {};
          note("工作台上下文（兜底 host.getContext）", r.ok && Object.keys(ctx).length > 0,
            r.ok ? describeCtx(ctx) : errText(r.error));
          return ctx;
        });
    }
    note("工作台上下文", false, "宿主既未提供同步 context，也无 request 方法");
    return Promise.resolve({});
  }

  function describeCtx(ctx) {
    var keys = Object.keys(ctx || {});
    return "字段 [" + keys.slice(0, 8).join(", ") + (keys.length > 8 ? ", …" : "") + "]";
  }

  /* ============ 侧车 RPC ============ */

  function rpcRaw(method, params, timeoutMs) {
    if (!api || typeof api.invoke !== "function") {
      return Promise.reject(new Error("侧车通道未就绪（宿主未提供 dbxPlugin.invoke）"));
    }
    var out;
    try {
      out = api.invoke(method, params || {}, { timeoutMs: timeoutMs || RPC_TIMEOUT });
    } catch (e) {
      return Promise.reject(new Error(method + " 调用失败：" + errText(e)));
    }
    return Promise.resolve(out);
  }

  // rpc 在 rpcRaw 之上做三件事：
  //   1. 记录分步诊断（含耗时）
  //   2. 把 {error} 形态的返回转成 rejection（宿主对预期错误的返回方式）
  //   3. 剥掉可能存在的 result 包装层
  function rpc(method, params, timeoutMs) {
    var started = since();
    var ms = function () { return (since() - started).toFixed(0) + "ms"; };
    return rpcRaw(method, params, timeoutMs).then(function (res) {
      // 宿主把「预期内的业务失败」包成 { error } 返回，而不是 reject
      if (res && typeof res === "object" && res.error && res.ok === undefined) {
        note("侧车调用 " + method, false, ms() + " · " + errText(res.error));
        throw new Error(errText(res.error));
      }
      var out = res;
      if (res && typeof res === "object" && res.result !== undefined && res.ok === undefined) {
        out = res.result;
      }
      note("侧车调用 " + method, true, ms());
      return out;
    }, function (err) {
      note("侧车调用 " + method, false, ms() + " · " + errText(err));
      throw err;
    });
  }

  /* ============ 事件订阅（流式翻译的通道）============ */

  var eventHandlers = [];
  var eventUnsub = null;

  // subscribeEvents 注册 onEvent 监听，把侧车事件分发给业务层。
  //
  // 只注册一次：重复注册会导致同一个 chunk 被处理多次（译文重复）。
  function subscribeEvents() {
    if (eventUnsub || !api || typeof api.onEvent !== "function") { return; }
    try {
      eventUnsub = api.onEvent(function (msg) {
        var method = msg && msg.method;
        var params = (msg && msg.params) || {};
        if (!method) { return; }
        for (var i = 0; i < eventHandlers.length; i++) {
          try { eventHandlers[i](method, params); } catch (e) { /* 单个处理器异常不影响其他 */ }
        }
      });
      note("订阅宿主事件（onEvent）", true, "流式翻译通道已建立");
    } catch (e) {
      note("订阅宿主事件（onEvent）", false, errText(e));
    }
  }

  // onSidecarEvent 注册一个事件处理器，返回取消函数。
  function onSidecarEvent(fn) {
    if (typeof fn !== "function") { return function () {}; }
    eventHandlers.push(fn);
    return function () {
      eventHandlers = eventHandlers.filter(function (x) { return x !== fn; });
    };
  }

  /* ============ 降级后端 ============
   * 侧车不可用时退到「会话内存」：数据只在当前页面生命周期内有效，
   * 刷新或重开工作台就丢。
   *
   * 【绝不静默退化】：status.persistent=false 会让 UI 顶部常驻红色横幅
   * 明确告知「未持久化」，用户不会误以为数据已保存。
   */
  var memoryData = { engines: [], history: [], prefs: null, rev: 0 };

  function failHard(reason) {
    setStatus({
      backend: "memory", persistent: false, available: false, ok: false,
      sidecarAvailable: false, sidecarError: reason, lastError: reason
    });
  }

  /* ============ 初始化 ============ */

  var initCalled = false;
  var initDone = false;

  function init() {
    if (initDone) { return Promise.resolve(status); }
    initCalled = true;
    var t0 = since();
    note("=== 桥接初始化开始 ===", null, UI_VERSION + " @ " + new Date().toLocaleTimeString("zh-CN"));
    note("环境快照", null, envSnapshot());

    // 看门狗：启动超时也要在 UI 留下「卡在哪一步」的证据，
    // 否则用户只看到一片空白，完全不知道该反馈什么。
    var watchdog = setTimeout(function () {
      if (!initDone) {
        note("启动看门狗触发", false, "已耗时 " + WATCHDOG_TIMEOUT + "ms 仍未完成初始化");
        setStatus({ lastError: "初始化超时，卡在：" + (diag.length ? diag[diag.length - 1].step : "未知步骤") });
      }
    }, WATCHDOG_TIMEOUT);

    return waitBridge(BRIDGE_TIMEOUT).then(function (a) {
      note("等 window.dbxPlugin 注入", !!a, (since() - t0).toFixed(0) + "ms" +
        (a ? " · 已拿到" : " · 超时 " + BRIDGE_TIMEOUT + "ms"));
      if (!a) {
        var msg = "宿主未注入 window.dbxPlugin（当前不在 DBX 插件工作台内）";
        setStatus({ hostAvailable: false });
        failHard(msg);
        clearTimeout(watchdog);
        initDone = true;
        return status;
      }
      setStatus({ hostAvailable: true });
      note("获取 dbxPlugin 桥接", true,
        "invoke=" + (typeof a.invoke) + " request=" + (typeof a.request) + " onEvent=" + (typeof a.onEvent));

      return waitReady(a)
        .then(function () { return readContext(a); })
        .then(function (ctx) {
          setStatus({ connectionId: nonEmptyStr(ctx && ctx.connectionId) });
          subscribeEvents();
          return ping();
        })
        .then(function () {
          clearTimeout(watchdog);
          initDone = true;
          note("=== 桥接初始化完成 ===", true, (since() - t0).toFixed(0) + "ms");
          return status;
        })
        .catch(function (err) {
          clearTimeout(watchdog);
          initDone = true;
          note("=== 桥接初始化失败 ===", false, errText(err));
          return status;
        });
    });
  }

  // ping 与侧车握手，并把健康状态写进 status。
  function ping() {
    return settle(withTimeout(rpc("translate/ping", {}, PING_TIMEOUT), PING_TIMEOUT + 500, "侧车握手 translate/ping"))
      .then(function (r) {
        if (!r.ok) {
          var why = "侧车不可用：" + errText(r.error);
          note("侧车握手 translate/ping", false, why);
          failHard(why);
          return false;
        }
        var pl = r.value || {};
        note("侧车握手 translate/ping", true,
          "version=" + (pl.version || "?") + " dbOk=" + pl.dbOk +
          " secret=" + (pl.secretScheme || "?") + "(secure=" + pl.secretSecure + ")");
        setStatus({
          backend: "sidecar",
          persistent: pl.dbOk === true,
          available: true,
          ok: true,
          lastError: "",
          sidecarAvailable: true,
          sidecarError: "",
          sidecarVersion: nonEmptyStr(pl.version),
          dataDir: nonEmptyStr(pl.dataDir),
          dbOk: pl.dbOk === true,
          dbPath: nonEmptyStr(pl.dbPath),
          secretScheme: nonEmptyStr(pl.secretScheme),
          secretSecure: pl.secretSecure === true,
          inflight: pl.inflight || 0,
          rev: pl.rev || 0,
          engineKinds: pl.engineKinds || []
        });
        if (pl.dbOk !== true) {
          // 侧车活着但库打不开：能翻译（不落历史），但配置存不了。
          // 这是「部分可用」状态，必须明确告知而不是假装正常。
          setStatus({ lastError: "数据存储不可用：引擎配置与翻译历史无法保存。请检查插件数据目录的读写权限。" });
        }
        return true;
      });
  }

  /* ============ 对外 API ============ */

  var Store = {
    VERSION: UI_VERSION,

    // --- 生命周期 ---
    init: init,
    ping: ping,
    status: function () {
      var copy = {};
      for (var k in status) {
        if (Object.prototype.hasOwnProperty.call(status, k)) { copy[k] = status[k]; }
      }
      copy.diag = diagLines();
      return copy;
    },
    isPersistent: function () { return status.persistent === true; },
    isSidecarAlive: function () { return status.sidecarAvailable === true; },

    // --- 诊断 ---
    onStatus: function (fn) {
      if (typeof fn === "function") { listeners.push(fn); }
      return function () { listeners = listeners.filter(function (x) { return x !== fn; }); };
    },
    onDiag: onDiag,
    log: note,
    report: report,
    env: envSnapshot,

    // --- 通用 RPC ---
    invoke: function (method, params, timeoutMs) { return rpc(method, params, timeoutMs); },
    TIMEOUTS: {
      rpc: RPC_TIMEOUT, translate: TRANSLATE_TIMEOUT, test: TEST_TIMEOUT
    },
    LIMITS: {
      bridgePayload: BRIDGE_PAYLOAD_LIMIT,
      maxUpstreamBytes: MAX_UPSTREAM_BYTES
    },
    jsonBytes: jsonBytes,

    // --- 事件（流式翻译）---
    onSidecarEvent: onSidecarEvent,

    // onInit 订阅宿主的 init 消息。
    //
    // 为什么需要它：某些宿主在桥接就绪时只发 init 不发 env，
    // 若只监听 dbx-plugin-env，首屏就可能用错主题/语言。
    // 老宿主没有该方法，调用方必须 try/catch 包住。
    //
    // 返回取消订阅函数（宿主没提供时返回空函数，保证调用方无需判空）。
    onInit: function (fn) {
      if (typeof fn !== "function") { return function () {}; }
      if (api && typeof api.onInit === "function") {
        try {
          var off = api.onInit(fn);
          return (typeof off === "function") ? off : function () {};
        } catch (e) { return function () {}; }
      }
      return function () {};
    },

    // --- 元信息 ---
    getSchema: function () { return rpc("translate/schema", {}, RPC_TIMEOUT); },

    // --- 引擎管理 ---
    listEngines: function () {
      return rpc("engines/list", {}, RPC_TIMEOUT).then(function (r) {
        if (r && typeof r.rev === "number") { setStatus({ rev: r.rev }); }
        return r;
      });
    },
    getEngine: function (id) { return rpc("engines/get", { id: id }, RPC_TIMEOUT); },
    saveEngine: function (payload) { return withRevRetry("engines/save", payload); },
    deleteEngine: function (id) { return withRevRetry("engines/delete", { id: id }); },
    setDefaultEngine: function (id) { return withRevRetry("engines/setDefault", { id: id }); },
    toggleEngine: function (id, enabled) { return withRevRetry("engines/toggle", { id: id, enabled: enabled }); },
    reorderEngines: function (ids) { return withRevRetry("engines/reorder", { ids: ids }); },
    testEngine: function (payload) { return rpc("engines/test", payload, TEST_TIMEOUT); },

    // --- 翻译 ---
    translateRun: function (payload) { return rpc("translate/run", payload, TRANSLATE_TIMEOUT); },
    translateStart: function (payload) { return rpc("translate/start", payload, RPC_TIMEOUT); },
    translateCancel: function (taskId) { return rpc("translate/cancel", { taskId: taskId }, RPC_TIMEOUT); },

    // --- 历史 ---
    listHistory: function (q) { return rpc("history/list", q || {}, RPC_TIMEOUT); },
    starHistory: function (id, starred) { return rpc("history/star", { id: id, starred: starred }, RPC_TIMEOUT); },
    deleteHistory: function (ids) { return rpc("history/delete", { ids: ids }, RPC_TIMEOUT); },
    clearHistory: function (keepStarred) { return rpc("history/clear", { keepStarred: keepStarred }, RPC_TIMEOUT); },

    // --- 偏好 ---
    getPrefs: function () {
      return rpc("prefs/get", {}, RPC_TIMEOUT).then(function (r) {
        if (r && typeof r.rev === "number") { setStatus({ rev: r.rev }); }
        return (r && r.prefs) || {};
      }).catch(function () { return {}; });
    },
    setPrefs: function (prefs) { return withRevRetry("prefs/set", { prefs: prefs || {} }); },

    // --- 数据导入导出 ---
    exportData: function (includeSecrets) {
      return rpc("data/export", { includeSecrets: includeSecrets === true }, TRANSLATE_TIMEOUT);
    },
    importData: function (dataBase64, mode) {
      return rpc("data/import", { dataBase64: dataBase64, mode: mode || "merge" }, TRANSLATE_TIMEOUT);
    },

    // --- 宿主能力 ---
    copyText: function (text) {
      // 写剪贴板不需要权限（读才需要 host.clipboard:read）
      if (api && typeof api.copy === "function") {
        return Promise.resolve(api.copy(text)).then(function () { return true; })
          .catch(function () { return legacyCopy(text); });
      }
      return Promise.resolve(legacyCopy(text));
    },
    hasHostSave: function () { return !!(api && typeof api.saveFile === "function"); },
    saveFile: function (fileName, contentType, bytes) { return hostSaveFile(fileName, contentType, bytes); },
    theme: function () { return readTheme(); },
    // themeObject 返回完整主题负载 { appearance, tokens, editor }，
    // 供 app.js 的 applyTheme 把宿主的颜色/字体/圆角 token 应用到文档根元素。
    themeObject: function () { return readThemeObject(); },
    locale: function () { try { return (api && api.locale) || "zh-CN"; } catch (e) { return "zh-CN"; } }
  };

  /* ============ rev 乐观锁与冲突重试 ============
   * 侧车用 meta.rev 做乐观锁：前端回声 rev，不匹配则拒绝并回传最新 rev。
   * 这防的是「两个 DBX 窗口同时改配置，后保存的静默覆盖先保存的」。
   *
   * 冲突时刷新 rev 后【只重试一次】：
   *   - 单会话下不会冲突，重试一次足够消掉偶发的时序问题
   *   - 多会话下无限重试会变成两个窗口互相覆盖的死循环
   */
  function withRevRetry(method, payload, tries) {
    tries = tries || 0;
    var body = {};
    for (var k in (payload || {})) {
      if (Object.prototype.hasOwnProperty.call(payload, k)) { body[k] = payload[k]; }
    }
    if (body.rev === undefined) { body.rev = status.rev; }

    return rpc(method, body, RPC_TIMEOUT).then(function (res) {
      if (res && typeof res === "object") {
        if (typeof res.rev === "number") { setStatus({ rev: res.rev }); }
        // 冲突：刷新 rev 后重试一次
        if (res.conflict === true && tries < 1) {
          note(method + " 遇到 rev 冲突，刷新后重试", false,
            "本地 rev=" + body.rev + " 服务端 rev=" + res.newRev);
          if (typeof res.newRev === "number") { setStatus({ rev: res.newRev }); }
          return withRevRetry(method, payload, tries + 1);
        }
      }
      return res;
    });
  }

  /* ============ 主题 ============ */

  // readThemeObject 读宿主的【完整】主题负载，兼容「属性」与「函数」两种形态。
  //
  // 官方文档说明 env 消息的 theme 结构为 { appearance, tokens, editor? }：
  //   - appearance 是 "light" | "dark"
  //   - tokens 是按前缀扫取的根节点设计变量（--color-*、--radius-*、--font-*）
  //   - editor 是 SQL 编辑器设置的结构化快照（含 fontFamily / fontSize / theme）
  //
  // 不同宿主版本实现不同：有的把 theme 做成 getter 返回对象，有的做成函数，
  // 老宿主甚至只返回 "dark"/"light" 字符串。三种形态都要接，
  // 统一归一成 { appearance, tokens, editor } 结构，缺失的字段给安全默认值。
  function readThemeObject() {
    var fallback = { appearance: "light", tokens: {}, editor: null };
    try {
      if (!api || !api.theme) { return fallback; }
      var th = api.theme;
      if (typeof th === "function") { th = th(); }
      // 老宿主：theme 直接是 "dark"/"light" 字符串
      if (typeof th === "string") {
        return { appearance: th === "dark" ? "dark" : "light", tokens: {}, editor: null };
      }
      if (th && typeof th === "object") {
        return {
          appearance: th.appearance === "dark" ? "dark" : "light",
          tokens: (th.tokens && typeof th.tokens === "object") ? th.tokens : {},
          editor: (th.editor && typeof th.editor === "object") ? th.editor : null
        };
      }
      return fallback;
    } catch (e) { return fallback; }
  }

  // readTheme 只返回外观字符串，供不需要 tokens 的调用方使用（向后兼容）。
  function readTheme() {
    return readThemeObject().appearance;
  }

  /* ============ 剪贴板降级 ============ */

  // legacyCopy 在宿主没有 copy 桥接时的降级方案。
  //
  // 沙箱里 navigator.clipboard 不可用（无权限），只能用老的
  // execCommand("copy")：创建一个临时 textarea，选中后执行复制命令。
  function legacyCopy(text) {
    try {
      var ta = document.createElement("textarea");
      ta.value = String(text == null ? "" : text);
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.top = "-1000px";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      var ok = document.execCommand("copy");
      document.body.removeChild(ta);
      return ok;
    } catch (e) {
      return false;
    }
  }

  /* ============ 宿主「另存为」 ============
   * 沙箱 iframe 里的 blob 下载会被宿主【无声取消】（不报错，文件也不出现），
   * 因此导出一律走宿主的 saveFile（原生保存对话框）。
   *
   * 开发宿主【不支持】 saveFile，所以必须做特性检测并降级为「复制到剪贴板」。
   */
  function base64ToBytes(b64) {
    var bin = atob(String(b64 || "").replace(/\s+/g, ""));
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i) & 0xff; }
    return out;
  }

  function toExactBuffer(v) {
    var u8;
    if (v instanceof ArrayBuffer) { return v; }
    if (typeof v === "string") { u8 = base64ToBytes(v); }
    else if (v instanceof Uint8Array) { u8 = v; }
    else { u8 = new Uint8Array(v || 0); }
    if (u8.byteOffset === 0 && u8.byteLength === u8.buffer.byteLength) { return u8.buffer; }
    var copy = new Uint8Array(u8.byteLength);
    copy.set(u8);
    return copy.buffer;
  }

  function hostSaveFile(fileName, contentType, bytes) {
    if (!Store.hasHostSave()) {
      note("宿主 saveFile", false, "当前宿主没有 saveFile 能力（开发宿主不支持），需降级为复制");
      return Promise.resolve({ ok: false, canceled: false, path: "", unsupported: true,
        error: "当前 DBX 宿主不支持「另存为」对话框" });
    }
    var buf, size = 0;
    try {
      buf = toExactBuffer(bytes);
      size = buf.byteLength;
    } catch (e) {
      note("宿主 saveFile", false, "字节准备失败：" + errText(e));
      return Promise.resolve({ ok: false, canceled: false, path: "", error: errText(e) });
    }
    return Promise.resolve(api.saveFile({ fileName: fileName, contentType: contentType }, buf))
      .then(function (res) {
        if (res === null || res === undefined) {
          note("宿主 saveFile", null, "用户取消了保存：" + fileName);
          return { ok: false, canceled: true, path: "", error: "已取消" };
        }
        var pth = (res && typeof res.path === "string") ? res.path : "";
        note("宿主 saveFile", true, fileName + "（" + size + " B）-> " + (pth || "(宿主未回传路径)"));
        return { ok: true, canceled: false, path: pth, error: "" };
      })
      .catch(function (e) {
        note("宿主 saveFile", false, fileName + "：" + errText(e));
        return { ok: false, canceled: false, path: "", error: errText(e) };
      });
  }

  // 导出到各模块
  for (var key in Store) {
    if (Object.prototype.hasOwnProperty.call(Store, key)) { TS[key] = Store[key]; }
  }
})();
