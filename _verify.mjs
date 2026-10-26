// 校验 .dbxp：条目、manifest.executable、checksums 精确覆盖 + sha256、
// Unix 权限位、前端静态约束，以及【真跑一次包内的侧车】。
//
// 用法: node _verify.mjs [某个.dbxp]
//   不给参数时自动取 dist/ 下最新的那个，并拿它和源码 manifest.json 的版本比对
//   （防止「验的是旧包」这种假绿 —— 这是最容易自欺欺人的一类错误）。
//
// 为什么要「真跑」而不是 grep 字符串：
//   grep 只能证明「源码里有这个字符串」，证明不了「包里的二进制真的具备这个能力」。
//   交叉编译产物尤其危险：字节正确不代表行为正确。
//   把包内的 exe 解出来、用 JSON-RPC 真问它几条，是唯一可靠的证明。
//   但只能对【本机平台】的包执行 —— darwin/linux 二进制在 Windows 上跑不了，
//   那种情况标记为 SKIP 并注明「需在该平台真机冒烟」。
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import zlib from "node:zlib";
import { fileURLToPath } from "node:url";
import { removeTree, runProcess } from "./_testutil.mjs";

// 用 fileURLToPath 而非手工替换 URL pathname：
// 手工正则在 Linux 上碰巧能工作（pathname 本来就是 /home/...），
// 但在 Windows 上要靠正则去掉前导斜杠，且遇到含空格或百分号编码的路径就会出错。
// fileURLToPath 是标准做法，两个平台都正确。
const ROOT = path.dirname(fileURLToPath(import.meta.url));
const BINARY = "dbx-plugin-translation";
const srcMani = JSON.parse(fs.readFileSync(path.join(ROOT, "manifest.json"), "utf8"));
const srcVer = srcMani.version;

const f = process.argv[2] || (() => {
  const dir = path.join(ROOT, "dist");
  if (!fs.existsSync(dir)) { console.error("dist/ 不存在，请先构建"); process.exit(2); }
  const cands = fs.readdirSync(dir).filter((n) => n.endsWith(".dbxp"))
    .map((n) => ({ n, t: fs.statSync(path.join(dir, n)).mtimeMs }))
    .sort((a, b) => b.t - a.t);
  if (!cands.length) { console.error("dist/ 下没有 .dbxp"); process.exit(2); }
  return path.join(dir, cands[0].n);
})();

console.log("校验包: " + f);
console.log("源码 manifest.version = " + srcVer + "\n");
const buf = fs.readFileSync(f);

/* ---------------- zip 解析 ---------------- */
function entries(b) {
  const out = [];
  const off = b.length - 22;
  if (b.readUInt32LE(off) !== 0x06054b50) throw new Error("not a zip");
  const count = b.readUInt16LE(off + 10);
  let p = b.readUInt32LE(off + 16);
  for (let i = 0; i < count; i++) {
    const nameLen = b.readUInt16LE(p + 28);
    const extraLen = b.readUInt16LE(p + 30);
    const commentLen = b.readUInt16LE(p + 32);
    out.push({
      name: b.slice(p + 46, p + 46 + nameLen).toString("utf8"),
      method: b.readUInt16LE(p + 10),
      csize: b.readUInt32LE(p + 20),
      usize: b.readUInt32LE(p + 24),
      lho: b.readUInt32LE(p + 42),
      madeBy: b.readUInt16LE(p + 4),   // 高字节 = 制作系统（3 = Unix）
      ext: b.readUInt32LE(p + 38),     // 外部属性：高 16 位 = Unix 权限
    });
    p += 46 + nameLen + extraLen + commentLen;
  }
  return out;
}
function read(e) {
  const nameLen = buf.readUInt16LE(e.lho + 26);
  const extraLen = buf.readUInt16LE(e.lho + 28);
  const start = e.lho + 30 + nameLen + extraLen;
  const raw = buf.slice(start, start + e.csize);
  return e.method === 8 ? zlib.inflateRawSync(raw) : raw;
}

const es = entries(buf);
const has = (n) => es.some((e) => e.name === n);
const get = (n) => { const e = es.find((x) => x.name === n); return e ? read(e) : null; };

console.log("=== 包内条目 (" + es.length + ") ===");
es.forEach((e) => console.log("  " + e.name + "  (" + e.usize + " B)"));

const mani = JSON.parse(get("manifest.json").toString("utf8"));
const exeRel = mani.entrypoints.backend.executable;
const cks = JSON.parse(get("checksums.json").toString("utf8"));
const inPkg = es.map((e) => e.name).filter((n) => n !== "checksums.json" && n !== "signature.json").sort();
const listed = Object.keys(cks.files).sort();
const mismatch = [];
for (const e of es) {
  if (e.name === "checksums.json") continue;
  const h = crypto.createHash("sha256").update(read(e)).digest("hex");
  if (cks.files[e.name] !== h) mismatch.push(e.name);
}

// 从文件名解析 target（<id>-<version>-<target>.dbxp）
const baseName = path.basename(f);
const pkgPrefix = srcMani.id + "-" + srcMani.version + "-";
const pkgTarget = baseName.startsWith(pkgPrefix) && baseName.endsWith(".dbxp")
  ? baseName.slice(pkgPrefix.length, -".dbxp".length) : "";
const exeSuffix = pkgTarget.startsWith("windows") ? ".exe" : "";
const exeExpected = pkgTarget ? `bin/${pkgTarget}/${BINARY}${exeSuffix}` : "";

const HOST_TARGET = (() => {
  const osName = process.platform === "win32" ? "windows" : process.platform;
  const arch = process.arch === "arm64" ? "arm64" : "x64";
  return `${osName}-${arch}`;
})();

/* ---------------- 真跑包内侧车 ---------------- */
async function probePackagedSidecar() {
  const out = {
    ok: false, detail: "", versionOk: false, version: "",
    pingOk: false, pingDetail: "", schemaOk: false, schemaDetail: "",
    prefsOk: false, skip: false, secretLeak: false,
    unknownMethodOk: false, unknownMethodDetail: "",
  };
  if (pkgTarget && pkgTarget !== HOST_TARGET) {
    out.skip = true;
    out.detail = `非本机平台（包=${pkgTarget} 本机=${HOST_TARGET}），无法执行 —— 需在该平台真机冒烟`;
    return out;
  }
  const exeBytes = get(exeRel);
  if (!exeBytes) { out.detail = "包内找不到 " + exeRel; return out; }

  const base = path.join(ROOT, "_verify_tmp", `probe-${process.pid}`);
  const exePath = path.join(base, path.basename(exeRel));
  const dataDir = path.join(base, "data");
  try {
    fs.mkdirSync(dataDir, { recursive: true });
    fs.writeFileSync(exePath, exeBytes);
    if (!exeRel.endsWith(".exe")) fs.chmodSync(exePath, 0o755);

    // 用 JSON Lines 依次问它几条。注意 id 必须能关联回来 ——
    // SDK 对每个请求开一个 goroutine，响应顺序不保证与请求顺序一致。
    const input = [
      JSON.stringify({ jsonrpc: "2.0", id: "1", method: "plugin/initialize", params: { host: { protocolVersions: [1] } } }),
      JSON.stringify({ jsonrpc: "2.0", id: "2", method: "translate/ping", params: {} }),
      JSON.stringify({ jsonrpc: "2.0", id: "3", method: "translate/schema", params: {} }),
      JSON.stringify({ jsonrpc: "2.0", id: "4", method: "prefs/get", params: {} }),
      JSON.stringify({ jsonrpc: "2.0", id: "5", method: "engines/list", params: {} }),
      // 方法名必须是 ASCII：SDK 的 validProtocolName 只接受
      // [A-Za-z0-9] 开头 + [A-Za-z0-9._:/-]，中文名会在 decodeRequest 阶段
      // 就被拒（stderr: "request method is invalid"），根本到不了 Handle，
      // 因此测不出 -32601 这条路径。
      JSON.stringify({ jsonrpc: "2.0", id: "6", method: "nonexistent/method", params: {} }),
    ].join("\n") + "\n";

    // 侧车【必须用异步 spawn】：本机沙箱里 spawnSync 对任何 exe 都 EBUSY。
    // 刚写出来的二进制还可能被杀软短暂锁住，所以带重试。
    const env = { ...process.env, DBX_PLUGIN_DATA_DIR: dataDir };
    const runProbe = () => runProcess(exePath, [], { env, input, timeoutMs: 60000 });
    let r = await runProbe();
    for (let i = 0; i < 4 && r.error && /EBUSY|EPERM|ETXTBSY|EACCES/.test(String(r.error.code || r.error.message)); i++) {
      await new Promise((res) => setTimeout(res, 500));
      r = await runProbe();
    }

    const replies = new Map();
    String(r.out || "").split(/\r?\n/).forEach((line) => {
      if (!line.trim()) return;
      try {
        const m = JSON.parse(line);
        if (m && m.id != null) replies.set(String(m.id), m);
      } catch { /* 非 JSON 行忽略（侧车若往 stdout 写了日志会在这里暴露） */ }
    });

    // stdout 里出现非 JSON 行 = 违反铁律 1（会污染协议流）
    const nonJson = String(r.out || "").split(/\r?\n/)
      .filter((l) => l.trim())
      .filter((l) => { try { JSON.parse(l); return false; } catch { return true; } });
    if (nonJson.length) {
      out.detail = "stdout 含 " + nonJson.length + " 行非 JSON 输出（违反铁律1，会污染协议流）: " + nonJson[0].slice(0, 80);
      return out;
    }

    const init = replies.get("1");
    if (init && init.result && init.result.plugin) {
      out.ok = true;
      out.version = init.result.plugin.version;
      out.versionOk = out.version === mani.version;
      if (init.result.plugin.id !== mani.id) {
        out.detail = "plugin.id 不匹配: " + init.result.plugin.id;
        out.ok = false;
      }
    } else if (init && init.error) {
      out.detail = JSON.stringify(init.error).slice(0, 140);
      return out;
    } else {
      // 把进程本身的错误也报出来：启动失败（EBUSY/超时/权限）与「侧车不回话」是两回事
      const why = r.error ? ("进程启动/执行失败：" + r.error.message) : "没有回应";
      out.detail = `${why}（exit=${r.code} signal=${r.signal || "-"} stderr=${String(r.err || "").slice(0, 140)}）`;
      return out;
    }

    const pg = replies.get("2");
    out.pingOk = !!pg && !pg.error && !!(pg.result && pg.result.ok);
    out.pingDetail = pg
      ? (pg.error ? JSON.stringify(pg.error).slice(0, 100)
        : `v=${pg.result.version} dbOk=${pg.result.dbOk} secret=${pg.result.secretScheme}(secure=${pg.result.secretSecure})`)
      : "无响应";

    const sc = replies.get("3");
    const kinds = (sc && sc.result && sc.result.kinds) || [];
    out.schemaOk = !!sc && !sc.error && kinds.length > 0;
    out.schemaDetail = out.schemaOk
      ? kinds.length + " 种引擎: " + kinds.map((k) => k.kind).join(",")
      : (sc && sc.error ? JSON.stringify(sc.error).slice(0, 100) : "无 kinds");

    const pf = replies.get("4");
    out.prefsOk = !!pf && !pf.error && !!(pf.result && pf.result.prefs);

    // 未知方法必须返回 -32601，而不是崩溃或静默无响应。
    // 静默无响应最危险：前端的 invoke 会一直挂到超时，用户只看到「卡住了」。
    const unk = replies.get("6");
    out.unknownMethodOk = !!unk && !!unk.error && unk.error.code === -32601;
    out.unknownMethodDetail = unk
      ? (unk.error ? "code=" + unk.error.code : "返回了 result 而非 error")
      : "无响应（前端会挂到超时）";

    // 密钥泄露断言：把全部 stdout 与 trace 日志拼起来，确认没有测试密钥
    // （这里没有写入过密钥，所以只需确认日志里不含 "apiKey" 后面跟着值）
    const tracePath = path.join(dataDir, "sidecar-trace.log");
    if (fs.existsSync(tracePath)) {
      const log = fs.readFileSync(tracePath, "utf8");
      if (/api[_-]?key["']?\s*[:=]\s*["']?[A-Za-z0-9_\-]{16,}/i.test(log)) {
        out.secretLeak = true;
        out.detail = "trace 日志疑似含密钥明文";
      }
    }
    return out;
  } catch (e) {
    out.detail = String((e && e.message) || e);
    return out;
  } finally {
    await removeTree(base);
    await removeTree(path.dirname(base));
  }
}

const probe = await probePackagedSidecar();

/* ---------------- 前端静态约束（对包内文件，而非源码）---------------- */
const st = (get("ui/storage.js") || Buffer.from("")).toString("utf8");
const ap = (get("ui/app.js") || Buffer.from("")).toString("utf8");
const ix = (get("ui/index.html") || Buffer.from("")).toString("utf8");
const cs = (get("ui/styles.css") || Buffer.from("")).toString("utf8");
// 剥离注释后再扫描：注释里会引用「不该这么写」的反例代码
// （例如 storage.js 的注释在解释为什么【不用】localStorage），
// 不剥注释就会把自己的文档当成违规代码报出来 —— 这是纯粹的误报。
// _buildpkg.js 早就同时剥了块注释与行注释，这里之前只剥了块注释，
// 导致两份闸门的判据不一致：同一个包 buildpkg 放行、verify 拒绝。
const stripComments = (s) => s
  .replace(/\/\*[\s\S]*?\*\//g, "")
  .replace(/^[ \t]*\/\/.*$/gm, "");
const apCode = stripComments(ap);
const stCode = stripComments(st);
const liveIds = new Set([...ix.replace(/<!--[\s\S]*?-->/g, "").matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]));
const dangling = [...new Set([...apCode.matchAll(/\$\("([^"]+)"\)\s*\./g)].map((m) => m[1]))]
  .filter((id) => !liveIds.has(id));

const checks = [
  ["manifest.version 与源码一致", mani.version === srcVer, mani.version + " vs " + srcVer],
  ["manifest.id 与源码一致（防「验的是旧 id 的包」）", mani.id === srcMani.id, mani.id + " vs " + srcMani.id],
  ["manifest.publisher 与源码一致", mani.publisher === srcMani.publisher, mani.publisher + " vs " + srcMani.publisher],
  ["文件名里的 target 合法", /^[a-z0-9-]{1,64}$/.test(pkgTarget), pkgTarget || "(解析不出 target)"],
  ["executable 指向 bin/<target>/<binary>[.exe]（windows 才带 .exe）",
    exeRel === exeExpected, exeRel + (exeExpected ? " vs " + exeExpected : "")],
  ["executable 在包内真实存在", has(exeRel)],
  ["checksums.algorithm = sha256", cks.algorithm === "sha256", cks.algorithm],
  ["checksums 精确覆盖包内文件", JSON.stringify(inPkg) === JSON.stringify(listed),
    "包内" + inPkg.length + " / 清单" + listed.length],
  ["全部 sha256 匹配", mismatch.length === 0, mismatch.join(",")],
  ["无 _ 前缀临时文件混入", !es.some((e) => /(^|\/)_/.test(e.name))],
  ["无 .dbx-dev / dist / node_modules 混入",
    !es.some((e) => /\.dbx-dev|(^|\/)dist\/|node_modules/.test(e.name))],

  // Unix 权限位：宿主 installer.rs 只在 entry.unix_mode() 有值时才 set_permissions，
  // 而 zip crate 在 external_attributes==0（或制作系统不是 Unix）时返回 None
  // → 解出来的文件是默认 0644 → **侧车没有可执行位，macOS/Linux 上起不来**。
  ["制作系统声明为 Unix（否则权限位被忽略）", es.every((e) => (e.madeBy >> 8) === 3),
    es.map((e) => e.madeBy >> 8).join(",")],
  ["可执行文件带 0755 权限位", (() => {
    const e = es.find((x) => x.name === exeRel);
    return !!e && ((e.ext >> 16) & 0o777) === 0o755;
  })(), (() => { const e = es.find((x) => x.name === exeRel); return e ? "0" + ((e.ext >> 16) & 0o777).toString(8) : "(无)"; })()],
  ["普通文件带 0644 权限位", es.filter((e) => e.name !== exeRel).every((e) => ((e.ext >> 16) & 0o777) === 0o644)],

  // 前端桥接约束
  ["storage.js 以 window.dbxPlugin 为 API", st.includes("window.dbxPlugin")],
  ["storage.js 不把 ready 的值当 API", !/resolve\(\s*\w+\s*\|\|/.test(st)],
  ["storage.js 用 invoke(method, params, {timeoutMs})", /invoke\(method,\s*params\s*\|\|\s*\{\},\s*\{\s*timeoutMs/.test(st) || /api\.invoke\(method/.test(st)],
  ["storage.js 用 request('host.getContext') 兜底", st.includes('request("host.getContext"')],
  ["storage.js 含会话内存降级（不伪装持久）", st.includes("memoryData") || st.includes("backend: \"memory\"")],
  ["storage.js 导出诊断报告与看门狗", st.includes("report:") && st.includes("WATCHDOG")],
  ["storage.js 用 onEvent 接收流式事件", st.includes("onEvent") && st.includes("translate/chunk")],
  ["storage.js 的 UI_VERSION 与 manifest 一致",
    (st.match(/var UI_VERSION = "([^"]+)"/) || [])[1] === srcVer,
    (st.match(/var UI_VERSION = "([^"]+)"/) || [])[1]],

  // 前端安全约束
  ["app.js 无 innerHTML 赋值（防 XSS）", !/\.innerHTML\s*=/.test(apCode)],
  ["无 localStorage/sessionStorage（沙箱会抛 SecurityError）",
    !/\blocalStorage\b|\bsessionStorage\b/.test(apCode + stCode)],
  ["app.js 用安全绑定 click()/on()", apCode.includes("function click(id, fn)") && apCode.includes("function on(id, ev, fn)")],
  ["app.js 没有对缺失元素直接取属性", dangling.length === 0, dangling.join(",")],
  ["app.js 捕获页面级未处理异常",
    apCode.includes("unhandledrejection") && apCode.includes('window.addEventListener("error"')],
  ["app.js 不用 dbxPlugin.ready 卡住启动", !/dbxPlugin\.ready\.then\(boot\)/.test(apCode)],
  ["app.js 有流式竞态过滤（taskId 比对）", apCode.includes("state.taskId") && apCode.includes("params.taskId")],
  ["app.js 开关 SHOW_DIAG_BAR=false", /var\s+SHOW_DIAG_BAR\s*=\s*false/.test(apCode)],
  ["app.js 开关 ENABLE_IMPORT=true", /var\s+ENABLE_IMPORT\s*=\s*true/.test(apCode)],
  ["导入入口真的在 HTML 里（#btn-import / #file-input）",
    liveIds.has("btn-import") && liveIds.has("file-input")],
  ["app.js 没有重复的函数声明", (() => {
    const seen = {};
    apCode.split(/\r?\n/).forEach((l) => {
      const m = /^  function (\w+)\s*\(/.exec(l);
      if (m) seen[m[1]] = (seen[m[1]] || 0) + 1;
    });
    return Object.keys(seen).filter((k) => seen[k] > 1).length === 0;
  })()],
  ["[hidden] 有显式 CSS（否则 display:flex 会顶掉默认隐藏）",
    /\[hidden\]\s*\{[^}]*display:\s*none/.test(cs)],
  ["明暗双主题齐备", cs.includes(":root") && /body\[data-theme="dark"\]/.test(cs)],

  /* ---- 主题/字体跟随宿主（对齐 dbx-plugin-toolbox 的 applyTheme 行为）---- */
  // env 事件必须用 capture 注册：新版宿主把 dbx-plugin-env 的 target 放在
  // document 上且【不冒泡】，注册在 window 冒泡阶段的监听器永远收不到。
  // 漏掉的症状是「DBX 切了主题，插件界面纹丝不动」，且开发宿主可能行为不同，
  // 本地测不出来、装进桌面端才暴露。
  ["env 事件用 capture 注册（否则收不到 target=document 的不冒泡事件）",
    /addEventListener\(\s*"dbx-plugin-env"[^)]*,\s*true\s*\)/.test(ap)],
  ["env 事件同时注册在 window 与 document（兼容新旧宿主）",
    /window\.addEventListener\(\s*"dbx-plugin-env"/.test(ap) &&
    /document\.addEventListener\(\s*"dbx-plugin-env"/.test(ap)],
  ["applyTheme 读取完整主题负载（含 tokens），而非只读 appearance 字符串",
    /themeObject/.test(ap) && /flattenTokens/.test(ap)],
  ["applyTheme 把宿主 token 写到文档根元素（setProperty）",
    /root\.style\.setProperty/.test(ap)],
  ["applyTheme 清理不再投递的旧 token（removeProperty，防主题切换后残留）",
    /root\.style\.removeProperty/.test(ap)],
  ["字体跟随宿主：CSS 用 var(--font-sans/--font-mono) 而非写死字体栈",
    /var\(--font-sans/.test(cs) && /var\(--font-mono/.test(cs)],
  ["宿主调色板映射到本项目语义变量（HOST_TOKEN_MAP）",
    /HOST_TOKEN_MAP/.test(ap)],
  // 映射目标必须在 CSS 里真实定义且被消费，否则映射是无效的
  // （曾经出现过映射到 --primary-fg 但 CSS 里根本没这个变量、
  //   按钮仍硬编码 color:#fff 的情况，映射写了等于没写）
  ["HOST_TOKEN_MAP 的每个目标变量都在 CSS 中定义", (() => {
    const i = ap.indexOf("var HOST_TOKEN_MAP = [");
    if (i < 0) return false;
    const block = ap.slice(i, ap.indexOf("];", i));
    const pairs = [...block.matchAll(/\["(--[a-z0-9-]+)",\s*"(--[a-z0-9-]+)"\]/g)];
    if (!pairs.length) return false;
    const defined = new Set([...cs.matchAll(/^\s*(--[a-z0-9-]+)\s*:/gm)].map((m) => m[1]));
    return pairs.every((m) => defined.has(m[2]));
  })()],
  ["HOST_TOKEN_MAP 的每个目标变量都被 CSS 消费（var()）", (() => {
    const i = ap.indexOf("var HOST_TOKEN_MAP = [");
    if (i < 0) return false;
    const block = ap.slice(i, ap.indexOf("];", i));
    const pairs = [...block.matchAll(/\["(--[a-z0-9-]+)",\s*"(--[a-z0-9-]+)"\]/g)];
    if (!pairs.length) return false;
    const consumed = new Set([...cs.matchAll(/var\((--[a-z0-9-]+)/g)].map((m) => m[1]));
    return pairs.every((m) => consumed.has(m[2]));
  })()],
  ["无硬编码白色前景（应走 --primary-fg 等变量，否则宿主换浅色主色时看不清）",
    !/color:\s*#fff\b|color:\s*#ffffff/.test(cs)],
  /* ---- 构建脚本跨平台路径（CI 事故固化成的闸门）----
   * 构建脚本里【绝不能】用硬编码反斜杠拼路径。
   *
   * 本项目在 GitHub Actions 上真实踩过：_buildpkg.js 写成
   *   root + `_xbuild\\${binary}-${goos}-${goarch}`
   * Windows 上正常，Linux runner 上反斜杠不是分隔符，拼出
   *   /home/runner/.../_xbuild\dbx-plugin-translation-darwin-arm64
   * 这种"文件名里带反斜杠"的怪路径，fs.existsSync 必然 false。
   *
   * 症状极具误导性：日志显示 5 个平台 [build] 全部成功，
   * 下一步却报 [FATAL] 找不到侧车二进制 —— 看起来像 Go 交叉编译的问题，
   * 实际只是路径分隔符。排查会一路往编译器方向走，白费时间。
   */
  ["构建脚本无硬编码反斜杠路径（必须用 path.join，否则 Linux CI 上失败）", (() => {
    const scripts = ["_buildpkg.js", "_release.mjs", "_verify.mjs", "_e2e.mjs"];
    const bad = [];
    for (const s of scripts) {
      const fp = path.join(ROOT, s);
      if (!fs.existsSync(fp)) continue;
      // 必须剥离注释再扫描：本闸门的注释里就写着这个 bug 的示例代码
      // （root + `_xbuild\...`），不剥离会自我误报。
      // 这与「storage.js 注释解释为什么不用 localStorage 却被判违规」是同一类教训。
      const src = stripComments(fs.readFileSync(fp, "utf8"));
      // 匹配 root/ROOT 与反斜杠路径的拼接
      const re = /(root|ROOT|__dirname)\s*\+\s*[`"'][^`"']*\\[^`"']*[`"']/g;
      const hits = src.match(re);
      if (hits && hits.length) bad.push(`${s}: ${hits[0].slice(0, 60)}`);
    }
    return bad.length === 0;
  })(), (() => {
    const scripts = ["_buildpkg.js", "_release.mjs", "_verify.mjs", "_e2e.mjs"];
    const bad = [];
    for (const s of scripts) {
      const fp = path.join(ROOT, s);
      if (!fs.existsSync(fp)) continue;
      const src = stripComments(fs.readFileSync(fp, "utf8"));
      const re = /(root|ROOT|__dirname)\s*\+\s*[`"'][^`"']*\\[^`"']*[`"']/g;
      for (const m of src.matchAll(re)) bad.push(`${s}: ${m[0].slice(0, 50)}`);
    }
    return bad.length ? bad.join(" | ") : "无硬编码反斜杠路径";
  })()],

    /* ---- 启动时序（三个真实事故固化成的闸门）---- */
  // 事故1：E.init() 早于 S.init() 执行 → translate/schema 在桥接未就绪时失败
  //        且永不重试 → 引擎管理页的「新建引擎」表单永远是空的。
  ["E.init 在 S.init 之后调用（schema 加载依赖桥接就绪）",
    ap.indexOf("E.init(onEnginesChanged)") > ap.indexOf("S.init().then(")],
  // 事故2：rpcRaw 在桥接未就绪时直接 reject 而不等待 → 任何过早调用都永久失败
  ["rpcRaw 桥接未就绪时等待而非直接 reject",
    /waitBridge\(BRIDGE_TIMEOUT\)/.test(st) &&
    !/if \(!api \|\| typeof api\.invoke !== "function"\) \{\s*return Promise\.reject/.test(st)],
  // 事故3：detect() 只在 boot 开头跑一次，那时 init 消息还没到，
  //        dbxPlugin.locale 是桥接闭包的初始值（en）而非真实值 → 界面永远英文。
  ["S.init 完成后重新 detect 语言（否则读到的是 init 前的初始 locale）", (() => {
    const i = ap.indexOf("S.init().then(");
    const j = ap.indexOf("E.init(onEnginesChanged)");
    if (i < 0 || j < 0 || j < i) return false;
    return /TranslationI18N\.detect\(\)/.test(ap.slice(i, j));
  })()],
  ["S.init 完成后重新 applyI18N（重绘全部 data-i18n 节点）", (() => {
    const i = ap.indexOf("S.init().then(");
    const j = ap.indexOf("E.init(onEnginesChanged)");
    if (i < 0 || j < 0 || j < i) return false;
    return /applyI18N\(\)/.test(ap.slice(i, j));
  })()],
  ["E.init 的 Promise 有 catch（失败要提示用户，否则只看到空白弹框）",
    /Promise\.resolve\(E\.init\([^)]*\)\)\.catch/.test(ap)],
  // 事件通道有三重不确定性（新版 target=document 不冒泡 / 老版 target=window /
  // 某些宿主只发 init 不发 env），轮询是廉价的静默失效兜底。
  ["有 env 轮询兜底（事件通道不可靠时语言/主题仍能跟随）",
    /startEnvPolling/.test(ap) && /setInterval/.test(ap)],
  ["轮询与事件处理器共享指纹（避免重复重绘）",
    (ap.match(/lastEnvSignature = envSignature\(\)/g) || []).length >= 2],
  ["诊断报告含界面语言与宿主 locale（排查「语言没跟随」的关键证据）",
    /界面语言=/.test(st) && /dbxPlugin\.locale=/.test(st)],
  ["诊断报告含主题 appearance 与 token 数量",
    /tokens=/.test(st) && /body\[data-theme\]=/.test(st)],
    ["界面语言跟随宿主（i18n 从 dbxPlugin.locale 取，无手动覆盖入口）",
    /dbxPlugin\.locale/.test(fs.readFileSync(path.join(ROOT, "ui/i18n.js"), "utf8")) &&
    !/setOverride/.test(fs.readFileSync(path.join(ROOT, "ui/i18n.js"), "utf8"))],

  // 权限与贡献点
  ["权限最小化：声明了 host.events（流式硬依赖）",
    Array.isArray(mani.permissions) && mani.permissions.includes("host.events"), JSON.stringify(mani.permissions)],
  ["不依赖宿主内置 AI（未声明 host.ai）",
    Array.isArray(mani.permissions) && !mani.permissions.includes("host.ai")],
  ["未声明 host.network:*（网络全走侧车）",
    Array.isArray(mani.permissions) && !mani.permissions.some((p) => String(p).startsWith("host.network"))],
  ["声明 command/menus 时 engines.dbx 已抬到 >=0.6.21",
    !(mani.contributions || []).some((c) => c.type === "command" || c.type === "menus")
      || />=\s*0\.6\.21/.test(String(mani.engines && mani.engines.dbx)),
    JSON.stringify(mani.engines)],
  ["command 的 action 引用了已声明的 workbench", (() => {
    const wbs = new Set((mani.contributions || []).filter((c) => c.type === "workbench").map((c) => c.id));
    return (mani.contributions || []).filter((c) => c.type === "command")
      .every((c) => c.action && c.action.type === "open-workbench" && wbs.has(c.action.workbench));
  })()],
  ["menus 的项引用了已声明的 command 且 location 合法", (() => {
    const cmds = new Set((mani.contributions || []).filter((c) => c.type === "command").map((c) => c.id));
    const locs = ["commandPalette", "appToolbar", "appSidebar"];
    return (mani.contributions || []).filter((c) => c.type === "menus")
      .every((c) => (c.items || []).every((it) => cmds.has(it.command) && locs.indexOf(it.location) >= 0));
  })()],
  // 包内 manifest 的字段白名单（对齐官方 schema 的 additionalProperties:false）。
  // _buildpkg.js 打包前已校验一次，这里对【包内实际内容】再校验一次 ——
  // 防止有人手工改包、或打包逻辑本身把非法字段写了进去。
  ["包内 manifest 无未声明字段（menus 只允许 type/id/items 等）", (() => {
    const ALLOWED = {
      top: ["$schema", "manifest_version", "id", "name", "icon", "version", "publisher",
            "description", "source", "homepage", "engines", "permissions", "entrypoints",
            "capabilities", "contributions", "localizations"],
      "connection-provider": ["type", "id", "label", "icon", "database_type", "description",
                              "fields", "workbench", "filesystem_provider", "capabilities",
                              "proxy_route", "actions"],
      "workbench": ["type", "id", "label", "description", "icon", "ai"],
      "filesystem-provider": ["type", "id", "label", "description", "icon", "schemes",
                              "root_uri", "capabilities"],
      "context-menu": ["type", "id", "label", "description", "icon", "menu", "dynamic", "action"],
      "result-view": ["type", "id", "label", "description", "icon"],
      "command": ["type", "id", "label", "description", "icon", "action", "enablement"],
      "menus": ["type", "id", "items"],
      "mcp": ["type", "id", "description", "ai_tools", "external_tools"],
    };
    const bad = [];
    for (const k of Object.keys(mani)) {
      if (!ALLOWED.top.includes(k)) bad.push("顶层:" + k);
    }
    for (const c of mani.contributions || []) {
      const allowed = ALLOWED[c.type];
      if (!allowed) { bad.push("未知类型:" + c.type); continue; }
      for (const k of Object.keys(c)) {
        if (!allowed.includes(k)) bad.push(c.type + ":" + k);
      }
    }
    return bad.length === 0;
  })(), (() => {
    const bad = [];
    const A = { "menus": ["type", "id", "items"], "workbench": ["type", "id", "label", "description", "icon", "ai"],
                "command": ["type", "id", "label", "description", "icon", "action", "enablement"] };
    for (const c of mani.contributions || []) {
      const allowed = A[c.type];
      if (allowed) for (const k of Object.keys(c)) if (!allowed.includes(k)) bad.push(c.type + "." + k);
    }
    return bad.join(",") || "无多余字段";
  })()],
  ["包内 manifest 的 menus 贡献点不含 label（本次安装失败的根因）",
    (mani.contributions || []).filter((c) => c.type === "menus").every((c) => c.label === undefined)],
    ["localizations 键都能对上真实贡献点 id", (() => {
    const l = mani.localizations && mani.localizations.en;
    if (!l || !l.contributions) return false;
    const ids = new Set((mani.contributions || []).map((c) => c.id));
    return Object.keys(l.contributions).every((k) => ids.has(k));
  })()],
  [".dbx-store.json 的 permissions / name 与 manifest 一致", (() => {
    try {
      const pub = JSON.parse(fs.readFileSync(path.join(ROOT, ".dbx-store.json"), "utf8"));
      return JSON.stringify(pub.permissions) === JSON.stringify(mani.permissions) && pub.name === mani.name;
    } catch { return false; }
  })()],

  // 包内二进制「真跑一次」
  ["包内侧车能启动并完成 plugin/initialize", probe.ok, probe.detail, probe.skip ? "skip" : ""],
  ["包内侧车自报版本与 manifest 一致", probe.versionOk, probe.version, probe.skip ? "skip" : ""],
  ["包内侧车 translate/ping 正常（含密钥方案与库健康）", probe.pingOk, probe.pingDetail, probe.skip ? "skip" : ""],
  ["包内侧车 translate/schema 返回引擎清单", probe.schemaOk, probe.schemaDetail, probe.skip ? "skip" : ""],
  ["包内侧车 prefs/get 可用", probe.prefsOk, "", probe.skip ? "skip" : ""],
  ["包内侧车对未知方法返回 -32601（而非静默无响应）",
    probe.unknownMethodOk, probe.unknownMethodDetail, probe.skip ? "skip" : ""],
  ["包内侧车 stdout 与 trace 日志无密钥泄露", !probe.secretLeak, probe.detail, probe.skip ? "skip" : ""],
];

console.log("\n=== 校验 ===");
let fails = 0, skips = 0;
for (const [name, ok, extra, mode] of checks) {
  if (mode === "skip") { skips++; console.log("  SKIP  " + name + (extra ? "  |  " + extra : "")); continue; }
  if (!ok) fails++;
  console.log("  " + (ok ? "PASS" : "FAIL") + "  " + name + (extra ? "  |  " + extra : ""));
}
console.log("\nRESULT: " + (fails ? "FAIL(" + fails + ")" : "PASS") +
  (skips ? `（另有 ${skips} 项因非本机平台跳过，需真机冒烟）` : ""));
process.exit(fails ? 1 : 0);
