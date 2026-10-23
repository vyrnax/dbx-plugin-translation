// 从源码直接构造 .dbxp（zip）。
//
// 必须复刻官方 `dbx-plugin package` 的三条硬规则，否则宿主在安装阶段就直接拒绝
// （宿主源码 crates/dbx-plugin-runtime/src/plugins/installer.rs 与 manifest.rs）：
//
//   1) manifest.entrypoints.backend.executable 必须指向包内【真实存在】的文件。
//      官方打包器会把它重写成 bin/<target>/<binary>[.exe]（target 形如 windows-x64，
//      **只有 windows 目标才带 .exe** —— 官方 CLI 的 executable_name() 就是这么定的）。
//      宿主解析时不会自动补 ".exe"，也不会做 target 目录替换 —— 名字对不上就是
//      "Plugin backend executable does not exist" → 整个包判为 incompatible。
//
//   2) 必须包含 checksums.json（algorithm 必须是 "sha256"），且 files 必须
//      【精确等于】包内除 checksums.json / signature.json 之外的每一个文件。
//      少一个多一个都是 "Plugin checksums do not cover the package exactly"。
//
//   3) signature.json 可省，但只能在插件中心开启「允许安装未签名开发包」后安装。
//
//   4) zip 必须写 Unix 权限位（极易漏）：
//      宿主安装器在 Unix 上执行 `if let Some(mode) = entry.unix_mode() { set_permissions(...) }`，
//      而 zip crate 的 unix_mode() 在 external_attributes == 0 时返回 None
//      → 宿主跳过设权限 → 解出来的文件是默认 0o644
//      → **侧车没有可执行位，macOS/Linux 上起不来**（Windows 上测不出来）。
//
// 前三条里 1 和 2 任何一条不满足，安装会直接失败，表现为「装完之后什么也存不了」。
//
// 用法：
//   node _buildpkg.js                            # 默认 windows-x64
//   node _buildpkg.js --target linux-x64         # 用 _xbuild/ 下的交叉编译产物
//   node _buildpkg.js --target darwin-arm64 --exe <path>
//
// 关于多平台：官方 CLI（dbx-plugin package）**拒绝**为「非当前宿主」的 target 打包，
// 官方推荐在 CI 上按平台矩阵分别构建。但本插件的侧车是**纯 Go、无 cgo**，
// 可以直接交叉编译（见 _release.mjs），因此本地也能产出其它平台的合法包。
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const zlib = require("zlib");

const root = __dirname + path.sep;
const binary = "dbx-plugin-translation";   // 与 dbx-plugin.toml [backend].binary 一致

const argv = process.argv.slice(2);
function argValue(name) {
  const i = argv.indexOf(name);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : "";
}
const target = argValue("--target") || "windows-x64";   // current_target(): {os}-{arch}
if (!/^[a-z0-9-]{1,64}$/.test(target)) {
  console.error(`\n[FATAL] target 含非法字符：${target}（只允许小写字母/数字/-，与官方 validate_artifact_target 一致）\n`);
  process.exit(1);
}

// 目标 → Go 的 GOOS/GOARCH
const GO_TRIPLES = {
  "windows-x64": ["windows", "amd64"],
  "windows-arm64": ["windows", "arm64"],
  "darwin-arm64": ["darwin", "arm64"],
  "darwin-x64": ["darwin", "amd64"],
  "linux-x64": ["linux", "amd64"],
  "linux-arm64": ["linux", "arm64"],
};

function resolveExe() {
  const explicit = argValue("--exe");
  if (explicit) return explicit;
  if (target === "windows-x64") return root + `backend\\${binary}.exe`;
  const t = GO_TRIPLES[target];
  if (!t) {
    console.error(`\n[FATAL] 未知 target：${target}。已知：${Object.keys(GO_TRIPLES).join(", ")}\n`);
    process.exit(1);
  }
  return root + `_xbuild\\${binary}-${t[0]}-${t[1]}`;
}

const mani = JSON.parse(fs.readFileSync(root + "manifest.json", "utf8"));
const ver = mani.version || "0.0.0";
// 只有 windows 目标带 .exe（对齐官方 executable_name()）
const exeRel = `bin/${target}/${binary}${target.startsWith("windows") ? ".exe" : ""}`;
const exePath = resolveExe();
const out = root + `dist\\${mani.id}-${ver}-${target}.dbxp`;

if (!fs.existsSync(exePath)) {
  console.error(`\n[FATAL] 找不到 target=${target} 的侧车二进制：${exePath}`);
  console.error(`        先构建：CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build -C backend -o ../_xbuild/${binary}-<os>-<arch> .`);
  console.error(`        （或直接跑 node _release.mjs 一次产出全部平台）\n`);
  process.exit(1);
}

// ---- 1) 重写 manifest，使 executable 与包内真实路径一致 ----
mani.entrypoints = mani.entrypoints || {};
mani.entrypoints.backend = Object.assign({}, mani.entrypoints.backend, { executable: exeRel });
const manifestBytes = Buffer.from(JSON.stringify(mani, null, 2) + "\n", "utf8");

/* ==========================================================================
 * 打包前闸门：这些检查每一条都对应一个「装上了但不工作」的历史事故。
 * 放在打包阶段而不是测试阶段，是因为一旦漏出去，用户看到的现象
 * （白屏 / 存不上）与真实原因（DOM id 拼错）之间隔着好几层，极难排查。
 * ========================================================================== */

// ---- 1.5a) 前端 DOM 自检 ----
const htmlRaw = fs.readFileSync(root + "ui/index.html", "utf8");
const htmlLive = htmlRaw.replace(/<!--[\s\S]*?-->/g, "");   // 去掉注释后的真实 DOM
const liveIds = new Set([...htmlLive.matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]));

const jsFiles = ["app.js", "engines.js", "history.js", "storage.js"].map((n) => ({
  name: n,
  raw: fs.readFileSync(root + "ui/" + n, "utf8"),
}));
// 扫描前去掉注释：注释里会引用历史错误代码（例如 `$("x").onclick = ...`），不能当真
jsFiles.forEach((f) => {
  f.code = f.raw.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^[ \t]*\/\/.*$/gm, "");
});

// (a) `$("x").` 悬空引用：对 null 取属性 = TypeError = 启动连坐死
const dangling = [];
for (const f of jsFiles) {
  for (const m of f.code.matchAll(/\$\("([^"]+)"\)\s*\./g)) {
    if (!liveIds.has(m[1])) dangling.push(`${f.name}: #${m[1]}`);
  }
}
if (dangling.length) {
  console.error("\n[FATAL] 对不存在的 DOM 元素做了属性访问（会导致启动中断）：");
  dangling.forEach((d) => console.error("   - " + d));
  console.error("   修复：改用安全绑定 click(id, fn) / on(id, ev, fn)，或把元素从 HTML 注释里放出来。\n");
  process.exit(1);
}

// (b) 同名函数声明：后声明的会静默覆盖前面的，函数名撞车不会有任何提示
const dupFns = [];
for (const f of jsFiles) {
  const seen = {};
  f.code.split(/\r?\n/).forEach((l, i) => {
    const m = /^  function (\w+)\s*\(/.exec(l);
    if (m) (seen[m[1]] = seen[m[1]] || []).push(i + 1);
  });
  Object.keys(seen).filter((k) => seen[k].length > 1)
    .forEach((k) => dupFns.push(`${f.name}: ${k} (行 ${seen[k].join(", ")})`));
}
if (dupFns.length) {
  console.error("\n[FATAL] 存在重复的函数声明（后者会覆盖前者，必须改名）：");
  dupFns.forEach((d) => console.error("   - " + d));
  console.error("");
  process.exit(1);
}

// (c) innerHTML 与 localStorage：前者是 XSS 风险，后者在沙箱里直接抛异常
for (const f of jsFiles) {
  if (/\.innerHTML\s*=/.test(f.code)) {
    console.error(`\n[FATAL] ${f.name} 使用了 innerHTML 赋值（XSS 风险，改用 textContent）\n`);
    process.exit(1);
  }
  if (/\blocalStorage\b|\bsessionStorage\b/.test(f.code)) {
    console.error(`\n[FATAL] ${f.name} 引用了 localStorage/sessionStorage（opaque origin 沙箱会抛 SecurityError）\n`);
    process.exit(1);
  }
}

// (d) 关键 UI 节点必须在 index.html 里真实存在
const CRITICAL = [
  "app", "topbar", "main", "statusbar", "toast",
  "btn-view-translate", "btn-view-engines", "btn-view-history", "btn-view-settings",
  "view-translate", "view-engines", "view-history", "view-settings",
  "sel-source-lang", "sel-target-lang", "sel-engine",
  "src-text", "dst-text", "dst-wrap", "dst-empty", "dst-cursor",
  "btn-translate", "btn-cancel", "btn-copy-dst", "translate-hint", "no-engine",
  "engine-list", "engine-empty", "btn-add-engine",
  "engine-modal", "engine-kind", "engine-fields", "engine-form-error",
  "btn-engine-save", "btn-engine-test", "btn-engine-cancel", "btn-engine-delete",
  "history-list", "history-empty", "history-search", "btn-clear-history",
  "btn-save-settings", "settings-status",
  // 外观只读展示（跟随宿主，无手动覆盖控件）
  "info-theme", "info-locale", "info-font", "info-token-count",
  "banner-not-persistent", "banner-insecure-secret", "btn-banner-retry",
  "more-menu", "btn-more", "btn-export", "btn-import", "btn-diag",
  "confirm-modal", "confirm-title", "confirm-lines", "btn-confirm-yes", "btn-confirm-no",
  "diag-modal", "diag-content", "btn-diag-copy",
  "file-input", "engine-badge", "status-dot", "status-text",
  "chk-instant", "chk-stream", "info-datadir", "info-secret",
];
const missingCritical = CRITICAL.filter((id) => !liveIds.has(id));
if (missingCritical.length) {
  console.error("\n[FATAL] index.html 缺少关键元素：" + missingCritical.join(", ") + "\n");
  process.exit(1);
}

// (e) 功能开关必须与 HTML 一致
const appCode = jsFiles.find((f) => f.name === "app.js").code;
const showDiagBar = /var\s+SHOW_DIAG_BAR\s*=\s*(true|false)/.exec(appCode);
if (!showDiagBar) {
  console.error("\n[FATAL] app.js 缺少 SHOW_DIAG_BAR 开关（置顶诊断条的状态无法自证）。\n");
  process.exit(1);
}
if (showDiagBar[1] === "true" && !liveIds.has("diag-bar")) {
  console.error("\n[FATAL] SHOW_DIAG_BAR=true 但 index.html 里 #diag-bar 不存在。\n");
  process.exit(1);
}
if (showDiagBar[1] === "false" && liveIds.has("diag-bar")) {
  console.error("\n[FATAL] SHOW_DIAG_BAR=false 但 index.html 里 #diag-bar 仍存在：上线不该出现。\n");
  process.exit(1);
}
const enableImport = /var\s+ENABLE_IMPORT\s*=\s*(true|false)/.exec(appCode);
if (!enableImport) {
  console.error("\n[FATAL] app.js 缺少 ENABLE_IMPORT 开关。\n");
  process.exit(1);
}
const importLive = liveIds.has("btn-import") && liveIds.has("file-input");
if (enableImport[1] === "false" && importLive) {
  console.error("\n[FATAL] ENABLE_IMPORT=false 但 #btn-import / #file-input 仍存在。\n");
  process.exit(1);
}
if (enableImport[1] === "true" && !importLive) {
  console.error("\n[FATAL] ENABLE_IMPORT=true 但 #btn-import / #file-input 缺失。\n");
  process.exit(1);
}

// ---- 2) 权限闸门 ----
// host.ai 只在 >=0.6.20 的宿主上存在，而 permissions 是静态的 ——
// 声明它会让老宿主在安装阶段直接拒绝整个包。固化成闸门，防止日后被顺手加回。
const perms = mani.permissions || [];
if (perms.indexOf("host.ai") >= 0) {
  console.error("\n[FATAL] manifest 声明了 host.ai：本插件不依赖宿主内置 AI。");
  console.error("        它会把最低宿主抬到 0.6.20，且旧宿主会在安装阶段直接拒绝整个包。\n");
  process.exit(1);
}
// host.network 是静态白名单（最多 8 个 origin、不允许通配符），
// 无法满足「用户自填任意端点」；网络请求全部由侧车发出，不受 CSP 约束。
if (perms.some((p) => String(p).startsWith("host.network"))) {
  console.error("\n[FATAL] manifest 声明了 host.network:*：本插件的网络请求全部走侧车。");
  console.error("        该权限是静态白名单（最多 8 个 origin、不允许通配符），");
  console.error("        无法满足用户自填端点的需求，声明它只会白白抬高安装门槛。\n");
  process.exit(1);
}
// host.events 是流式翻译的硬依赖：侧车事件只有在声明该权限时才会被转发到前端
if (perms.indexOf("host.events") < 0) {
  console.error("\n[FATAL] manifest 未声明 host.events：流式翻译的事件无法到达前端。");
  console.error("        dev-runtime: sidecar.on(\"event\") 仅在 manifest 声明该权限时才 broadcast。\n");
  process.exit(1);
}

// ---- 2.5) manifest 字段白名单闸门（对齐官方 manifest.schema.json）----
//
// 为什么需要这道闸门：Manifest v1 的所有对象都是 `additionalProperties: false`，
// 多写一个字段宿主就会在【安装阶段】直接拒绝整个包，报错形如：
//
//   Failed to parse plugin manifest: unknown field `label`,
//   expected `id` or `items` at line 68 column 3
//
// 这类错误在打包阶段完全看不出来（zip 打得出来、checksums 也算得对），
// 只有装的时候才炸，而且报错只指出第一个问题 —— 修一个再装一次，
// 逐个试错非常费时。因此在打包前一次性校验全部字段。
//
// 白名单直接抄自官方 schema 的 $defs（各 *Contribution 的 properties 键），
// 升级 DBX 后应重新比对一次 https://raw.githubusercontent.com/t8y2/dbx/main/plugins/manifest.schema.json
const MANIFEST_FIELDS = {
  top: ["$schema", "manifest_version", "id", "name", "icon", "version", "publisher",
        "description", "source", "homepage", "engines", "permissions", "entrypoints",
        "capabilities", "contributions", "localizations"],
  engines: ["dbx", "host_api"],
  entrypoints: ["backend", "ui"],
  backendEntrypoint: ["protocol_versions", "transport", "executable"],
  uiEntrypoint: ["root", "entry"],
  manifestLocalization: ["name", "description", "contributions"],
  contributionLocalization: ["label", "description", "fields", "actions"],
  contributions: {
    "connection-provider": ["type", "id", "label", "icon", "database_type", "description",
                            "fields", "workbench", "filesystem_provider", "capabilities",
                            "proxy_route", "actions"],
    "workbench":            ["type", "id", "label", "description", "icon", "ai"],
    "filesystem-provider":  ["type", "id", "label", "description", "icon", "schemes",
                            "root_uri", "capabilities"],
    "context-menu":         ["type", "id", "label", "description", "icon", "menu",
                            "dynamic", "action"],
    "result-view":          ["type", "id", "label", "description", "icon"],
    "command":              ["type", "id", "label", "description", "icon", "action", "enablement"],
    // menus 只有 type/id/items —— 加 label 会被宿主拒装（本项目踩过这个坑）
    "menus":                ["type", "id", "items"],
    "mcp":                  ["type", "id", "description", "ai_tools", "external_tools"],
  },
  menuItem: ["location", "command", "group", "order", "default_visible", "when"],
  openWorkbenchAction: ["type", "workbench", "presentation", "instance_key"],
};

// 权限白名单：官方 schema 用 enum + host.network 正则两种形态
const PERMISSION_ENUM = [
  "host.events", "host.binary", "host.workbench", "host.filesystem",
  "host.plans:read", "host.schema:read", "host.storage", "host.ai",
  "host.clipboard:read", "host.data:read", "host.connections:read",
  "host.connections::read",
];
// 下面几条 pattern 都含 "/" 或 "//"。
// 【不能用正则字面量】：JS 的正则字面量以 / 包裹，内部的裸 / 会提前终止字面量，
// 导致 "Unterminated group" 这类语法错误（本项目踩过）。
// 统一改用 new RegExp(字符串) —— 字符串里只需按普通字符串规则转义一次。
const PERMISSION_NETWORK_RE = new RegExp("^host\\.network:https://[A-Za-z0-9._-]+(:[0-9]+)?$");
const IDENTIFIER_RE = new RegExp("^[a-z0-9][a-z0-9._-]*$");
const VERSION_RE = new RegExp("^[0-9]+\\.[0-9]+\\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$");
// assetPath：不能以 / 开头、不能含反斜杠、不能含 .. 或 //
const ASSET_PATH_RE = new RegExp("^(?!/)(?!.*\\\\)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*//).+$");
const LOCALE_RE = new RegExp("^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$");
const URL_RE = new RegExp("^https?://[^\\s]+$");

const schemaErrors = [];
function rejectUnknown(obj, allowed, where) {
  for (const k of Object.keys(obj || {})) {
    if (!allowed.includes(k)) {
      schemaErrors.push(`${where}: 含未声明字段 "${k}"（只允许 ${allowed.join(", ")}）`);
    }
  }
}
function requireFields(obj, required, where) {
  for (const k of required) {
    if (obj == null || obj[k] === undefined) schemaErrors.push(`${where}: 缺少必填字段 "${k}"`);
  }
}
function checkPattern(value, re, where) {
  if (typeof value === "string" && !re.test(value)) {
    schemaErrors.push(`${where}: "${value}" 不符合格式要求 ${re}`);
  }
}

(function validateManifest() {
  rejectUnknown(mani, MANIFEST_FIELDS.top, "manifest");
  requireFields(mani, ["manifest_version", "id", "name", "version", "publisher", "engines"], "manifest");
  if (mani.manifest_version !== 1) schemaErrors.push("manifest_version 必须为数字 1");
  checkPattern(mani.id, IDENTIFIER_RE, "manifest.id");
  checkPattern(mani.version, VERSION_RE, "manifest.version");
  for (const k of ["source", "homepage"]) {
    if (mani[k] !== undefined) checkPattern(mani[k], URL_RE, `manifest.${k}`);
  }
  if (mani.icon !== undefined) checkPattern(mani.icon, ASSET_PATH_RE, "manifest.icon");

  rejectUnknown(mani.engines, MANIFEST_FIELDS.engines, "engines");
  requireFields(mani.engines, ["host_api"], "engines");

  for (const perm of mani.permissions || []) {
    if (!PERMISSION_ENUM.includes(perm) && !PERMISSION_NETWORK_RE.test(perm)) {
      schemaErrors.push(`permissions: 非法权限 "${perm}"`);
    }
  }

  const ep = mani.entrypoints || {};
  rejectUnknown(ep, MANIFEST_FIELDS.entrypoints, "entrypoints");
  if (ep.backend) {
    rejectUnknown(ep.backend, MANIFEST_FIELDS.backendEntrypoint, "entrypoints.backend");
    requireFields(ep.backend, ["executable"], "entrypoints.backend");
    // executable 在打包时会被重写为 bin/<target>/...，此处只校验格式合法性
    checkPattern(ep.backend.executable, ASSET_PATH_RE, "entrypoints.backend.executable");
  }
  if (ep.ui) {
    rejectUnknown(ep.ui, MANIFEST_FIELDS.uiEntrypoint, "entrypoints.ui");
    requireFields(ep.ui, ["entry"], "entrypoints.ui");
    checkPattern(ep.ui.entry, ASSET_PATH_RE, "entrypoints.ui.entry");
  }

  for (const [i, c] of (mani.contributions || []).entries()) {
    const where = `contributions[${i}](${c.type})`;
    const allowed = MANIFEST_FIELDS.contributions[c.type];
    if (!allowed) { schemaErrors.push(`${where}: 未知贡献点类型 "${c.type}"`); continue; }
    rejectUnknown(c, allowed, where);
    checkPattern(c.id, IDENTIFIER_RE, `${where}.id`);
    if (c.icon !== undefined) checkPattern(c.icon, ASSET_PATH_RE, `${where}.icon`);
    // 各类型的必填字段
    const requiredByType = {
      "connection-provider": ["type", "id", "database_type", "fields"],
      "workbench": ["type", "id", "label"],
      "filesystem-provider": ["type", "id", "label", "schemes"],
      "context-menu": ["type", "id", "label", "menu"],
      "result-view": ["type", "id", "label"],
      "command": ["type", "id", "label", "action"],
      "menus": ["type", "id"],
      "mcp": ["type", "id"],
    };
    requireFields(c, requiredByType[c.type], where);

    if (c.type === "menus") {
      (c.items || []).forEach((it, j) => {
        const w2 = `${where}.items[${j}]`;
        rejectUnknown(it, MANIFEST_FIELDS.menuItem, w2);
        requireFields(it, ["location", "command", "group", "order"], w2);
        if (!["commandPalette", "appToolbar", "appSidebar"].includes(it.location)) {
          schemaErrors.push(`${w2}: location "${it.location}" 非法`);
        }
      });
    }
    if (c.type === "command" && c.action) {
      rejectUnknown(c.action, MANIFEST_FIELDS.openWorkbenchAction, `${where}.action`);
      requireFields(c.action, ["type", "workbench"], `${where}.action`);
      if (c.action.presentation !== undefined &&
          !["tab", "panel"].includes(c.action.presentation)) {
        schemaErrors.push(`${where}.action: presentation "${c.action.presentation}" 非法（只允许 tab/panel）`);
      }
    }
  }

  const contribIds = new Set((mani.contributions || []).map((c) => c.id));
  for (const [locale, body] of Object.entries(mani.localizations || {})) {
    if (!LOCALE_RE.test(locale)) schemaErrors.push(`localizations: 键 "${locale}" 不是合法 locale`);
    rejectUnknown(body, MANIFEST_FIELDS.manifestLocalization, `localizations.${locale}`);
    for (const [cid, cbody] of Object.entries(body.contributions || {})) {
      if (!contribIds.has(cid)) {
        schemaErrors.push(`localizations.${locale}.contributions: 含不存在的贡献点 id "${cid}"`);
      }
      rejectUnknown(cbody, MANIFEST_FIELDS.contributionLocalization,
        `localizations.${locale}.contributions.${cid}`);
    }
  }
})();

if (schemaErrors.length) {
  console.error("\n[FATAL] manifest.json 不符合官方 schema（宿主会在安装阶段直接拒绝整个包）：");
  schemaErrors.forEach((e) => console.error("   - " + e));
  console.error("\n   参考：https://raw.githubusercontent.com/t8y2/dbx/main/plugins/manifest.schema.json");
  console.error("   注意 Manifest v1 的所有对象都是 additionalProperties:false，多一个字段就装不上。\n");
  process.exit(1);
}
console.log("  manifest schema 校验通过（字段白名单 / 必填项 / 格式 / 权限枚举 / locale 键）");

// ---- 3) 贡献点闸门 ----
const contribs = mani.contributions || [];
const wbIds = new Set(contribs.filter((c) => c.type === "workbench").map((c) => c.id));
const cmdIds = new Set(contribs.filter((c) => c.type === "command").map((c) => c.id));
// command / menus 是 DBX 0.6.21 才引入；声明它必须把最低宿主抬到 0.6.21，
// 否则老宿主会在安装阶段直接拒绝整个包。
if (contribs.some((c) => c.type === "command" || c.type === "menus")) {
  const eng = String((mani.engines && mani.engines.dbx) || "");
  if (!/>=\s*0\.6\.21/.test(eng)) {
    console.error(`\n[FATAL] manifest 声明了 command/menus，但 engines.dbx 仍是「${eng}」，必须 >=0.6.21（否则老宿主拒装）。\n`);
    process.exit(1);
  }
}
for (const c of contribs) {
  if (c.type === "command") {
    const action = c.action || {};
    if (action.type !== "open-workbench" || !wbIds.has(action.workbench)) {
      console.error(`\n[FATAL] command 贡献点 ${c.id} 的 action 必须引用已声明的 workbench（当前：${[...wbIds].join(", ")}）。\n`);
      process.exit(1);
    }
  }
  if (c.type === "menus") {
    (c.items || []).forEach((it, i) => {
      if (!cmdIds.has(it.command)) {
        console.error(`\n[FATAL] menus 贡献点 ${c.id} 的第 ${i + 1} 项引用的 command「${it.command}」未声明。\n`);
        process.exit(1);
      }
      if (["commandPalette", "appToolbar", "appSidebar"].indexOf(it.location) < 0) {
        console.error(`\n[FATAL] menus 贡献点 ${c.id} 的第 ${i + 1} 项 location 必须是 commandPalette/appToolbar/appSidebar。\n`);
        process.exit(1);
      }
    });
  }
}
// localizations 的键必须对上真实贡献点 id，否则宿主会把整个清单判为无效
const loc = (mani.localizations && mani.localizations.en && mani.localizations.en.contributions) || null;
if (loc) {
  const ids = new Set(contribs.map((c) => c.id));
  const bad = Object.keys(loc).filter((k) => !ids.has(k));
  if (bad.length) {
    console.error("\n[FATAL] localizations.en.contributions 含不存在的贡献点 id：" + bad.join(", "));
    console.error("        宿主会把对不上的键判为无效清单。\n");
    process.exit(1);
  }
  // 只对【有可本地化文案】的贡献点要求覆盖。
  // menus 贡献点的 schema 只允许 type/id/items —— 它本身没有 label/description，
  // 因此不存在需要本地化的文案，不该因为它没出现在 localizations 里而告警。
  const localizableIds = new Set(
    contribs.filter((c) => c.label !== undefined || c.description !== undefined).map((c) => c.id)
  );
  const uncovered = [...localizableIds].filter((id) => !loc[id]);
  if (uncovered.length) {
    console.error("\n[WARN] 以下有文案的贡献点没有英文本地化：" + uncovered.join(", ") + "\n");
  }
  // 反向检查：localizations 里不该有「对应贡献点无可本地化文案」的死条目
  const deadLoc = Object.keys(loc).filter((id) => ids.has(id) && !localizableIds.has(id));
  if (deadLoc.length) {
    console.error("\n[FATAL] localizations 含死条目（对应贡献点没有 label/description 可本地化）：" +
      deadLoc.join(", "));
    console.error("        menus 贡献点的 schema 只允许 type/id/items，为它本地化 label 是无效配置。\n");
    process.exit(1);
  }
}

// ---- 4) .dbx-store.json 一致性（商店会比对）----
try {
  const pub = JSON.parse(fs.readFileSync(root + ".dbx-store.json", "utf8"));
  if (JSON.stringify(pub.permissions) !== JSON.stringify(perms)) {
    console.error("\n[FATAL] .dbx-store.json 的 permissions 与 manifest 不一致（商店会拒绝这个包）：");
    console.error("        manifest   : " + JSON.stringify(perms));
    console.error("        .dbx-store : " + JSON.stringify(pub.permissions) + "\n");
    process.exit(1);
  }
  if (pub.name !== mani.name) {
    console.error("\n[FATAL] .dbx-store.json 的 name 与 manifest 不一致：");
    console.error("        manifest   : " + mani.name);
    console.error("        .dbx-store : " + pub.name + "\n");
    process.exit(1);
  }
} catch (e) {
  console.warn("\n[WARN] 读不到 .dbx-store.json（跳过一致性检查）：" + e.message + "\n");
}

// ---- 5) 版本号三处必须一致 ----
// 不一致会导致「装的是新版、跑的是旧代码」这种极难排查的问题。
const uiVer = (jsFiles.find((f) => f.name === "storage.js").raw.match(/var UI_VERSION = "([^"]+)"/) || [])[1];
if (uiVer !== ver) {
  console.error(`\n[FATAL] ui/storage.js UI_VERSION=${uiVer} 与 manifest.version=${ver} 不一致。\n`);
  process.exit(1);
}
const goMain = fs.readFileSync(root + "backend/main.go", "utf8");
const goVer = (goMain.match(/pluginVersion\s*=\s*"([^"]+)"/) || [])[1];
if (goVer !== ver) {
  console.error(`\n[FATAL] backend/main.go pluginVersion=${goVer} 与 manifest.version=${ver} 不一致。\n`);
  process.exit(1);
}
const goId = (goMain.match(/pluginID\s*=\s*"([^"]+)"/) || [])[1];
if (goId !== mani.id) {
  console.error(`\n[FATAL] backend/main.go pluginID=${goId} 与 manifest.id=${mani.id} 不一致。\n`);
  process.exit(1);
}

// ---- 6) 发布前提醒：别再拿占位身份发出去 ----
if (!mani.publisher || mani.publisher === "example" || /^com\.example\./.test(mani.id)) {
  console.warn(`\n[WARN] manifest 的 id / publisher 仍是占位值（${mani.id} / ${mani.publisher}）：正式发布前请改成自己的。\n`);
}

console.log("  闸门通过：DOM 0 处悬空 / 无重复函数 / 无 innerHTML 与 localStorage / " +
  `关键元素 ${CRITICAL.length} 项齐备 / 权限最小 / 三处版本号一致 = ${ver}`);

// ---- 7) 收集包内文件（跳过 _ 前缀临时文件与隐藏文件）----
const files = [
  ["manifest.json", manifestBytes],
  [exeRel, fs.readFileSync(exePath)],
];
for (const dir of ["assets", "ui"]) {
  (function walk(d, rel) {
    for (const e of fs.readdirSync(d, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      if (e.name.startsWith("_") || e.name.startsWith(".")) continue;
      const fp = path.join(d, e.name);
      if (e.isDirectory()) walk(fp, rel + "/" + e.name);
      else files.push([rel + "/" + e.name, fs.readFileSync(fp)]);
    }
  })(root + dir, dir);
}

// ---- 8) checksums.json 必须精确覆盖除自身与 signature.json 外的全部文件 ----
const checksums = { algorithm: "sha256", files: {} };
for (const [name, data] of files) {
  checksums.files[name] = crypto.createHash("sha256").update(data).digest("hex");
}
files.push(["checksums.json", Buffer.from(JSON.stringify(checksums, null, 2) + "\n", "utf8")]);

// ---- 9) 写出 zip（deflate）----
function crc32(buf) {
  let c = ~0;
  for (let i = 0; i < buf.length; i++) {
    c ^= buf[i];
    for (let k = 0; k < 8; k++) c = (c >>> 1) ^ (0xedb88320 & -(c & 1));
  }
  return (~c) >>> 0;
}

const local = [];
const central = [];
for (const [name, data] of files) {
  const nameBuf = Buffer.from(name, "utf8");
  const comp = zlib.deflateRawSync(data, { level: 9 });
  const crc = crc32(data);
  const off = Buffer.concat(local).length;

  // 本地文件头
  const lh = Buffer.alloc(30);
  lh.writeUInt32LE(0x04034b50, 0);
  lh.writeUInt16LE(20, 4);       // version needed
  lh.writeUInt16LE(0, 6);        // flags
  lh.writeUInt16LE(8, 8);        // method: deflate
  lh.writeUInt16LE(0, 10);       // mod time
  lh.writeUInt16LE(0, 12);       // mod date
  lh.writeUInt32LE(crc, 14);
  lh.writeUInt32LE(comp.length, 18);
  lh.writeUInt32LE(data.length, 22);
  lh.writeUInt16LE(nameBuf.length, 26);
  lh.writeUInt16LE(0, 28);       // extra length
  local.push(lh, nameBuf, comp);

  // 中央目录头 —— Unix 权限位写在这里
  //
  // 宿主安装器（installer.rs）在 Unix 上执行：
  //     if let Some(mode) = entry.unix_mode() { set_permissions(from_mode(mode & 0o777)) }
  // 而 zip crate 的 unix_mode() 在 `external_attributes == 0` 时直接返回 None
  //   → 宿主跳过设权限 → 解出来的文件是默认 0o644 → **侧车没有可执行位，无法启动**。
  // 官方打包器给 bin/<target>/ 下的文件 0o755、其余 0o644；这里照做。
  const isExe = name === exeRel;
  const unixMode = isExe ? 0o100755 : 0o100644;   // S_IFREG | 权限（与 zip crate 的写法一致）

  const ch = Buffer.alloc(46);
  ch.writeUInt32LE(0x02014b50, 0);
  ch.writeUInt16LE((3 << 8) | 20, 4);  // version made by：高字节 3 = Unix
  ch.writeUInt16LE(20, 6);             // version needed
  ch.writeUInt16LE(0, 8);              // flags
  ch.writeUInt16LE(8, 10);             // method: deflate
  ch.writeUInt16LE(0, 12);             // mod time
  ch.writeUInt16LE(0, 14);             // mod date
  ch.writeUInt32LE(crc, 16);
  ch.writeUInt32LE(comp.length, 20);
  ch.writeUInt32LE(data.length, 24);
  ch.writeUInt16LE(nameBuf.length, 28);
  ch.writeUInt16LE(0, 30);             // extra length
  ch.writeUInt16LE(0, 32);             // comment length
  ch.writeUInt16LE(0, 34);             // disk number start
  ch.writeUInt16LE(0, 36);             // internal attrs
  // 注意：JS 的 `<<` 是 32 位【有符号】运算，0o100644 << 16 会溢出成负数 —— 必须用乘法。
  ch.writeUInt32LE((unixMode * 0x10000) >>> 0, 38);
  ch.writeUInt32LE(off, 42);
  central.push(Buffer.concat([ch, nameBuf]));
}

const body = Buffer.concat(local);
const cd = Buffer.concat(central);
const eo = Buffer.alloc(22);
eo.writeUInt32LE(0x06054b50, 0);
eo.writeUInt16LE(0, 4);            // disk number
eo.writeUInt16LE(0, 6);            // disk with CD
eo.writeUInt16LE(files.length, 8);
eo.writeUInt16LE(files.length, 10);
eo.writeUInt32LE(cd.length, 12);
eo.writeUInt32LE(body.length, 16);
eo.writeUInt16LE(0, 20);           // comment length

fs.mkdirSync(root + "dist", { recursive: true });
const pkg = Buffer.concat([body, cd, eo]);
fs.writeFileSync(out, pkg);

// 顺带产出官方同名的 artifact.json
fs.writeFileSync(out.replace(/\.dbxp$/, ".artifact.json"), JSON.stringify({
  target,
  url: path.basename(out),
  sha256: crypto.createHash("sha256").update(pkg).digest("hex"),
  size: pkg.length,
}, null, 2) + "\n");

console.log("built " + out);
console.log("  target=" + target + "  size=" + pkg.length + "  entries=" + files.length);
console.log("  executable=" + exeRel + "  version=" + ver);
