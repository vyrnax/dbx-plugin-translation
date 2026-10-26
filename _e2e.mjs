// 侧车协议端到端测试。
//
// 与 go test 的分工：
//   - go test 覆盖【单元】：签名算法、SSE 解析、分段、加密、SQL
//   - 本脚本覆盖【集成】：真实二进制 + 真实 SQLite + 真实 JSON-RPC 协议栈
//
// 为什么必须单独有一层集成测试：
//   单元测试用的是 httptest 与内存对象，而真实链路要经过
//   「JSON Lines 编解码 → SDK 分发 → 参数反序列化 → SQLite 落盘 → 事件回推」。
//   这几层里任何一层的字段名拼错、类型不匹配，单元测试都发现不了。
//
// 测试方式：起一个假的 OpenAI 兼容端点（本地 HTTP），让侧车真的去请求它，
// 然后把 stdin 的 JSON Lines 全部发过去，按 id 关联响应与事件做断言。
//
// 用法：
//   node _e2e.mjs                # 用 backend/dbx-plugin-translation.exe
//   node _e2e.mjs <exe路径>       # 指定二进制（例如验包内解出来的那份）
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { removeTree } from "./_testutil.mjs";

const ROOT = path.dirname(fileURLToPath(import.meta.url));
const EXE = process.argv[2] || path.join(ROOT, "backend", "dbx-plugin-translation.exe");
const DATA = path.join(ROOT, "_e2e_tmp", "data-" + process.pid);
// 测试用的假密钥。全流程结束后会断言它【绝不出现】在 stdout 与 trace 日志里。
const FAKE_KEY = "sk-e2e-FAKEKEY-ABCDEFG1234567890";

if (!fs.existsSync(EXE)) {
  console.error("[FATAL] 找不到侧车二进制：" + EXE);
  console.error("        先构建：go build -C backend -o dbx-plugin-translation.exe .");
  process.exit(2);
}

/* ================= 假 OpenAI 兼容端点 =================
 * 支持三种模式，由 URL 路径区分：
 *   /v1/chat/completions        正常（按 stream 参数决定流式与否）
 *   /v1/auth-fail/...           返回 401
 *   /v1/rate-limit/...          返回 429
 * 这样能用同一个引擎配置测出多条错误分类路径。
 */
let upstreamHits = 0;
const upstream = http.createServer((req, res) => {
  upstreamHits++;
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    // 记录收到的鉴权头，用于断言密钥确实被发送（但绝不打印它）
    lastAuth = req.headers["authorization"] || "";

    if (req.url.includes("/auth-fail")) {
      res.writeHead(401, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: { message: "Incorrect API key provided" } }));
      return;
    }
    if (req.url.includes("/rate-limit")) {
      res.writeHead(429, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: { message: "Rate limit reached" } }));
      return;
    }

    let parsed = {};
    try { parsed = JSON.parse(body); } catch { /* 忽略 */ }
    const isStream = parsed.stream === true;

    if (!isStream) {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({
        choices: [{ message: { content: "你好世界" }, finish_reason: "stop" }],
      }));
      return;
    }

    // 流式：分 4 个 chunk 推送，模拟真实 SSE
    res.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
      Connection: "keep-alive",
    });
    const parts = ["你好", "，", "世界", "！"];
    let i = 0;
    const tick = () => {
      if (i >= parts.length) {
        res.write("data: [DONE]\n\n");
        res.end();
        return;
      }
      res.write("data: " + JSON.stringify({
        choices: [{ delta: { content: parts[i] } }],
      }) + "\n\n");
      i++;
      setTimeout(tick, 20);
    };
    tick();
  });
});
let lastAuth = "";

/* ================= 侧车进程 ================= */

const replies = new Map();     // id -> 响应
const events = [];             // 侧车推送的事件（无 id 的消息）
const nonJsonLines = [];       // stdout 里的非 JSON 行（违反铁律 1 的证据）
let stderrBuf = "";

function startSidecar() {
  const proc = spawn(EXE, [], {
    env: { ...process.env, DBX_PLUGIN_DATA_DIR: DATA },
    windowsHide: true,
  });
  let pending = "";
  proc.stdout.on("data", (d) => {
    pending += d.toString("utf8");
    let idx;
    while ((idx = pending.indexOf("\n")) >= 0) {
      const line = pending.slice(0, idx).trim();
      pending = pending.slice(idx + 1);
      if (!line) continue;
      try {
        const m = JSON.parse(line);
        if (m.id !== undefined && m.id !== null) replies.set(String(m.id), m);
        else if (m.method) events.push(m);
      } catch {
        // stdout 出现非 JSON = 协议流被污染，宿主会直接丢弃侧车
        nonJsonLines.push(line.slice(0, 160));
      }
    }
  });
  proc.stderr.on("data", (d) => (stderrBuf += d.toString("utf8")));
  return proc;
}

let seq = 0;
function send(proc, method, params) {
  const id = String(++seq);
  proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params: params || {} }) + "\n");
  return id;
}

// waitFor 轮询等待某个条件成立（侧车是异步的，响应顺序不保证）
async function waitFor(pred, timeoutMs, what) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (pred()) return true;
    await new Promise((r) => setTimeout(r, 20));
  }
  throw new Error("等待超时：" + what);
}

/* ================= 断言框架 ================= */
let pass = 0, fail = 0;
function check(name, ok, extra) {
  if (ok) { pass++; console.log("  PASS  " + name + (extra ? "  |  " + extra : "")); }
  else { fail++; console.log("  FAIL  " + name + (extra ? "  |  " + extra : "")); }
}

/* ================= 主流程 ================= */
async function main() {
  await new Promise((r) => upstream.listen(0, "127.0.0.1", r));
  const port = upstream.address().port;
  const endpoint = `http://127.0.0.1:${port}/v1/chat/completions`;
  console.log(`假 OpenAI 端点: ${endpoint}`);
  console.log(`侧车二进制: ${EXE}`);
  console.log(`数据目录: ${DATA}\n`);

  fs.mkdirSync(DATA, { recursive: true });
  const proc = startSidecar();

  try {
    // ---------- 1) 协议握手 ----------
    console.log("=== 1. plugin/initialize ===");
    const idInit = send(proc, "plugin/initialize", { host: { protocolVersions: [1] } });
    await waitFor(() => replies.has(idInit), 15000, "initialize 响应");
    const init = replies.get(idInit);
    check("protocolVersion = 1", init.result?.protocolVersion === 1, String(init.result?.protocolVersion));
    const mani = JSON.parse(fs.readFileSync(path.join(ROOT, "manifest.json"), "utf8"));
    check("plugin.id 与 manifest 一致", init.result?.plugin?.id === mani.id, init.result?.plugin?.id);
    check("plugin.version 与 manifest 一致", init.result?.plugin?.version === mani.version,
      `${init.result?.plugin?.version} vs ${mani.version}`);
    check("capabilities 非空", Array.isArray(init.result?.capabilities) && init.result.capabilities.length > 0,
      JSON.stringify(init.result?.capabilities));
    check("manifest.name 为「Translation 即时翻译」", mani.name === "Translation 即时翻译", mani.name);
    check("localizations.en.name 为英文", mani.localizations?.en?.name === "Translation",
      mani.localizations?.en?.name);
    check(".dbx-store.json 的 name 与 manifest 一致", (() => {
      try {
        const pub = JSON.parse(fs.readFileSync(path.join(ROOT, ".dbx-store.json"), "utf8"));
        return pub.name === mani.name;
      } catch { return false; }
    })());

    // ---------- 2) ping ----------
    console.log("\n=== 2. translate/ping ===");
    const idPing = send(proc, "translate/ping", {});
    await waitFor(() => replies.has(idPing), 10000, "ping 响应");
    const ping = replies.get(idPing).result;
    check("ok = true", ping?.ok === true);
    check("dbOk = true（SQLite 已打开）", ping?.dbOk === true, String(ping?.dbOk));
    check("dataDir 指向测试目录", String(ping?.dataDir || "").includes(String(process.pid)), ping?.dataDir);
    check("secretScheme 非空", !!ping?.secretScheme, ping?.secretScheme);
    check("secretSecure = true（Windows 上应为 DPAPI）",
      ping?.secretSecure === true || process.platform !== "win32",
      `scheme=${ping?.secretScheme} secure=${ping?.secretSecure}`);
    check("engineKinds 含 openai", Array.isArray(ping?.engineKinds) && ping.engineKinds.includes("openai"),
      JSON.stringify(ping?.engineKinds));

    // ---------- 3) schema ----------
    console.log("\n=== 3. translate/schema ===");
    const idSchema = send(proc, "translate/schema", {});
    await waitFor(() => replies.has(idSchema), 10000, "schema 响应");
    const schema = replies.get(idSchema).result;
    const kinds = schema?.kinds || [];
    check("kinds 非空", kinds.length > 0, `${kinds.length} 种`);
    const openai = kinds.find((k) => k.kind === "openai");
    check("openai schema 存在", !!openai);
    check("openai 声明支持流式", openai?.streaming === true);
    check("openai 有字段定义", (openai?.fields || []).length >= 3, `${(openai?.fields || []).length} 个字段`);
    check("openai 含 endpoint/model/apiKey 三个核心字段",
      ["endpoint", "model", "apiKey"].every((k) => (openai?.fields || []).some((f) => f.key === k)));
    const apiKeyField = (openai?.fields || []).find((f) => f.key === "apiKey");
    check("apiKey 字段标记为 secret", apiKeyField?.secret === true);
    check("apiKey 字段类型为 password", apiKeyField?.type === "password");
    check("languages 含 auto/zh/en",
      ["auto", "zh", "en"].every((c) => (schema?.languages || []).some((l) => l.code === c)),
      `${(schema?.languages || []).length} 种语言`);
    check("openai 的 languages 含 auto（支持源语言自动检测）",
      (openai?.languages || []).includes("auto"));

    // 主流引擎应全部注册（阶段 2 目标：8 类）
    const wantKinds = ["openai", "deepl", "google", "baidu", "youdao", "aliyun", "tencent"];
    const gotKinds = kinds.map((k) => k.kind);
    for (const wk of wantKinds) {
      check(`引擎 ${wk} 已注册`, gotKinds.includes(wk), gotKinds.join(","));
    }

    // 每个引擎都必须有 defaultName / defaultNameEn ——
    // 这是「选类型自动补全名称」功能的前提，缺了前端只能填入 kind 原文
    // （例如 "aliyun"），用户看到一串小写英文会以为是 bug。
    for (const k of kinds) {
      check(`引擎 ${k.kind} 有 defaultName`, !!k.defaultName, String(k.defaultName));
      check(`引擎 ${k.kind} 有 defaultNameEn`, !!k.defaultNameEn, String(k.defaultNameEn));
    }

    // defaultName 不应等于 kind 本身（那说明 schema 没配）
    for (const k of kinds) {
      check(`引擎 ${k.kind} 的 defaultName 不是 kind 原文`,
        k.defaultName !== k.kind, String(k.defaultName));
    }

    // 传统机翻引擎都不支持流式，LLM 类支持 —— 前端据此决定是否显示流式光标
    check("openai 支持流式而传统机翻不支持",
      kinds.find((k) => k.kind === "openai")?.streaming === true &&
      wantKinds.slice(1).every((wk) => kinds.find((k) => k.kind === wk)?.streaming === false));

    // 有道是唯一不支持源语言自动检测的引擎，schema 必须如实反映
    check("有道不支持 auto（supportsAuto=false）",
      kinds.find((k) => k.kind === "youdao")?.supportsAuto === false);
    check("其余引擎支持 auto",
      wantKinds.filter((k) => k !== "youdao")
        .every((wk) => kinds.find((k) => k.kind === wk)?.supportsAuto === true));

    // ---------- 4) 引擎 CRUD ----------
    console.log("\n=== 4. engines/save + list + get ===");
    const idSave = send(proc, "engines/save", {
      name: "E2E 测试引擎",
      kind: "openai",
      config: { endpoint, model: "e2e-model", temperature: 0.3 },
      secrets: { apiKey: FAKE_KEY },
      rev: -1,
    });
    await waitFor(() => replies.has(idSave), 10000, "save 响应");
    const saved = replies.get(idSave);
    check("save 成功", saved.result?.ok === true, JSON.stringify(saved.error || saved.result));
    const engineId = saved.result?.id;
    check("返回了引擎 id", !!engineId, engineId);
    const rev1 = saved.result?.rev;
    check("返回了 rev", typeof rev1 === "number", String(rev1));
    check("返回了 fingerprint", !!saved.result?.fingerprint, saved.result?.fingerprint);
    // 【关键安全断言】返回值里绝不能有密钥明文
    check("save 返回值不含密钥明文",
      !JSON.stringify(saved).includes(FAKE_KEY));
    check("fingerprint 不是完整密钥",
      saved.result?.fingerprint !== FAKE_KEY && !String(saved.result?.fingerprint || "").includes("FAKEKEY"));

    const idList = send(proc, "engines/list", {});
    await waitFor(() => replies.has(idList), 10000, "list 响应");
    const listRes = replies.get(idList).result;
    check("list 返回 1 个引擎", (listRes?.engines || []).length === 1);
    const listed = (listRes?.engines || [])[0] || {};
    check("首个引擎自动成为默认", listed.isDefault === true);
    check("首个引擎自动启用", listed.enabled === true);
    check("hasSecret = true", listed.hasSecret === true);
    check("list 返回值不含密钥明文", !JSON.stringify(listRes).includes(FAKE_KEY));
    check("list 里有 fingerprint 而非明文", !!listed.fingerprint);

    const idGet = send(proc, "engines/get", { id: engineId });
    await waitFor(() => replies.has(idGet), 10000, "get 响应");
    check("get 返回值不含密钥明文", !JSON.stringify(replies.get(idGet)).includes(FAKE_KEY));

    // ---------- 5) rev 乐观锁 ----------
    console.log("\n=== 5. rev 乐观锁 ===");
    const idStale = send(proc, "engines/save", {
      id: engineId, name: "被覆盖的名字", kind: "openai",
      config: { endpoint, model: "e2e-model" }, rev: 0,   // 故意用过期的 rev
    });
    await waitFor(() => replies.has(idStale), 10000, "stale save 响应");
    const stale = replies.get(idStale);
    // 冲突被设计成【业务结果】而非协议错误：SDK 的 Emitter.respond() 在
    // pluginError != nil 时会丢弃 result，若同时返回两者前端就拿不到 newRev、
    // 无法自动刷新重试。因此断言的是 result 而非 error。
    check("过期 rev 返回 conflict 业务结果（code=-32020）",
      stale.result?.conflict === true && stale.result?.code === -32020 && !stale.error,
      JSON.stringify(stale.result || stale.error));
    check("冲突响应带 newRev（前端据此刷新重试）",
      typeof stale.result?.newRev === "number", String(stale.result?.newRev));
    const idCheck = send(proc, "engines/get", { id: engineId });
    await waitFor(() => replies.has(idCheck), 10000, "get 响应");
    check("冲突时数据未被覆盖",
      replies.get(idCheck).result?.engine?.name === "E2E 测试引擎",
      replies.get(idCheck).result?.engine?.name);

    // ---------- 6) 连通性测试 ----------
    console.log("\n=== 6. engines/test ===");
    const idTest = send(proc, "engines/test", { id: engineId });
    await waitFor(() => replies.has(idTest), 30000, "test 响应");
    const testRes = replies.get(idTest).result;
    check("连通性测试成功", testRes?.ok === true, testRes?.message || testRes?.detail);
    check("返回了耗时", typeof testRes?.elapsedMs === "number", `${testRes?.elapsedMs}ms`);
    check("密钥确实被发送到上游（Authorization 头正确）",
      lastAuth === "Bearer " + FAKE_KEY, lastAuth ? "(已发送，内容不打印)" : "(未发送)");

    // ---------- 7) 非流式翻译 ----------
    console.log("\n=== 7. translate/run ===");
    const idRun = send(proc, "translate/run", {
      engineId, text: "hello world", sourceLang: "en", targetLang: "zh",
    });
    await waitFor(() => replies.has(idRun), 30000, "run 响应");
    const runRes = replies.get(idRun);
    check("翻译成功", runRes.result?.ok === true, JSON.stringify(runRes.error || runRes.result));
    check("译文正确", runRes.result?.target === "你好世界", runRes.result?.target);
    check("返回了 chunks", runRes.result?.chunks >= 1, String(runRes.result?.chunks));

    // ---------- 8) 流式翻译 + 事件 ----------
    console.log("\n=== 8. translate/start（流式 + 事件）===");
    events.length = 0;
    const idStart = send(proc, "translate/start", {
      engineId, text: "hello world", sourceLang: "en", targetLang: "zh", stream: true,
    });
    await waitFor(() => replies.has(idStart), 15000, "start 响应");
    const startRes = replies.get(idStart).result;
    check("start 返回 taskId", !!startRes?.taskId, startRes?.taskId);
    const taskId = startRes?.taskId;

    await waitFor(() => events.some((e) => e.method === "translate/end" && e.params?.taskId === taskId),
      30000, "translate/end 事件");
    const chunks = events.filter((e) => e.method === "translate/chunk" && e.params?.taskId === taskId);
    const endEvt = events.find((e) => e.method === "translate/end" && e.params?.taskId === taskId);
    check("收到了多个 chunk 事件（真流式）", chunks.length >= 2, `${chunks.length} 个`);
    check("chunk 增量拼接 = 完整译文",
      chunks.map((c) => c.params.text).join("") === (endEvt?.params?.target || ""),
      chunks.map((c) => c.params.text).join(""));
    check("chunk 序号从 0 递增",
      chunks.every((c, i) => c.params.index === i), chunks.map((c) => c.params.index).join(","));
    check("end 事件带完整译文", endEvt?.params?.target === "你好，世界！", endEvt?.params?.target);
    check("end 事件带耗时与引擎名", typeof endEvt?.params?.elapsedMs === "number" && !!endEvt?.params?.engine);

    // ---------- 9) 取消 ----------
    console.log("\n=== 9. translate/cancel ===");
    const idCancel = send(proc, "translate/cancel", { taskId: "不存在的任务" });
    await waitFor(() => replies.has(idCancel), 10000, "cancel 响应");
    check("取消不存在的任务是幂等的（不报错）",
      replies.get(idCancel).result?.ok === true && replies.get(idCancel).result?.alreadyDone === true,
      JSON.stringify(replies.get(idCancel).result));

    // ---------- 10) 历史 ----------
    console.log("\n=== 10. history ===");
    const idHist = send(proc, "history/list", { limit: 50, offset: 0 });
    await waitFor(() => replies.has(idHist), 10000, "history 响应");
    const hist = replies.get(idHist).result;
    check("历史已记录 2 条（run + start 各一次）", (hist?.items || []).length === 2,
      `${(hist?.items || []).length} 条，total=${hist?.total}`);
    check("历史记录不含密钥", !JSON.stringify(hist).includes(FAKE_KEY));
    const firstId = (hist?.items || [])[0]?.id;
    if (firstId) {
      const idStar = send(proc, "history/star", { id: firstId, starred: true });
      await waitFor(() => replies.has(idStar), 10000, "star 响应");
      check("收藏成功", replies.get(idStar).result?.ok === true);
      const idSearch = send(proc, "history/list", { q: "hello", limit: 10 });
      await waitFor(() => replies.has(idSearch), 10000, "search 响应");
      check("搜索命中", (replies.get(idSearch).result?.items || []).length > 0);
    }

    // ---------- 11) 偏好 ----------
    console.log("\n=== 11. prefs ===");
    const idPrefs = send(proc, "prefs/set", { prefs: { instantDebounceMs: 900, lastView: "history" }, rev: -1 });
    await waitFor(() => replies.has(idPrefs), 10000, "prefs/set 响应");
    check("prefs/set 成功", replies.get(idPrefs).result?.ok === true);
    const idPrefsGet = send(proc, "prefs/get", {});
    await waitFor(() => replies.has(idPrefsGet), 10000, "prefs/get 响应");
    const prefsBack = replies.get(idPrefsGet).result?.prefs;
    check("偏好往返一致（debounce=900）", prefsBack?.instantDebounceMs === 900, String(prefsBack?.instantDebounceMs));
    check("偏好往返一致（lastView=history）", prefsBack?.lastView === "history", prefsBack?.lastView);
    check("未写入的偏好保持默认（historyLimit=500）", prefsBack?.historyLimit === 500, String(prefsBack?.historyLimit));

    // 外观与语言偏好：主题/字体/语言一律跟随宿主，【不应入库】。
    // 尝试写入 themeOverride / uiLocale，确认它们被白名单拒绝，
    // 同时夹带的合法键不受影响。
    const idApp = send(proc, "prefs/set", {
      prefs: { themeOverride: "dark", uiLocale: "en", instantDebounceMs: 800 }, rev: -1,
    });
    await waitFor(() => replies.has(idApp), 10000, "外观偏好 set 响应");
    const appBack = replies.get(idApp).result?.prefs || {};
    check("themeOverride 被拒绝入库（主题跟随宿主）",
      appBack.themeOverride === undefined, String(appBack.themeOverride));
    check("uiLocale 被拒绝入库（语言跟随宿主）",
      appBack.uiLocale === undefined, String(appBack.uiLocale));
    check("夹带的合法键仍正常保存",
      appBack.instantDebounceMs === 800, String(appBack.instantDebounceMs));

    // 非法值应被夹到范围
    const idClamp = send(proc, "prefs/set", { prefs: { instantDebounceMs: 999999 }, rev: -1 });
    await waitFor(() => replies.has(idClamp), 10000, "clamp 响应");
    check("超范围值被夹到上限 3000",
      replies.get(idClamp).result?.prefs?.instantDebounceMs === 3000,
      String(replies.get(idClamp).result?.prefs?.instantDebounceMs));

    // ---------- 12) 错误分类 ----------
    console.log("\n=== 12. 错误分类 ===");
    const idAuth = send(proc, "engines/save", {
      name: "鉴权失败引擎", kind: "openai",
      config: { endpoint: endpoint.replace("/v1/", "/v1/auth-fail/"), model: "m" },
      secrets: { apiKey: "bad-key" }, rev: -1,
    });
    await waitFor(() => replies.has(idAuth), 10000, "save 响应");
    const authEngineId = replies.get(idAuth).result?.id;
    const idAuthTest = send(proc, "engines/test", { id: authEngineId });
    await waitFor(() => replies.has(idAuthTest), 30000, "test 响应");
    const authTest = replies.get(idAuthTest).result;
    check("401 归类为鉴权失败（code=-32031）", authTest?.code === -32031,
      `code=${authTest?.code} kind=${authTest?.kind} msg=${authTest?.message}`);
    check("鉴权失败的错误消息可理解", /鉴权|API Key|401/.test(authTest?.message || ""), authTest?.message);

    const idRate = send(proc, "engines/save", {
      name: "限流引擎", kind: "openai",
      config: { endpoint: endpoint.replace("/v1/", "/v1/rate-limit/"), model: "m" },
      secrets: { apiKey: "k" }, rev: -1,
    });
    await waitFor(() => replies.has(idRate), 10000, "save 响应");
    const rateId = replies.get(idRate).result?.id;
    const idRateTest = send(proc, "engines/test", { id: rateId });
    await waitFor(() => replies.has(idRateTest), 30000, "test 响应");
    check("429 归类为限流（code=-32032）",
      replies.get(idRateTest).result?.code === -32032,
      `code=${replies.get(idRateTest).result?.code}`);

    // 参数校验
    const idBad = send(proc, "translate/run", { text: "", targetLang: "zh" });
    await waitFor(() => replies.has(idBad), 10000, "bad params 响应");
    check("空文本返回 -32602", replies.get(idBad).error?.code === -32602,
      JSON.stringify(replies.get(idBad).error));
    const idBadLang = send(proc, "translate/run", { engineId, text: "hi", targetLang: "auto" });
    await waitFor(() => replies.has(idBadLang), 10000, "bad lang 响应");
    check("目标语言为 auto 被拒绝", !!replies.get(idBadLang).error,
      JSON.stringify(replies.get(idBadLang).error || replies.get(idBadLang).result));
    const idUnk = send(proc, "no/such-method", {});
    await waitFor(() => replies.has(idUnk), 10000, "unknown method 响应");
    check("未知方法返回 -32601", replies.get(idUnk).error?.code === -32601,
      JSON.stringify(replies.get(idUnk).error));

    // ---------- 13) 导出（默认不含密钥）----------
    console.log("\n=== 13. data/export ===");
    const idExp = send(proc, "data/export", {});
    await waitFor(() => replies.has(idExp), 20000, "export 响应");
    const exp = replies.get(idExp).result;
    check("导出成功", exp?.ok === true, JSON.stringify(replies.get(idExp).error || {}));
    const decoded = Buffer.from(exp?.dataBase64 || "", "base64").toString("utf8");
    check("导出内容可解码为 JSON", (() => { try { JSON.parse(decoded); return true; } catch { return false; } })());
    check("默认导出【不含】密钥明文", !decoded.includes(FAKE_KEY));
    check("导出含引擎与历史", (() => {
      try { const o = JSON.parse(decoded); return (o.engines || []).length > 0 && (o.history || []).length > 0; }
      catch { return false; }
    })());

    const idExpSec = send(proc, "data/export", { includeSecrets: true });
    await waitFor(() => replies.has(idExpSec), 20000, "export+secrets 响应");
    const expSec = replies.get(idExpSec).result;
    const decodedSec = Buffer.from(expSec?.dataBase64 || "", "base64").toString("utf8");
    check("显式要求时导出【含】密钥（迁移场景）", decodedSec.includes(FAKE_KEY));
    check("含密钥的导出带明文警告", /警告|WARNING/i.test(decodedSec));

    // ---------- 14) 密钥泄露总审计 ----------
    console.log("\n=== 14. 密钥泄露审计 ===");
    // stdout 的全部输出（响应 + 事件）
    const allStdout = JSON.stringify([...replies.values()]) + JSON.stringify(events);
    check("全部 RPC 响应与事件中无密钥明文", !allStdout.includes(FAKE_KEY));
    // trace 日志
    const tracePath = path.join(DATA, "sidecar-trace.log");
    const traceText = fs.existsSync(tracePath) ? fs.readFileSync(tracePath, "utf8") : "";
    check("trace 日志中无密钥明文", !traceText.includes(FAKE_KEY));
    check("trace 日志确实被写入了（否则上面的断言无意义）", traceText.length > 0, `${traceText.length} 字节`);
    check("trace 日志含脱敏标记或不含 apiKey 值",
      !/apiKey\s*[:=]\s*sk-e2e/i.test(traceText));
    // SQLite 里的密文不能是明文。
    //
    // 【必须把 -wal 与 -shm 一起读】：库开了 WAL 模式（journal_mode=WAL），
    // 侧车还在运行时新写入的数据都在 translation.db-wal 里，主库文件可能
    // 还只有 4096 字节的空壳。只读主库会得出「数据没写进去」的错误结论 ——
    // 而进程干净退出时 WAL 会 checkpoint 回主库，那时又一切正常，
    // 这种「时好时坏」的断言最难排查。
    const dbFiles = ["translation.db", "translation.db-wal", "translation.db-shm"];
    const dbRaw = Buffer.concat(dbFiles
      .map((n) => path.join(DATA, n))
      .filter((f) => fs.existsSync(f))
      .map((f) => fs.readFileSync(f)));
    check("数据库中不存在密钥明文", !dbRaw.includes(Buffer.from(FAKE_KEY, "utf8")));
    check("数据库确实写入了内容（含 WAL）", dbRaw.length > 8192,
      `${dbRaw.length} 字节（${dbFiles.filter((n) => fs.existsSync(path.join(DATA, n))).join(" + ")}）`);
    // stdout 协议纯净性（铁律 1）
    check("stdout 无非 JSON 输出（铁律 1：协议流未被污染）",
      nonJsonLines.length === 0, nonJsonLines.slice(0, 2).join(" | "));

    // ---------- 15) 关闭 ----------
    console.log("\n=== 15. 优雅关闭 ===");
    proc.stdin.end();
    const exited = await new Promise((r) => {
      const t = setTimeout(() => r(false), 8000);
      proc.on("exit", () => { clearTimeout(t); r(true); });
    });
    check("stdin 关闭后侧车自行退出（不挂死）", exited);
  } catch (err) {
    fail++;
    console.log("\n  FAIL  测试流程异常：" + (err && err.message ? err.message : String(err)));
    if (stderrBuf) console.log("  stderr: " + stderrBuf.slice(0, 500));
  } finally {
    try { proc.kill("SIGKILL"); } catch { /* 忽略 */ }
    try { upstream.close(); } catch { /* 忽略 */ }
    await removeTree(path.join(ROOT, "_e2e_tmp"));
  }

  console.log(`\nRESULT: ${fail ? "FAIL(" + fail + ")" : "PASS"}  （${pass} 通过 / ${fail} 失败）`);
  console.log(`上游端点被请求 ${upstreamHits} 次`);
  process.exit(fail ? 1 : 0);
}

main();
