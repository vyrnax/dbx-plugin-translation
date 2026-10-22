/*
 * 翻译助手 —— 界面国际化
 * ==========================================================================
 * 为什么前端要自建 i18n：
 *
 *   manifest.json 的 localizations 只覆盖【manifest 层】的文案
 *   （插件名称、说明、贡献点 label/description、连接字段与动作），
 *   官方文档明确说明它「不会翻译插件 UI 自己的文本」。
 *   因此界面里的每个字符串都必须由前端自己管。
 *
 * 语言来源：dbxPlugin.locale，并监听 dbx-plugin-env 事件实时刷新
 * （用户改宿主语言后不需要重开工作台）。
 */
(function () {
  "use strict";

  var I18N = window.TranslationI18N = window.TranslationI18N || {};

  var dict = {
    "zh-CN": {
      appTitle: "翻译助手",
      viewTranslate: "翻译",
      viewEngines: "引擎",
      viewHistory: "历史",
      viewSettings: "设置",

      // 翻译页
      sourceText: "原文",
      targetText: "译文",
      sourcePlaceholder: "在此输入或粘贴要翻译的文本…",
      dstEmpty: "译文将显示在这里",
      translate: "翻译",
      translating: "翻译中…",
      cancel: "取消",
      copy: "复制",
      clear: "清空",
      paste: "粘贴",
      swapLang: "交换语言方向",
      engine: "引擎",
      manage: "管理",
      instant: "输入即译",
      stream: "流式输出",
      noEngineTitle: "还没有配置翻译引擎",
      noEngineDesc: "添加一个引擎后即可开始翻译。支持 OpenAI 兼容大模型（DeepSeek、通义、Ollama 等）、DeepL、百度、有道、阿里云，以及本地 CLI 代理。",
      addEngine: "添加引擎",
      copied: "已复制到剪贴板",
      copyFailed: "复制失败，请手动选择文本复制",
      emptySource: "请先输入要翻译的文本",
      tooLongForInstant: "文本较长（{n} 字符），已暂停自动翻译。请点击「翻译」按钮手动执行。",
      cancelled: "已取消翻译",
      hardCutHint: "文本过长已强制切分，译文质量可能下降",

      // 引擎页
      engineList: "引擎列表",
      engineEmptyTitle: "还没有引擎",
      engineEmptyDesc: "点击右上角「添加引擎」创建第一个翻译引擎。",
      engineType: "引擎类型",
      testConnection: "测试连接",
      testing: "测试中…",
      delete: "删除",
      save: "保存",
      editEngine: "编辑引擎",
      defaultBadge: "默认",
      disabledBadge: "已禁用",
      setDefault: "设为默认",
      enable: "启用",
      disable: "禁用",
      edit: "编辑",
      deleteEngineConfirm: "确定删除引擎「{name}」吗？",
      deleteEngineNote: "该引擎的 API Key 也会一并删除，此操作不可撤销。",
      engineSaved: "引擎已保存",
      engineDeleted: "引擎已删除",
      testOk: "连接成功（{ms} 毫秒）",
      secretKept: "（保留原有 API Key）",
      secretCleared: "（已清除 API Key）",
      fieldRequired: "「{label}」为必填项",
      insecureSecretBanner: "系统密钥库不可用，API Key 仅以机器绑定方式混淆存储，安全性降低。",

      // 历史页
      searchHistory: "搜索原文或译文…",
      all: "全部",
      starred: "已收藏",
      clearHistory: "清空历史",
      historyEmptyTitle: "还没有翻译记录",
      historyEmptyDesc: "翻译过的内容会自动保存在这里。",
      clearHistoryConfirm: "确定清空全部翻译历史吗？",
      clearHistoryNote: "已收藏的记录会保留。此操作不可撤销。",
      historyCleared: "已清空 {n} 条记录",
      historyDeleted: "已删除 {n} 条记录",
      star: "收藏",
      unstar: "取消收藏",
      reuse: "用这条重新翻译",
      prevPage: "上一页",
      nextPage: "下一页",
      pageInfo: "第 {cur} / {total} 页，共 {n} 条",

      // 设置页
      settings: "设置",
      groupInstant: "输入即译",
      groupTranslate: "翻译行为",
      groupHistory: "历史",
      groupData: "数据与诊断",
      instantEnabled: "启用输入即译",
      instantDebounce: "触发延迟（毫秒）",
      instantMinChars: "最少字符数",
      instantMaxLength: "自动翻译长度上限",
      instantNote: "超过长度上限时不会自动翻译，需手动点击「翻译」按钮。",
      streamEnabled: "启用流式输出",
      chunkDelay: "分段间隔（毫秒）",
      chunkDelayNote: "长文本会被自动分段翻译。百度、有道等免费额度通常为 1 QPS，建议保持 1100 毫秒以上以避免触发限流。",
      defaultSourceLang: "默认源语言",
      defaultTargetLang: "默认目标语言",
      historyLimit: "保留条数上限",
      historyLimitNote: "超出上限时自动删除最旧的记录，已收藏的记录不会被删除。",
      dataDir: "数据目录",
      dbFile: "数据库文件",
      sidecarVersion: "侧车版本",
      secretScheme: "密钥保护方案",
      saveSettings: "保存设置",
      settingsSaved: "设置已保存",
      reconnect: "重新连接侧车",
      exportData: "导出数据…",
      importData: "导入数据…",
      showDiag: "查看诊断报告",
      diagTitle: "诊断报告",
      diagHint: "遇到问题时，复制下面的报告发给开发者。报告不含 API Key（已自动脱敏）。",
      copyReport: "复制报告",

      // 导入导出
      exportTitle: "导出数据",
      exportQuestion: "导出内容包含引擎配置、翻译历史与界面偏好。是否同时包含 API Key？",
      exportWithSecrets: "包含 API Key（明文，用于迁移到另一台机器）",
      exportWithoutSecrets: "不包含 API Key（推荐，更安全）",
      exportSecretWarning: "导出的文件将包含 API Key 明文，请通过安全渠道传输，导入后立即删除。",
      exportDone: "已导出到 {path}",
      exportCopied: "宿主不支持「另存为」，导出内容已复制到剪贴板，请粘贴保存到文件。",
      importDone: "导入完成：引擎 {engines} 个、历史 {history} 条、密钥 {secrets} 个",
      importMerge: "合并（保留现有数据）",
      importReplace: "替换（先清空现有数据）",
      importTitle: "导入数据",
      importQuestion: "选择导入方式：",

      // 状态条
      statusInit: "初始化中…",
      statusReady: "就绪",
      statusNoSidecar: "侧车不可用：数据未持久化",
      statusNoDb: "数据存储不可用：配置与历史无法保存",
      statusNotPersistent: "未持久化（会话内存）",
      retry: "重试连接",
      reconnecting: "正在重新连接侧车…",
      reconnected: "侧车已重新连接",
      reconnectFailed: "侧车重连失败",

      // 通用
      confirm: "确定",
      error: "出错",
      unknownError: "未知错误",
      conflictRetry: "配置已被其它会话修改，已自动重试",
      genericFail: "操作失败：{msg}"
    },

    "en": {
      appTitle: "Translation Assistant",
      viewTranslate: "Translate",
      viewEngines: "Engines",
      viewHistory: "History",
      viewSettings: "Settings",

      sourceText: "Source",
      targetText: "Translation",
      sourcePlaceholder: "Type or paste text to translate…",
      dstEmpty: "Translation will appear here",
      translate: "Translate",
      translating: "Translating…",
      cancel: "Cancel",
      copy: "Copy",
      clear: "Clear",
      paste: "Paste",
      swapLang: "Swap languages",
      engine: "Engine",
      manage: "Manage",
      instant: "Translate as you type",
      stream: "Streaming",
      noEngineTitle: "No translation engine configured",
      noEngineDesc: "Add an engine to start translating. Supports OpenAI-compatible LLMs (DeepSeek, Qwen, Ollama…), DeepL, Baidu, Youdao, Alibaba Cloud and local CLI proxies.",
      addEngine: "Add engine",
      copied: "Copied to clipboard",
      copyFailed: "Copy failed, please select and copy manually",
      emptySource: "Please enter text to translate first",
      tooLongForInstant: "Text is long ({n} chars); auto-translate paused. Click Translate to run manually.",
      cancelled: "Translation cancelled",
      hardCutHint: "Text was force-split; quality may be reduced",

      engineList: "Engines",
      engineEmptyTitle: "No engines yet",
      engineEmptyDesc: "Click “Add engine” to create your first translation engine.",
      engineType: "Engine type",
      testConnection: "Test connection",
      testing: "Testing…",
      delete: "Delete",
      save: "Save",
      editEngine: "Edit engine",
      defaultBadge: "Default",
      disabledBadge: "Disabled",
      setDefault: "Set as default",
      enable: "Enable",
      disable: "Disable",
      edit: "Edit",
      deleteEngineConfirm: "Delete engine “{name}”?",
      deleteEngineNote: "Its API key will also be removed. This cannot be undone.",
      engineSaved: "Engine saved",
      engineDeleted: "Engine deleted",
      testOk: "Connected ({ms} ms)",
      secretKept: "(existing API key kept)",
      secretCleared: "(API key cleared)",
      fieldRequired: "“{label}” is required",
      insecureSecretBanner: "OS keychain unavailable; API keys are stored with machine-bound obfuscation only, which is less secure.",

      searchHistory: "Search source or translation…",
      all: "All",
      starred: "Starred",
      clearHistory: "Clear history",
      historyEmptyTitle: "No translations yet",
      historyEmptyDesc: "Translated text is saved here automatically.",
      clearHistoryConfirm: "Clear all translation history?",
      clearHistoryNote: "Starred entries are kept. This cannot be undone.",
      historyCleared: "Cleared {n} entries",
      historyDeleted: "Deleted {n} entries",
      star: "Star",
      unstar: "Unstar",
      reuse: "Translate this again",
      prevPage: "Previous",
      nextPage: "Next",
      pageInfo: "Page {cur} / {total}, {n} entries",

      settings: "Settings",
      groupInstant: "Translate as you type",
      groupTranslate: "Translation behaviour",
      groupHistory: "History",
      groupData: "Data & diagnostics",
      instantEnabled: "Enable translate-as-you-type",
      instantDebounce: "Debounce (ms)",
      instantMinChars: "Minimum characters",
      instantMaxLength: "Auto-translate length limit",
      instantNote: "Longer text is not auto-translated; click Translate to run it manually.",
      streamEnabled: "Enable streaming output",
      chunkDelay: "Delay between chunks (ms)",
      chunkDelayNote: "Long text is split automatically. Free tiers of Baidu/Youdao are usually 1 QPS, so keep this at 1100 ms or higher to avoid rate limits.",
      defaultSourceLang: "Default source language",
      defaultTargetLang: "Default target language",
      historyLimit: "History limit",
      historyLimitNote: "Oldest entries are pruned beyond this limit; starred entries are never pruned.",
      dataDir: "Data directory",
      dbFile: "Database file",
      sidecarVersion: "Sidecar version",
      secretScheme: "Key protection",
      saveSettings: "Save settings",
      settingsSaved: "Settings saved",
      reconnect: "Reconnect sidecar",
      exportData: "Export data…",
      importData: "Import data…",
      showDiag: "View diagnostics",
      diagTitle: "Diagnostics",
      diagHint: "Copy this report when reporting an issue. It contains no API keys (redacted automatically).",
      copyReport: "Copy report",

      exportTitle: "Export data",
      exportQuestion: "The export contains engine config, history and preferences. Include API keys?",
      exportWithSecrets: "Include API keys (plaintext, for migrating to another machine)",
      exportWithoutSecrets: "Exclude API keys (recommended, safer)",
      exportSecretWarning: "The exported file will contain API keys in plaintext. Transfer it securely and delete it after importing.",
      exportDone: "Exported to {path}",
      exportCopied: "This host has no Save dialog; the export was copied to the clipboard. Paste it into a file to keep it.",
      importDone: "Imported: {engines} engines, {history} history entries, {secrets} keys",
      importMerge: "Merge (keep existing data)",
      importReplace: "Replace (clear existing data first)",
      importTitle: "Import data",
      importQuestion: "Choose an import mode:",

      statusInit: "Initialising…",
      statusReady: "Ready",
      statusNoSidecar: "Sidecar unavailable: data is not persisted",
      statusNoDb: "Storage unavailable: config and history cannot be saved",
      statusNotPersistent: "Not persisted (session memory)",
      retry: "Retry",
      reconnecting: "Reconnecting sidecar…",
      reconnected: "Sidecar reconnected",
      reconnectFailed: "Sidecar reconnection failed",

      confirm: "OK",
      error: "Error",
      unknownError: "Unknown error",
      conflictRetry: "Config was changed by another session; retried automatically",
      genericFail: "Operation failed: {msg}"
    }
  };

  var current = "zh-CN";

  // detect 从宿主读取语言，并归一到字典支持的键。
  //
  // 宿主可能给 "zh-CN"、"zh"、"en-US"、"en" 等形态，
  // 归一化规则：以 zh 开头一律用 zh-CN，其余非中文一律用 en。
  // 这样即使宿主给出 "zh-TW" 也有合理回退，不会整页变成键名。
  function detect() {
    var loc = "zh-CN";
    try {
      if (window.dbxPlugin && window.dbxPlugin.locale) { loc = String(window.dbxPlugin.locale); }
    } catch (e) { /* 忽略 */ }
    var low = loc.toLowerCase();
    current = (low.indexOf("zh") === 0) ? "zh-CN" : "en";
    return current;
  }

  // t 取当前语言的文案，支持 {name} 占位符替换。
  //
  // 缺失键时回退到 zh-CN，再回退到键名本身 —— 绝不返回 undefined，
  // 否则界面会出现 "undefined" 字样，比显示英文更难排查。
  function t(key, vars) {
    var table = dict[current] || dict["zh-CN"];
    var s = table[key];
    if (s === undefined) { s = dict["zh-CN"][key]; }
    if (s === undefined) { s = key; }
    if (vars && typeof s === "string") {
      s = s.replace(/\{(\w+)\}/g, function (m, name) {
        return (vars[name] !== undefined && vars[name] !== null) ? String(vars[name]) : m;
      });
    }
    return s;
  }

  // apply 把 DOM 里带 data-i18n / data-i18n-placeholder 的节点全部刷新。
  //
  // 用属性驱动而非在 JS 里逐个 setText：新增界面元素时只要加属性，
  // 不需要改 apply 的代码，少一处漏改的机会。
  function apply(root) {
    var scope = root || document;
    var nodes = scope.querySelectorAll("[data-i18n]");
    for (var i = 0; i < nodes.length; i++) {
      var key = nodes[i].getAttribute("data-i18n");
      if (key) { nodes[i].textContent = t(key); }
    }
    var phs = scope.querySelectorAll("[data-i18n-placeholder]");
    for (var j = 0; j < phs.length; j++) {
      var pkey = phs[j].getAttribute("data-i18n-placeholder");
      if (pkey) { phs[j].setAttribute("placeholder", t(pkey)); }
    }
    // 同步 <html lang>，让屏幕阅读器与浏览器拼写检查用对语言
    try { document.documentElement.lang = (current === "zh-CN") ? "zh-CN" : "en"; } catch (e) { /* 忽略 */ }
  }

  I18N.detect = detect;
  I18N.t = t;
  I18N.apply = apply;
  I18N.locale = function () { return current; };
  I18N.has = function (loc) { return !!dict[loc]; };

  // 便捷全局别名：界面代码里大量使用，短名字更清爽
  window.t = t;
})();
