// 一次产出**全部平台**的候选包 + `release-candidates.json`。
//
// 背景（官方文档与 CLI 源码实证）：
//   - `dbx-plugin package` 只按**当前宿主**平台打包；显式传 --target 指向别的平台会被拒绝：
//     "Native plugin target 'X' does not match build host 'Y'; run this package command on the target platform"
//     → 官方的多平台做法是在 CI 上开平台矩阵，各自构建，再合并出 release-candidates.json。
//   - 本插件的侧车是 **纯 Go、无 cgo**，所以可以直接交叉编译（CGO_ENABLED=0），
//     在 Windows 上就能产出 darwin-arm64 / linux-x64 的合法包。
//     ⚠️ 交叉编译的是**字节正确**，不等于**在目标机上验证过** —— 真机冒烟仍需各平台跑一次。
//
// 用法：
//   node _release.mjs                                  # 默认 5 个平台
//   node _release.mjs windows-x64 linux-x64            # 只做指定平台
//   node _release.mjs --skip-verify                    # 跳过校验（不推荐）
//
// 产物：
//   dist/<id>-<version>-<target>.dbxp
//   dist/<id>-<version>-<target>.artifact.json
//   dist/release-candidates.json                       ← 官方 {plugin, artifacts} 格式，
//                                                          dbx-store 的 sync-release-candidate.mjs 消费它
//   dist/store-candidate/<id>.json                     ← 官方【扁平】候选 schema（由 _storecandidate.mjs 生成），
//                                                          结构可直接复制进 dbx-store 的 candidates/<id>.json
//
// 【为什么两份都要出】
//   商店有两条上架路径，吃的格式不同：
//     a) 自动同步：dbx-store 的 discover-plugin-releases.mjs 从 Release 抓
//        release-candidates.json（{plugin, artifacts}），再由 sync-release-candidate.mjs
//        合并 .dbx-store.json 生成扁平候选。
//     b) 手工提 PR：作者自己往 fork 里写 candidates/<plugin-id>.json，
//        必须是【扁平 schema】（schemaVersion/id/publisher/version/.../targets[]），
//        且 targets[].url 必须是 release 资产的【绝对 HTTPS 地址】。
//   v0.2.1 首次上架时把 (a) 的文件当成 (b) 提交，validate.mjs 直接报
//   "unknown field(s): plugin, artifacts"。所以这里两份都生成，避免再拿错。
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { runProcess } from "./_testutil.mjs";

const ROOT = path.dirname(fileURLToPath(import.meta.url));
const BINARY = "dbx-plugin-translation";

// Go 工具链定位：优先 PATH 里的 go，其次本机固定安装路径。
// 用 process.env.DBX_GO 可覆盖（CI 里指定具体版本）。
const GO = process.env.DBX_GO || "go";

// GOCACHE / GOTMPDIR 指到【项目内】：本机沙箱会杀掉写到项目外的编译进程，
// 表现为 go build 无故失败。
//
// 但 CI 上【不能】这么干：actions/setup-go 已经把 GOCACHE 指到 runner 的缓存目录，
// 覆盖掉会让每次构建都从零编译（多花几分钟），且缓存完全失效。
// 用 CI 环境变量区分 —— GitHub Actions 一定会设 CI=true。
//
// 注意：这两个目录都在 .gitignore 里，因此即使建在工作树内也不会让
// go build 的 vcs.modified 变成 true（Go 用 git status --porcelain 判定，
// 被 ignore 的文件不算修改）。这一点很关键：vcs.modified=true 会让商店
// 认为「被签名的字节不是从干净源码构建的」。
const IN_CI = process.env.CI === "true" || process.env.GITHUB_ACTIONS === "true";
const GOCACHE = IN_CI ? (process.env.GOCACHE || "") : path.join(ROOT, "_gocache");
const GOTMPDIR = IN_CI ? (process.env.GOTMPDIR || "") : path.join(ROOT, "_gotmp");

// target → Go 工具链三元组（官方 current_target() 的命名：darwin/linux/windows + arm64/x64）
const TRIPLES = {
  "windows-x64": ["windows", "amd64"],
  "windows-arm64": ["windows", "arm64"],
  "darwin-arm64": ["darwin", "arm64"],
  "darwin-x64": ["darwin", "amd64"],
  "linux-x64": ["linux", "amd64"],
  "linux-arm64": ["linux", "arm64"],
};
// 默认不含 windows-arm64：占比极低，且会让 CI 多一个产物。需要时显式指定。
const DEFAULT_TARGETS = ["windows-x64", "darwin-arm64", "darwin-x64", "linux-x64", "linux-arm64"];

const rawArgs = process.argv.slice(2);
const skipVerify = rawArgs.includes("--skip-verify");
const targets = rawArgs.filter((a) => !a.startsWith("--"));
const list = (targets.length ? targets : DEFAULT_TARGETS).slice().sort();

// 部分构建：只做了 DEFAULT_TARGETS 的一个子集。
//
// 官方 reusable workflow（plugin-release-reusable.yml）按平台矩阵【逐个 target】调用
// package-command，每个 runner 只产出一个 .dbxp + 一个 .artifact.json，
// 再由它的 publish job 汇总出 release-candidates.json。
// 这种情况下本脚本【不能】写汇总文件 —— 只含 1 个 target 的候选文件是错的，
// 万一被误传到 Release 上，商店同步脚本会以为这个版本只有一个平台。
const PARTIAL = targets.length > 0 && list.length < DEFAULT_TARGETS.length;

for (const t of list) {
  if (!TRIPLES[t]) {
    console.error(`[FATAL] 未知 target：${t}。已知：${Object.keys(TRIPLES).join(", ")}`);
    process.exit(1);
  }
}

const mani = JSON.parse(fs.readFileSync(path.join(ROOT, "manifest.json"), "utf8"));
const ver = mani.version;

// 用**异步** spawn（见 _testutil.mjs）：本机沙箱里 spawnSync 对任何 exe 都直接 EBUSY。
async function run(cmd, args, opts = {}) {
  const r = await runProcess(cmd, args, {
    cwd: opts.cwd || ROOT,
    env: opts.env || process.env,
    timeoutMs: opts.timeoutMs || 900000,
  });
  if (r.out) process.stdout.write(r.out);
  if (r.err) process.stderr.write(r.err);
  if (r.error) {
    console.error(`[FATAL] 无法执行命令（${r.error.message}）：${cmd} ${args.join(" ")}`);
    process.exit(1);
  }
  if (r.code !== 0) {
    console.error(`[FATAL] 命令失败（exit ${r.code}${r.signal ? " signal=" + r.signal : ""}）：${cmd} ${args.join(" ")}`);
    process.exit(1);
  }
  return r.out || "";
}

// ---- 0) 归档旧包 ----
// 构建前把 dist/ 顶层（非 dist/old/）的旧版本产物移入 dist/old/。
// 只搬版本号 != 当前 ver 的；release-candidates.json 无版本号，排除。
// 同盘 rename 是原子的、且不删文件 —— 旧包保留可追溯（官方要求「已发布资产不可覆盖」）。
const PKG_RE = new RegExp(
  "^" + mani.id.replace(/\./g, "\\.") + "-(.+)-(windows|darwin|linux)-(x64|arm64)(\\.artifact\\.json|\\.dbxp)$"
);
function archiveOldPackages() {
  const distDir = path.join(ROOT, "dist");
  const oldDir = path.join(distDir, "old");
  if (!fs.existsSync(distDir)) return;
  if (!fs.existsSync(oldDir)) fs.mkdirSync(oldDir, { recursive: true });
  let moved = 0;
  for (const e of fs.readdirSync(distDir)) {
    const full = path.join(distDir, e);
    if (!fs.statSync(full).isFile()) continue;      // 跳过 dist/old 目录本身
    if (e === "release-candidates.json") continue;  // 无版本号，排除（避免被反复搬运）
    const m = e.match(PKG_RE);
    if (!m) continue;
    if (m[1] === ver) continue;                     // 当前版本不搬，将被重新生成
    fs.renameSync(full, path.join(oldDir, e));      // 同盘原子移动
    moved++;
  }
  if (moved) console.log(`[archive] 已归档 ${moved} 个旧包到 dist/old/`);
}

console.log(`=== 构建 ${list.length} 个平台：${list.join(", ")} ===`);
console.log(`    插件 ${mani.id} v${ver}\n`);

if (GOCACHE) fs.mkdirSync(GOCACHE, { recursive: true });
if (GOTMPDIR) fs.mkdirSync(GOTMPDIR, { recursive: true });
archiveOldPackages();

// ---- 1) 交叉编译侧车 ----
// CGO_ENABLED=0 是硬性要求：一旦引入 cgo 就无法在 Windows 上交叉编译 darwin/linux。
for (const t of list) {
  const [goos, goarch] = TRIPLES[t];
  // `go build -C backend` 先切到 backend/，所以 -o 相对 backend/：
  //   windows-x64（本机）→ backend/<binary>.exe（e2e 探针也读这份）
  //   其余目标          → ../_xbuild/<binary>-<os>-<arch>（_buildpkg.js 按这个路径取）
  const outArg = t === "windows-x64"
    ? `${BINARY}.exe`
    : `../_xbuild/${BINARY}-${goos}-${goarch}`;
  const shown = outArg.replace(/^\.\.\//, "");
  process.stdout.write(`[build] ${t.padEnd(14)} GOOS=${goos} GOARCH=${goarch} -> ${shown}\n`);
  await run(GO, ["build", "-C", "backend", "-trimpath", "-ldflags", "-s -w", "-o", outArg, "."], {
    env: {
      ...process.env,
      CGO_ENABLED: "0",
      GOOS: goos,
      GOARCH: goarch,
      // 空串会【覆盖】掉继承来的正确值，所以只在非 CI 时显式传
      ...(GOCACHE ? { GOCACHE } : {}),
      ...(GOTMPDIR ? { GOTMPDIR } : {}),
    },
  });
}

// ---- 2) 逐平台打包 ----
console.log("");
for (const t of list) {
  await run(process.execPath, [path.join(ROOT, "_buildpkg.js"), "--target", t]);
}

// ---- 3) 逐包校验 ----
if (!skipVerify) {
  console.log("");
  const pkgOf = (t) => path.join(ROOT, "dist", `${mani.id}-${ver}-${t}.dbxp`);
  for (const t of list) {
    console.log(`--- verify ${t} ---`);
    await run(process.execPath, [path.join(ROOT, "_verify.mjs"), pkgOf(t)]);
  }
} else {
  console.log("\n[WARN] 已跳过校验（--skip-verify）。上架前必须完整校验一次。\n");
}

if (PARTIAL) {
  console.log("\n[partial] 本次只构建了 " + list.join(", ") + "，跳过汇总文件生成。");
  console.log("          汇总由调用方（官方 reusable workflow 的 publish job）完成。\n");
  process.exit(0);
}

// ---- 4) 汇总 release-candidates.json ----
// 结构对齐 DBX Store 官方发布流程：plugin 元信息 + artifacts（每平台的 target/url/sha256/size）。
// url 用**文件名**（与官方 artifact.json 一致）；候选包本体放在 GitHub Release / CDN 上，
// 由 dbx-store 的同步 Workflow 读取本文件生成候选 PR。
const artifacts = [];
for (const t of list) {
  const ap = path.join(ROOT, "dist", `${mani.id}-${ver}-${t}.artifact.json`);
  if (!fs.existsSync(ap)) {
    console.error(`[FATAL] 缺少 ${path.basename(ap)}`);
    process.exit(1);
  }
  const a = JSON.parse(fs.readFileSync(ap, "utf8"));
  artifacts.push({ target: a.target, url: a.url, sha256: a.sha256, size: a.size });
}
artifacts.sort((x, y) => x.target.localeCompare(y.target));

const payload = {
  plugin: {
    id: mani.id,
    name: mani.name,
    description: mani.description,
    publisher: mani.publisher,
    version: mani.version,
    permissions: mani.permissions || [],
  },
  artifacts,
};
const rcPath = path.join(ROOT, "dist", "release-candidates.json");
fs.writeFileSync(rcPath, JSON.stringify(payload, null, 2) + "\n", "utf8");

console.log("\n=== release-candidates.json ===");
console.log(JSON.stringify(payload, null, 2));

// ---- 5) 生成【扁平 schema】的商店候选文件 ----
//
// 逻辑在 _storecandidate.mjs 里（可单独调用）：CI 构建完、拿到真实 release 资产哈希后，
// 也需要用同一份逻辑生成扁平候选，两处不能各写一遍否则会漂移。
//
// 【重要】本地产物只用于开发自测。要提交给商店的候选文件，必须用【CI 从 tag 构建】
// 的那批资产哈希来生成 —— 本地构建的 sidecar 内嵌的 vcs.revision 在公开仓库里查不到，
// 商店会判为 provenance 硬伤（v0.2.1 因此被打回）。
// provenance 闸门的开关：
//   CI 里【不加】--allow-unverified —— 闸门强制生效。CI 由 tag 触发，
//     GITHUB_REF/GITHUB_SHA 是平台注入的可信值，能真正证明字节来源，
//     生成的这份文件才是【可以提交给商店】的。
//   本地【加】--allow-unverified —— 本地跑 _release.mjs 是为了自测打包链路，
//     此时工作树通常是脏的、tag 也还没打，闸门必然不过；硬卡住会打断开发节奏。
//     脚本内部会打醒目警告，说明这份文件的 sha256 不可提交商店。
console.log("");
const provArgs = IN_CI ? [] : ["--allow-unverified"];
await run(process.execPath, [path.join(ROOT, "_storecandidate.mjs"), "--tag", "v" + ver, ...provArgs]);

const totalMB = artifacts.reduce((s, a) => s + a.size, 0) / 1024 / 1024;
console.log(`\n已写出 ${rcPath}`);
console.log("已写出 dist/store-candidate/" + mani.id + ".json（扁平 schema 候选）");
console.log(`共 ${artifacts.length} 个平台产物，合计 ${totalMB.toFixed(1)} MB：`);
artifacts.forEach((a) => console.log(`  ${a.target.padEnd(14)} ${(a.size / 1024 / 1024).toFixed(2)} MB  ${a.sha256.slice(0, 16)}…`));
