// 生成 DBX Store 的【扁平 schema】候选文件。
//
// 【为什么单独抽一个脚本】
//   商店有两条上架路径，吃的格式不同：
//     a) 自动同步：dbx-store 的 discover-plugin-releases.mjs 从 Release 抓
//        release-candidates.json（{plugin, artifacts}），再由 sync-release-candidate.mjs
//        合并 .dbx-store.json 生成扁平候选。这条路我们只需产出 (a) 的文件。
//     b) 手工提 PR：作者往 fork 里写 candidates/<plugin-id>.json，
//        必须是【扁平 schema】，且 targets[].url 必须是 release 资产的【绝对 HTTPS 地址】。
//   v0.2.1 首次上架时把 (a) 的文件当成 (b) 提交，validate.mjs 直接报
//   "unknown field(s): plugin, artifacts" —— 所以两份都要能生成，且逻辑只写一遍。
//
// 【为什么必须能在 CI 里跑】
//   发布产物由 CI 从 tag 构建（见 .github/workflows/release.yml），
//   本地构建的字节内嵌的 vcs.revision 在公开仓库里查不到，会被判 provenance 硬伤。
//   因此扁平候选文件也得在 CI 里、拿到真实 release 资产哈希之后才能生成。
//
// 用法：
//   node _storecandidate.mjs                              # 用 dist/ 下的 artifact.json（本地全量构建后）
//   node _storecandidate.mjs --from <release-candidates.json> --tag v0.2.2 --out <dir>
//   node _storecandidate.mjs --repo owner/name --tag v0.2.2
//   node _storecandidate.mjs --allow-unverified          # 仅本地自测：跳过 provenance 闸门
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.dirname(fileURLToPath(import.meta.url));

// 仓库与 tag：候选文件里的 url 全部由此拼出，必须是【公开可访问、不可变】的地址。
//
// 默认值从 git remote 自动推断，而不是硬编码仓库名。
// 为什么：候选文件里的 url 全部由 "owner/repo" 拼出，一旦仓库改名或换 owner，
// 硬编码值会让【所有】 target url 与 icon url 静默指向一个不存在（甚至被别人抢注）的地址 ——
// 商店 validate.mjs 只检查「是不是 HTTPS」，不会去 HEAD 请求，所以本地和 PR 的 CI 都发现不了，
// 直到签名阶段下载失败才暴露。本项目作者就经历过 GitHub 用户名变更。
// CI 里会显式传 --repo "$GITHUB_REPOSITORY"，本地跑则靠这里推断。
const FALLBACK_REPO = "vyrnax/dbx-plugin-translation";

/**
 * 从 git remote origin 推断 owner/repo。
 *
 * @returns {string} 形如 "vyrnax/dbx-plugin-translation"；推断失败时返回 FALLBACK_REPO
 */
function detectRepo() {
  try {
    const url = execFileSync("git", ["config", "--get", "remote.origin.url"],
      { cwd: ROOT, encoding: "utf8" }).trim();
    // 同时支持 https://github.com/owner/repo(.git) 与 git@github.com:owner/repo(.git)
    const m = /github\.com[/:]([^/\s]+)\/([^/\s#?]+?)(?:\.git)?$/.exec(url);
    if (m) return m[1] + "/" + m[2];
  } catch { /* 非 git 环境或没有 remote：用兜底值 */ }
  return FALLBACK_REPO;
}

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    if (!argv[i].startsWith("--")) continue;
    out[argv[i].slice(2)] = argv[i + 1] && !argv[i + 1].startsWith("--") ? argv[++i] : true;
  }
  return out;
}

/**
 * provenance 闸门：确认「即将写进候选文件的这批字节，能被第三方从公开仓库复现」。
 *
 * 【为什么必须有这道闸门】
 *   商店审核会解包 sidecar、读 Go 内嵌的 VCS 戳（`go version -m` 的 vcs.revision /
 *   vcs.modified），然后去公开仓库核对这个 commit 是否存在、是否是声明 tag 的祖先。
 *   本地构建时 HEAD 往往还没推送，于是 revision 在远程查不到 —— 审核者据此判定
 *   「被签名的字节来自一个不存在的 commit」，这是**没有裁量空间的硬伤**。
 *   v0.2.1 与 v0.2.2 之前都因此被打回（release 资产的 sidecar 是本地 go1.27.3 构建的，
 *   内嵌 vcs.revision=53d8ebb，而远程 main 停在 a30dc7a，该 commit 从未推送）。
 *
 *   这道闸门把「人容易忘」的检查变成机器强制：不满足条件就拒绝生成可提交的候选文件。
 *
 * 【两种环境的判据不同】
 *   CI（GitHub Actions）：GITHUB_REF / GITHUB_SHA 是平台注入的可信值，
 *     直接核对「ref 就是 v<version> 这个 tag」且「SHA 就是当前 HEAD」且工作树干净。
 *   本地：没有可信环境变量，改为核对三件事 ——
 *     a) 工作树干净（否则 vcs.modified=true，字节不可复现）
 *     b) tag v<version> 存在且指向 HEAD（否则候选声明的版本与字节来源对不上）
 *     c) HEAD 已被某个远程分支包含（否则 revision 在公开仓库查不到）
 *
 * @param {string} version manifest 里的版本号，例如 "0.2.2"
 * @returns {{ok: boolean, mode: string, reasons: string[], head: string}} 检查结果
 */
export function checkProvenance(version) {
  const git = (args) => {
    try {
      return execFileSync("git", args, { cwd: ROOT, encoding: "utf8" }).trim();
    } catch (e) {
      // 非 git 环境（例如从 tarball 构建）：返回空串，由调用方按「无法证明」处理
      return "";
    }
  };
  const head = git(["rev-parse", "HEAD"]);
  const dirty = git(["status", "--porcelain"]);
  const reasons = [];

  if (!head) {
    return { ok: false, mode: "unknown", reasons: ["不在 git 仓库中，无法证明字节来源"], head: "" };
  }
  if (dirty) {
    reasons.push("工作树不干净（go build 会打上 vcs.modified=true，字节不可复现）：" +
      dirty.split("\n").slice(0, 5).join(" | "));
  }

  const inCI = process.env.CI === "true" || !!process.env.GITHUB_ACTIONS;
  if (inCI) {
    const ref = process.env.GITHUB_REF || "";
    const sha = process.env.GITHUB_SHA || "";
    const wantRef = "refs/tags/v" + version;
    if (ref !== wantRef) {
      reasons.push(`CI 的 GITHUB_REF='${ref}'，不是 '${wantRef}' —— 构建的不是该版本的 tag`);
    }
    if (sha && sha !== head) {
      reasons.push(`CI 的 GITHUB_SHA='${sha.slice(0, 12)}' 与 HEAD='${head.slice(0, 12)}' 不一致`);
    }
    return { ok: reasons.length === 0, mode: "ci", reasons, head };
  }

  // 本地：tag 必须存在且指向 HEAD
  const tagSha = git(["rev-parse", "--verify", "--quiet", "v" + version + "^{commit}"]);
  if (!tagSha) {
    reasons.push(`本地不存在 tag 'v${version}'（候选文件声明的 source 会指向一个不存在的 tag）`);
  } else if (tagSha !== head) {
    reasons.push(`tag 'v${version}' 指向 ${tagSha.slice(0, 12)}，而 HEAD 是 ${head.slice(0, 12)} —— 两者必须是同一个 commit`);
  }
  // HEAD 必须已推送到远程，否则 sidecar 内嵌的 vcs.revision 在公开仓库里查不到
  const remoteBranches = git(["branch", "-r", "--contains", head]);
  const remoteTags = git(["ls-remote", "--tags", "origin", "v" + version]);
  if (!remoteBranches && !remoteTags) {
    reasons.push(`HEAD ${head.slice(0, 12)} 尚未推送到远程 —— sidecar 内嵌的 vcs.revision 在公开仓库里查不到，` +
      "商店会判定「字节来自一个不存在的 commit」");
  }
  return { ok: reasons.length === 0, mode: "local", reasons, head };
}

/**
 * 由 release-candidates.json（官方 {plugin, artifacts} 格式）生成扁平候选对象。
 *
 * @param {object} releaseCandidates 官方格式：{plugin:{id,publisher,version,permissions,...}, artifacts:[{target,url,sha256,size}]}
 * @param {object} opts { repo, tag, storeMeta }
 * @returns {object} 符合 dbx-store schemas/plugin-candidate.schema.json 的对象
 */
export function buildStoreCandidate(releaseCandidates, opts) {
  const { repo, tag, storeMeta } = opts;
  const identity = releaseCandidates.plugin || {};
  const artifacts = releaseCandidates.artifacts || [];
  if (!identity.id || !identity.publisher || !identity.version) {
    throw new Error("release-candidates.json 缺少 plugin.id / publisher / version");
  }
  if (!artifacts.length) throw new Error("release-candidates.json 的 artifacts 为空");

  const assetUrl = (fileName) =>
    `https://github.com/${repo}/releases/download/${tag}/${encodeURIComponent(fileName)}`;
  // icon 指向 tag 上的 raw 文件：与候选字节同源、不可变，且扩展名是 .svg（商店只接受 SVG/PNG）
  const rawUrl = (p) => `https://raw.githubusercontent.com/${repo}/${tag}/${String(p).replace(/^\/+/, "")}`;

  // 键的书写顺序严格对齐 schema，便于人工逐条比对
  const candidate = {
    schemaVersion: 1,
    id: identity.id,
    publisher: identity.publisher,
    version: identity.version,
  };
  if (storeMeta.name) candidate.name = storeMeta.name;
  if (storeMeta.description) candidate.description = storeMeta.description;
  if (storeMeta.icon) {
    candidate.icon = /^[A-Za-z][A-Za-z0-9+.-]*:\/\//.test(storeMeta.icon) ? storeMeta.icon : rawUrl(storeMeta.icon);
  }
  if (Array.isArray(storeMeta.tags) && storeMeta.tags.length) candidate.tags = storeMeta.tags;
  // permissions 以 manifest（即 release-candidates.json 的 plugin.permissions）为准：
  // 商店的 sync-release-candidate.mjs 也是这么做的 —— manifest 是权威来源，
  // 手维护的 .dbx-store.json 副本会滞后，历史上造成过 catalog/package 权限不一致。
  const perms = identity.permissions && identity.permissions.length
    ? identity.permissions
    : (storeMeta.permissions || []);
  if (perms.length) candidate.permissions = [...new Set(perms)].sort();
  // source 指向【构建这批字节的确切 tag】，审核者据此核对 provenance
  candidate.source = `https://github.com/${repo}/tree/${tag}`;
  candidate.homepage = storeMeta.homepage || `https://github.com/${repo}`;
  if (storeMeta.license) candidate.license = storeMeta.license;
  if (storeMeta.releaseNotes) candidate.releaseNotes = storeMeta.releaseNotes;
  if (storeMeta.localizations) candidate.localizations = storeMeta.localizations;
  candidate.targets = artifacts
    .map((a) => ({
      target: a.target,
      url: assetUrl(path.posix.basename(new URL(a.url, "https://x.invalid/").pathname)),
      sha256: a.sha256,
      size: a.size,
    }))
    .sort((x, y) => x.target.localeCompare(y.target));
  return candidate;
}

/**
 * 把 dbx-store scripts/validate.mjs 的候选校验规则在本地先跑一遍。
 *
 * 为什么要自检：validate.mjs 在 PR 的 CI 上跑，一轮往返要等审核者触发；
 * 而它的规则是纯静态的（键白名单 / 正则 / URL 协议），本地完全能复现。
 * 提前挡住，避免「提了 PR 才发现格式错」这种低级往返。
 *
 * @param {object} c 候选对象
 * @returns {string[]} 错误列表（空数组表示通过）
 */
export function validateStoreCandidate(c) {
  const ALLOWED = ["schemaVersion", "id", "publisher", "version", "releaseNotes", "name",
    "description", "icon", "tags", "permissions", "source", "homepage", "license",
    "localizations", "targets"];
  const IDENT = /^[a-z0-9][a-z0-9._-]*$/;
  const SEMVER = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;
  const SHA = /^[A-Fa-f0-9]{64}$/;
  const STORE_RELEASES = "https://github.com/t8y2/dbx-store/releases/";
  const errs = [];
  for (const k of Object.keys(c)) if (!ALLOWED.includes(k)) errs.push(`含未声明字段 "${k}"（additionalProperties:false）`);
  if (c.schemaVersion !== 1) errs.push("schemaVersion 必须是 1");
  if (!IDENT.test(c.id || "")) errs.push(`id 非法：${c.id}`);
  if (!IDENT.test(c.publisher || "")) errs.push(`publisher 非法：${c.publisher}`);
  if (!SEMVER.test(c.version || "")) errs.push(`version 不是合法 semver：${c.version}`);
  // 新插件（plugins/<id>.json 尚不存在）时 name / source / license 是必填
  if (!c.name) errs.push("新插件必须提供 name");
  if (!/^https:\/\//.test(c.source || "")) errs.push(`source 必须是 HTTPS 仓库地址：${c.source}`);
  if (!c.license) errs.push("新插件必须提供 license");
  for (const f of ["name", "description", "icon", "source", "homepage", "license"]) {
    if (c[f] !== undefined && (typeof c[f] !== "string" || !c[f].length)) errs.push(`'${f}' 提供时必须是非空字符串`);
  }
  if (c.homepage !== undefined && !/^https?:\/\//.test(c.homepage)) errs.push("homepage 必须是 HTTP(S)");
  if (c.icon !== undefined) {
    if (!/^https?:\/\//.test(c.icon)) errs.push("icon 必须是 HTTP(S) 地址");
    else if (!/\.(svg|png)$/i.test(new URL(c.icon).pathname.split("/").pop() || "")) errs.push("icon 只能是 SVG 或 PNG");
  }
  for (const f of ["tags", "permissions"]) {
    if (c[f] !== undefined && (!Array.isArray(c[f]) || !c[f].every((x) => typeof x === "string" && x.length)
      || new Set(c[f]).size !== c[f].length)) errs.push(`'${f}' 必须是唯一的非空字符串数组`);
  }
  for (const [loc, l] of Object.entries(c.localizations || {})) {
    if (!l || typeof l !== "object" || Array.isArray(l)) { errs.push(`localizations.${loc} 必须是对象`); continue; }
    for (const k of Object.keys(l)) if (!["name", "description"].includes(k)) errs.push(`localizations.${loc} 含未声明字段 "${k}"（只允许 name/description）`);
  }
  if (c.releaseNotes !== undefined && typeof c.releaseNotes !== "string") errs.push("releaseNotes 必须是字符串");
  if (!Array.isArray(c.targets) || !c.targets.length) errs.push("至少要有一个 target");
  const seen = new Set();
  for (const t of c.targets || []) {
    for (const k of Object.keys(t)) if (!["target", "url", "sha256", "size"].includes(k)) errs.push(`target 含未声明字段 "${k}"`);
    if (!/^[a-z0-9-]{1,64}$/.test(t.target || "")) errs.push(`target 非法：${t.target}`);
    if (seen.has(t.target)) errs.push(`target 重复：${t.target}`);
    seen.add(t.target);
    let u;
    try { u = new URL(t.url); } catch { errs.push(`target url 无法解析：${t.url}`); }
    if (u) {
      if (u.protocol !== "https:") errs.push(`target url 必须 HTTPS：${t.url}`);
      if (t.url.startsWith(STORE_RELEASES)) errs.push("target url 不能指向 dbx-store 的 release（必须提交自己的未签名候选）");
    }
    if (!SHA.test(t.sha256 || "")) errs.push(`sha256 非法：${t.target}`);
    if (!Number.isSafeInteger(t.size) || t.size < 1 || t.size > 512 * 1024 * 1024) errs.push(`size 非法：${t.target}`);
  }
  return errs;
}

/* ------------------------------- CLI ------------------------------- */
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const args = parseArgs(process.argv.slice(2));
  const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, "manifest.json"), "utf8"));
  const storeMeta = JSON.parse(fs.readFileSync(path.join(ROOT, ".dbx-store.json"), "utf8"));
  const repo = args.repo || detectRepo();
  const tag = args.tag || "v" + manifest.version;
  if (tag !== "v" + manifest.version) {
    console.error(`[FATAL] tag '${tag}' 与 manifest.version '${manifest.version}' 不一致（期望 v${manifest.version}）`);
    process.exit(1);
  }

  let rc;
  if (typeof args.from === "string") {
    rc = JSON.parse(fs.readFileSync(args.from, "utf8"));
  } else {
    // 从 dist/ 下的 artifact.json 汇总（本地全量构建后的形态）
    const distDir = path.join(ROOT, "dist");
    const rcPath = path.join(distDir, "release-candidates.json");
    if (fs.existsSync(rcPath)) {
      rc = JSON.parse(fs.readFileSync(rcPath, "utf8"));
    } else {
      const arts = fs.readdirSync(distDir).filter((n) => n.endsWith(".artifact.json"))
        .map((n) => JSON.parse(fs.readFileSync(path.join(distDir, n), "utf8")));
      if (!arts.length) { console.error("[FATAL] dist/ 下没有 artifact.json，请先跑 node _release.mjs"); process.exit(1); }
      rc = {
        plugin: { id: manifest.id, name: manifest.name, description: manifest.description,
                  publisher: manifest.publisher, version: manifest.version, permissions: manifest.permissions || [] },
        artifacts: arts,
      };
    }
  }

  // provenance 闸门：默认强制。--allow-unverified 仅供本地自测跳过，
  // 跳过后产出的文件【不得】提交给商店（控制台会打醒目警告）。
  const allowUnverified = args["allow-unverified"] === true;
  const prov = checkProvenance(manifest.version);
  if (!prov.ok) {
    const head = "provenance 检查未通过（mode=" + prov.mode + "，HEAD=" + (prov.head || "?").slice(0, 12) + "）：";
    if (!allowUnverified) {
      console.error("\n[FATAL] " + head);
      prov.reasons.forEach((r) => console.error("   - " + r));
      console.error("\n   为什么这道闸门不能绕过：商店审核会解包 sidecar 读 Go 内嵌的 vcs.revision，");
      console.error("   再去公开仓库核对该 commit 是否存在、是否为声明 tag 的祖先。对不上就是硬伤。");
      console.error("\n   正确做法（见 README 第 7 节）：");
      console.error("     1) git add -A && git commit && git push origin main");
      console.error("     2) git tag v" + manifest.version + " && git push origin v" + manifest.version);
      console.error("     3) 由 .github/workflows/release.yml 从 tag 构建并上传 Release");
      console.error("     4) 用 CI 的 dbx-store-candidate artifact 提 PR（那份才带可核验的哈希）");
      console.error("\n   仅本地自测可加 --allow-unverified 跳过（产物不可提交商店）。\n");
      process.exit(1);
    }
    console.warn("\n[WARN] " + head);
    prov.reasons.forEach((r) => console.warn("   - " + r));
    console.warn("   已按 --allow-unverified 继续生成，但【这份文件不可提交给商店】：");
    console.warn("   它的 sha256 来自本地构建的字节，sidecar 内嵌的 vcs.revision 无法被审核者核验。");
    console.warn("   提交商店请用 CI 产出的 dbx-store-candidate artifact。\n");
  } else {
    console.log("provenance 检查通过（mode=" + prov.mode + "，HEAD=" + prov.head.slice(0, 12) + "）");
  }

  const candidate = buildStoreCandidate(rc, { repo, tag, storeMeta });
  const errs = validateStoreCandidate(candidate);
  if (errs.length) {
    console.error("\n[FATAL] 商店候选文件不符合 dbx-store 的 plugin-candidate schema：");
    errs.forEach((e) => console.error("   - " + e));
    console.error("\n   参考：https://github.com/t8y2/dbx-store/blob/main/schemas/plugin-candidate.schema.json\n");
    process.exit(1);
  }
  // 文件名【必须】等于 <plugin-id>.json —— validate.mjs 会核对文件名与 id 一致
  const outDir = typeof args.out === "string" ? args.out : path.join(ROOT, "dist", "store-candidate");
  fs.mkdirSync(outDir, { recursive: true });
  const outPath = path.join(outDir, candidate.id + ".json");
  fs.writeFileSync(outPath, JSON.stringify(candidate, null, 2) + "\n", "utf8");
  console.log(JSON.stringify(candidate, null, 2));
  console.log(`\n已写出 ${outPath}`);
  console.log("把该文件内容复制为 dbx-store fork 里的 candidates/" + candidate.id + ".json 即可提 PR。");
}
