# Translation 即时翻译（DBX 插件）

多引擎翻译插件，运行在 [DBX](https://dbxio.com) 宿主里。**装好就能直接翻译**：内置「免费翻译服务」引擎（B站公开部署的 Index-Translate 模型），无需注册、无需 API Key、无需任何配置。

需要更稳定的服务时可自行添加引擎，共支持 **9 类**：**内置免费翻译服务**、**OpenAI 兼容大模型**（DeepSeek、通义千问、智谱、Kimi、Ollama、vLLM、LM Studio…）、**DeepL**、**Google**、**百度**、**有道**、**阿里云**、**腾讯云**与**本地 CLI 代理**（claude / codex），含流式输出、输入即译、引擎管理与翻译历史。

API Key 由内置的 **Go 原生侧车**经系统密钥库（Windows DPAPI / macOS Keychain / Linux secret-tool）加密后落盘，前端全程接触不到明文。

> 开发规范见 [`AGENTS.md`](./AGENTS.md)，实施蓝图见 [`PLAN.md`](./PLAN.md)。

---

## 1. 架构总览

```
┌──────────────────────────────────────────────────────────────┐
│  DBX 宿主（桌面端 / dbx-plugin dev 开发宿主）                   │
│                                                                │
│  ┌────────────────────────────────────────────────────────┐   │
│  │  UI 工作台（sandbox iframe，opaque origin）              │   │
│  │   index.html · app.js · engines.js · history.js         │   │
│  │   · storage.js · i18n.js · styles.css                   │   │
│  │      │ invoke()  ↓↑  onEvent(translate/chunk|end|error)  │   │
│  └──────┼──────────────────────────────────────────────────┘   │
│         │   JSON-RPC 2.0（stdin/stdout, JSON Lines）            │
│         ▼                                                       │
│  ┌────────────────────────────────────────────────────────┐   │
│  │  原生侧车（Go 可执行文件，独立进程，不受 CSP 约束）        │   │
│  │   main.go            RPC 分发 + 流式任务表               │   │
│  │   dbxsdk/            vendor 的官方 Go SDK（禁改）        │   │
│  │   engine/            Engine 接口 + 注册表 + schema       │   │
│  │   engine/openai …    各引擎适配器                        │   │
│  │   secret/            DPAPI / Keychain / secret-tool / AES│   │
│  │   store/             SQLite（modernc，纯 Go 无 cgo）     │   │
│  │   lang/ chunk/ trace/  语言映射 · 分段器 · 脱敏日志       │   │
│  │   持久化：<数据目录>/translation.db                       │   │
│  └────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

四个关键事实：

1. **前端不碰网络，也不碰磁盘**。UI 跑在 opaque origin 的沙箱 iframe 里，开发宿主的 CSP 是 `connect-src 'none'`，`localStorage` 一碰就抛 `SecurityError`。所有 HTTP 请求与落盘都由侧车完成。
2. **侧车是原生进程**，不是网页 Worker / WASM。由 DBX 宿主按需启动，通过 **stdin/stdout 上的 JSON-RPC 2.0（JSON Lines）** 通信。
3. **`invoke` 与 `request` 不能混用**：`dbxPlugin.invoke()` 调侧车，`dbxPlugin.request()` 调宿主。
4. **流式翻译靠侧车事件**，需要 manifest 声明 `host.events` —— 宿主只在声明了该权限时才把侧车事件转发给前端。

---

## 2. 目录结构

```
dbx-plugin-translation/
├── AGENTS.md              # 开发规范手册（强制遵守，含 15 条铁律与踩坑清单）
├── PLAN.md                # 实施蓝图（功能/技术架构、详设、分阶段任务）
├── README.md              # 本文件
├── LICENSE                # Apache-2.0 全文（商店首次上架强制要求，缺失会被拒）
├── RELEASE_NOTES.md       # 【只写当前版本】的变更说明，CI 建 Release 时作为 body；历史归档交给 GitHub Releases 与 git tag
├── manifest.json          # 插件清单（id / 版本 / entrypoints / contributions / 权限）
├── dbx-plugin.toml        # 打包配置（后端语言、二进制名、include）
├── .dbx-store.json        # 商店元数据（name/permissions 必须与 manifest 一致）
├── .github/workflows/
│   ├── ci.yml             # 单测 + -race + 前端闸门 + e2e + 多平台打包校验
│   └── release.yml        # 【发版入口】推 tag 触发，复用官方 reusable workflow 从 tag 真机构建
├── assets/                # 图标（plugin.svg + 各引擎 logo，全部 LF 换行符）
├── ui/                    # 前端（sandbox iframe 内运行，无构建步骤）
│   ├── index.html
│   ├── app.js             # 主翻译页、视图切换、即时翻译、流式渲染
│   ├── engines.js         # 引擎管理：schema 驱动的动态表单
│   ├── history.js         # 历史：分页、搜索、收藏、删除
│   ├── storage.js         # 桥接层：invoke/onEvent、超时、诊断、看门狗、rev 管理
│   ├── i18n.js            # zh-CN / en 双语字典（语言跟随宿主 dbxPlugin.locale）
│   └── styles.css         # 明暗双主题 + 消费宿主 --color-*/--font-*/--radius-* token
├── backend/               # 原生侧车（Go）
│   ├── main.go            # RPC 分发、流式任务表、生命周期
│   ├── dbxsdk/            # vendor 的官方 Go SDK（sdk.go + VENDOR.md）
│   ├── engine/            # Engine 接口、注册表、schema、HTTP 客户端、错误分类
│   │   ├── openai/        # OpenAI 兼容（SSE 真流式）：DeepSeek/通义/智谱/Kimi/Ollama/vLLM…
│   │   ├── localcli/      # 本地 CLI 代理（claude / codex / 自定义），参数数组直接 exec
│   │   ├── free/          # 内置免费引擎（复用 openai 适配器 + 锁定配置，表单零字段）
│   │   ├── deepl/         # DeepL REST（free/pro/自定义端点、formality、glossary）
│   │   ├── google/        # Google Cloud Translation v2
│   │   ├── baidu/         # 百度翻译（MD5 签名）
│   │   ├── youdao/        # 有道智云（SHA256 签名 + truncate 规则）
│   │   ├── aliyun/        # 阿里云机器翻译（ACS3-HMAC-SHA256 签名 V3）
│   │   └── tencent/       # 腾讯云 TMT（TC3-HMAC-SHA256 签名）
│   ├── secret/            # 密钥保护（DPAPI / Keychain / secret-tool / AES-GCM）
│   ├── store/             # SQLite 存储层（engines / secrets / history / prefs / meta）
│   ├── lang/              # 规范语言集 + 各引擎私有码映射
│   ├── chunk/             # 分段器（保护代码块 / URL / 占位符）
│   ├── trace/             # 脱敏文件日志
│   └── go.mod / go.sum
├── dist/                  # 构建产物（gitignore，不进版本库）
│   ├── *.dbxp             # 各平台未签名候选包
│   ├── *.artifact.json    # 官方同名元数据（target/url/sha256/size）
│   ├── release-candidates.json          # {plugin, artifacts} 格式 → 上传到 GitHub Release
│   ├── store-candidate/<id>.json        # 扁平 schema → 手工提 dbx-store PR 时用
│   └── old/               # 历史版本归档（只挪不删）
├── _release.mjs           # 一键构建 + 打包 + 校验 + 出两份候选元数据
├── _buildpkg.js           # 单平台打包（zip + checksums + Unix 权限位 + 闸门）
├── _verify.mjs            # 单包校验（结构 / sha256 / 权限位 / 真跑侧车探针）
├── _e2e.mjs               # 侧车协议端到端测试（含假 OpenAI 端点，77 项断言）
├── _domcheck.py           # 前端静态闸门（DOM 悬空引用 / 关键元素 / 重复函数）
├── _storecandidate.mjs    # 生成 dbx-store 扁平 schema 候选文件（内置 validate.mjs 规则自检）
└── _testutil.mjs          # 异步 spawn 封装（绕过沙箱 spawnSync EBUSY）
```

---

## 3. 侧车 RPC 参考

所有方法经 `plugin.Handle()` 平铺分发，未知方法返回 `-32601`。

| 方法 | 入参（关键字段） | 返回 |
|---|---|---|
| `translate/ping` | — | `{ok, plugin, version, dataDir, dbOk, dbPath, secretScheme, secretSecure, inflight, engineKinds, rev}` |
| `translate/schema` | — | `{kinds:[{kind,label,description,streaming,maxChars,needsSecret,fields[],languages[],targetLanguages[],supportsAuto}], languages:[{code,zh,en,llm}]}` |
| `engines/list` | — | `{engines:[{id,name,kind,config,hasSecret,fingerprint,isDefault,enabled,ord}], rev}` |
| `engines/get` | `id` | `{engine, rev}`（不含密钥明文） |
| `engines/save` | `id?, name, kind, config, secrets?, rev` | `{ok, id, rev, fingerprint, hasSecret, secretAction, warning?}` |
| `engines/delete` | `id, rev` | `{ok, rev}` |
| `engines/setDefault` | `id, rev` | `{ok, rev}` |
| `engines/toggle` | `id, enabled, rev` | `{ok, rev, enabled}` |
| `engines/reorder` | `ids[], rev` | `{ok, rev}` |
| `engines/test` | `id` 或 `kind + config + secrets` | `{ok, message, detail?, kind?, code?, elapsedMs, streaming?, maxChars?}` |
| `translate/run` | `engineId?, text, sourceLang, targetLang` | `{ok, target, elapsedMs, engine, engineKind, model, detectedLang, chunks}` |
| `translate/start` | `engineId?, text, sourceLang, targetLang, stream?` | `{ok, taskId}` |
| `translate/cancel` | `taskId` | `{ok, taskId, alreadyDone?}` |
| `history/list` | `limit?, offset?, q?, starred?` | `{items[], total}` |
| `history/star` | `id, starred` | `{ok, starred}` |
| `history/delete` | `ids[]` | `{ok, deleted}` |
| `history/clear` | `keepStarred?` | `{ok, deleted}` |
| `prefs/get` | — | `{prefs, rev}` |
| `prefs/set` | `prefs, rev` | `{ok, prefs, rev}` |
| `data/export` | `includeSecrets?` | `{ok, fileName, dataBase64, bytes, counts, hasSecrets}` |
| `data/import` | `dataBase64, mode` | `{ok, counts, rev, mode}` |

### 3.1 侧车事件（需 `host.events` 权限）

```
translate/chunk  { taskId, text, index, total }
translate/end    { taskId, target, elapsedMs, engine, engineKind, model, detectedLang, chunks }
translate/error  { taskId, code, message }
```

前端用 `dbxPlugin.onEvent()` 订阅，并按 `taskId` 做**竞态过滤**：不匹配当前任务的事件直接丢弃。这是「输入即译」不出错的关键 —— 否则快速打字时旧响应晚到会覆盖新结果。

### 3.2 错误码

| Code | 含义 | 可重试 |
|---|---|---|
| `-32601` | 方法不存在 / 能力不支持 | 否 |
| `-32602` | 参数非法 / 配置问题 | 否 |
| `-32001` | 协议版本不匹配（SDK 自动） | 否 |
| `-32002` | 存储读写失败 | 是（一次） |
| `-32004` | 导出失败 | 否 |
| `-32006` | 导入失败 | 否 |
| `-32012` | 偏好写入失败 | 否 |
| `-32020` | rev 冲突（数据已被其它会话修改） | 是（刷新 rev 后一次） |
| `-32030` | 引擎网络失败 / 超时 / 上游 5xx | 用户手动 |
| `-32031` | 引擎鉴权失败（401/403） | **否**（重试无意义且浪费配额） |
| `-32032` | 引擎限流（429）/ 配额耗尽 | 否，提示稍后 |
| `-32033` | 引擎响应格式非法 | 否，附截断原始片段 |
| `-32034` | 翻译被取消 | 否 |
| `-32035` | 密钥解密失败（密钥库不可用 / 机器已变更） | 否，提示重新输入 |
| `-32036` | 文本超出引擎上限且无法分段 | 否 |
| `-32037` | 引擎未配置或已禁用 | 否 |

---

## 4. 主题、字体与语言：一律跟随宿主 DBX

本插件**不提供**任何主题或语言的手动设置项 —— 三者全部实时跟随 DBX，在 DBX 里改了立即生效，无需重开工作台。

| 跟随项 | 宿主投递形式 | 本插件的消费方式 |
|---|---|---|
| 浅色 / 深色 | `theme.appearance` | 设 `body[data-theme]`，切换 CSS 里两套兜底变量 |
| 调色板与自定义配色 | `--color-*` tokens | 经 `HOST_TOKEN_MAP` 映射到本项目语义变量（`--bg`/`--panel`/`--text`/`--primary`…） |
| 圆角风格 | `--radius-*` tokens | 映射到 `--radius` / `--radius-sm` |
| UI 字体 | `--font-sans` token | CSS 用 `var(--font-sans, 兜底字体栈)` |
| 编辑器（等宽）字体 | `--font-mono` token + `theme.editor.fontFamily` | CSS 用 `var(--font-mono, 兜底等宽栈)` |
| 界面语言 | `dbxPlugin.locale` | `i18n.js` 归一化后选 zh-CN 或 en 字典 |

**字号不跟随**：官方文档明确「字体&字号类关注点归插件所有，宿主不规定你的字号」。`theme.editor.fontSize` 是编辑器字号，UI 正文仍由插件自定为 14px。

### 4.1 两个实现要点

**① `dbx-plugin-env` 必须用 capture 注册。** 新版宿主把该事件 target 在 `document` 上且**不冒泡**，注册在 `window` 冒泡阶段的监听器永远收不到：

```js
window.addEventListener("dbx-plugin-env", onEnv, true);    // capture
document.addEventListener("dbx-plugin-env", onEnv, true);  // 双注册兼容新旧宿主
```

漏掉的症状是「DBX 切了主题，插件界面纹丝不动」，而**开发宿主测不出来**（它在 iframe 内 `dispatchEvent`，会冒泡），装进桌面端才暴露。另加 `onInit` 兜底，因为某些宿主桥接就绪时只发 init 不发 env。

**② 宿主 token 名与本项目变量名对不上，必须做映射。** 本项目 CSS 用 `--bg`/`--panel`/`--text`，宿主投递的是 `--color-background`/`--color-card`/`--color-foreground`。不映射的话 token 投递了也没人消费，用户在 DBX 里自定义的主色、边框色、圆角全都传不进来（只有明暗切换生效，因为 CSS 里有两套兜底值）。

映射目标必须在 CSS 里**真实定义且被 `var()` 消费**，否则映射写了等于没写 —— `_verify.mjs` 会解析 `HOST_TOKEN_MAP` 的每一对逐个核对。

### 4.2 开发宿主的局限

`dbx-plugin dev` 只投递 **15 个 token**（`--color-*` 与 `--radius-*`），**不含 `--font-*`**（已实测确认）。因此：

- 调色板与圆角跟随 → 开发宿主可验证
- **字体跟随 → 只能在桌面端验证**，开发宿主里永远走兜底字体栈
- 语言跟随 → 开发宿主可在调试页切换语言验证

设置页的「外观（跟随宿主）」组是**只读展示**，显示当前跟随到的主题、语言、UI 字体与宿主投递的 token 数量，便于确认插件确实跟上了 DBX 的设置。

---

## 5. 密钥安全模型

**分层策略**：

| 平台 | 主方案 | 降级 |
|---|---|---|
| Windows | DPAPI `CryptProtectData`（绑定当前用户账户，附加插件 ID 派生的 entropy） | 机器绑定 AES-256-GCM |
| macOS | Keychain（`security` 命令，参数数组，非 shell 拼接） | 同上 |
| Linux | libsecret（`secret-tool`，密钥从 stdin 传入以免出现在 `ps` 输出里） | 同上 |

降级方案**只是混淆，不是加密**（能读磁盘的本地进程也能派生出密钥）。因此 `translate/ping` 会返回 `secretSecure: false`，前端据此在界面顶部常驻黄色警示条明确披露，**绝不静默降级**。

**密钥流转五道铁律**：

1. 前端 `engines/save` 传入明文 → 侧车立即加密 → 返回值只含 `fingerprint`（如 `sk-e****7890`），**绝不回传明文**
2. `engines/list` / `engines/get` 只返回 `hasSecret` + `fingerprint`；`ListEngines` 的 SQL **刻意不 JOIN secrets 表**，从结构上杜绝泄露
3. 明文只在「加密前」与「构造 HTTP 请求头时」两个瞬间存在，不写入任何日志 / 事件 / 错误消息
4. `data/export` 默认**不含**密钥；`includeSecrets: true` 时导出文件内写入明文警告
5. 解密失败返回 `-32035` 并提示重新输入，**不静默清空**配置

`_e2e.mjs` 的第 14 节对此做自动化审计：把全部 RPC 响应、事件、trace 日志、SQLite 文件（含 `-wal`/`-shm`）拼起来 grep 测试密钥，必须零命中。

---

## 6. 构建与打包

### 6.1 前置

- Go（本机 1.27.3 @ `C:\develop\Go`），`GOPROXY=https://goproxy.cn,direct`
- **发布产物的 Go 工具链 pin 到 `1.21.13`**（见 `.github/workflows/release.yml` 与 `ci.yml`）。本地开发用更高版本没问题，但**提交商店的包必须由 CI 用 pin 的版本构建**，原因见 6.3 与第 7 节。
- Node.js 18.18+（跑打包/校验脚本，只用内置模块，无需 `npm install`）
- `@dbx-app/plugin-cli`（`dbx-plugin` 命令）

### 6.2 开发调试

```bash
# 编译侧车
go build -C backend -o dbx-plugin-translation.exe .

# 启动开发宿主（不启动 DBX 桌面端）
dbx-plugin dev --path . --port 5190
# 打开 http://127.0.0.1:5190/
```

**开发宿主的局限**（官方明确说明，且已实测确认）：它只模拟 6 个桥接方法（`host.getContext`、`backend.invoke`、`backend.notify`、`backend.sendBinary`、`ui.readAsset`、`host.openWorkbench`），且 CSP 为 `connect-src 'none'`、`child.stderr.resume()` 直接丢弃 stderr。因此：

- `storage` / `clipboard` / `floating` / `queryData` / `saveFile` / `copy` 在开发宿主里**全部不可用**
- `command` / `menus` 贡献点开发期不生效
- **安装、签名、真实数据目录、桌面端生命周期必须再用 DBX 桌面端验证**

侧车日志在 `<数据目录>/sidecar-trace.log`（>64 KiB 自动截断，内容已脱敏）。前端诊断报告在「⋯ → 查看诊断报告」。

### 6.3 一键出全平台包

```bash
node _release.mjs                       # 默认 5 个平台
node _release.mjs windows-x64 linux-x64 # 只做指定平台
```

它会依次：① 交叉编译侧车（`CGO_ENABLED=0`，纯 Go 无 cgo 才能跨平台）② 逐平台 `_buildpkg.js` 打包 ③ 逐包 `_verify.mjs` 校验（含真跑侧车探针）④ 汇总 `dist/release-candidates.json`。

> 官方 `dbx-plugin package` **拒绝**为非当前宿主平台打包。本插件因侧车纯 Go 无 cgo，可直接在 Windows 上交叉编译出 darwin/linux 的合法包。
>
> ⚠️ 交叉编译只保证**字节正确**，不等于在目标机上验证过。`darwin-*` / `linux-*` 包的侧车探针在校验阶段标记为 `SKIP`，**需在对应真机各跑一次冒烟**。
>
> 🚫 **本地 `node _release.mjs` 的产物只能用于开发自测，绝不能提交给商店。**
> Go 会把构建时的 git 戳编进二进制（`go version -m <exe>` 可见 `vcs.revision` / `vcs.modified`）。
> 本地构建时 HEAD 往往还没推到远程，于是 sidecar 内嵌的 `vcs.revision` 在公开仓库里查不到 ——
> 商店审核会据此判定「被签名的字节来自一个不存在的 commit」，这是**没有裁量空间的硬伤**。
> v0.2.1 正是因为本地构建 + 手工上传而被打回。提交商店的包一律由 CI 从 tag 构建（见第 7 节）。

`_release.mjs` 会同时产出两份候选元数据，**格式不同、用途不同，别拿错**：

| 文件 | 格式 | 用途 |
|---|---|---|
| `dist/release-candidates.json` | `{plugin, artifacts}` | 上传到 GitHub Release。dbx-store 的 `discover-plugin-releases.mjs` 抓它，再由 `sync-release-candidate.mjs` 合并 `.dbx-store.json` 自动生成扁平候选 |
| `dist/store-candidate/com.vyrnax.translation.json` | 扁平 schema（`schemaVersion`/`targets[]`） | 手工往 `t8y2/dbx-store` fork 里写 `candidates/<id>.json` 提 PR 时用。由 `_storecandidate.mjs` 生成，内置 `validate.mjs` 的规则自检 |

把 `{plugin, artifacts}` 那份当成手工 PR 的候选文件提交，`validate.mjs` 会直接报
`unknown field(s): plugin, artifacts`（v0.2.1 踩过）。

### 6.4 测试

```bash
# Go 单元测试（含 -race 并发检查）
go test -C backend ./...
go test -C backend -race ./...

# 前端静态闸门
python _domcheck.py

# 侧车协议端到端（77 项断言，含假 OpenAI 端点与密钥泄露审计）
go build -C backend -o dbx-plugin-translation.exe . && node _e2e.mjs

# 包结构与真跑探针
node _verify.mjs
```

### 6.5 版本号三处必须一致

改版本时同步这三处，`_buildpkg.js` 会做一致性闸门校验：

1. `manifest.json` 的 `version`
2. `backend/main.go` 的 `pluginVersion`
3. `ui/storage.js` 的 `UI_VERSION`

不一致会导致「装的是新版、跑的是旧代码」这种极难排查的问题。侧车运行时还会用 `resolveMetadata()` 从包内 `manifest.json` 反读版本号，作为第二道保险。

---

## 7. 发布到官方商店

**唯一正确的发布路径：推 tag → CI 从 tag 构建 → 产物自动上传到 Release → 提候选 PR。**
不要本地构建后手工上传（原因见 6.3 的 provenance 警告）。

### 7.1 发版步骤

```bash
# 1) 改版本号（三处必须一致，_buildpkg.js 有闸门校验）
#    manifest.json / backend/main.go 的 pluginVersion / ui/storage.js 的 UI_VERSION
#    RELEASE_NOTES.md 要【整体重写为当前版本的变更】，不要在旧内容后面追加
#    （_buildpkg.js 有闸门：首行版本号必须等于 manifest.version，否则拒绝打包）

# 2) 本地自检（产物仅用于自测）
python _domcheck.py
go test -C backend ./...
node _e2e.mjs
node _release.mjs          # 本地会自动加 --allow-unverified，产物不可提交商店

# 3) 提交并打 tag（tag 名必须是 v + manifest.version）
git add -A && git commit -m "..."
git push origin main
# tag 名必须等于 v + 当前 manifest.version（下面用 shell 现场取，避免升版后照抄到旧版本号）
VER=$(node -p "require('./manifest.json').version")
git tag "v$VER" && git push origin "v$VER"
```

tag 一推，`.github/workflows/release.yml` 就会：

1. **prepare**：校验 `GITHUB_REF` 确实是目标 tag、tag 与 `manifest.version` 一致，创建（或转为）**正式发布**（`--draft=false --prerelease=false --latest`）
2. **build**：调用官方 reusable workflow `t8y2/dbx/.github/workflows/plugin-release-reusable.yml`（pin 到具体 commit），按平台矩阵在**各自平台的 runner 上真机构建** 5 个 target，校验 sha256/size、拒绝已签名包、核对各 target 的 manifest 身份一致，汇总 `release-candidates.json` 后 `gh release upload --clobber`
3. **summary**：用 CI 的真实资产哈希跑 `_storecandidate.mjs` 生成扁平候选文件并上传为 `dbx-store-candidate` artifact，同时打印各 target 哈希与 sidecar 的 VCS 戳（provenance 自证）

> **为什么 Release 必须是非 prerelease**：商店的 `discover-plugin-releases.mjs` 只认 `!draft && !prerelease` 的 Release，标成 prerelease 会导致候选永远同步不进目录。

#### provenance 闸门（`_storecandidate.mjs` 内置）

生成扁平候选文件前，脚本会强制核对「这批字节能否被第三方从公开仓库复现」：

- **CI 环境**：核对 `GITHUB_REF` 就是 `refs/tags/v<version>`、`GITHUB_SHA` 等于 HEAD、工作树干净。
- **本地环境**：核对工作树干净、tag `v<version>` 存在且指向 HEAD、HEAD 已推送到远程。

任一不满足即 `[FATAL]` 退出。本地跑 `_release.mjs` 会自动带 `--allow-unverified` 跳过（仅供自测，
控制台打醒目警告，产物**不可提交商店**）。这道闸门把「本地构建的 sidecar 内嵌了未推送的
`vcs.revision`」这个 v0.2.1/v0.2.2 反复踩的硬伤，从「靠人记得」变成「机器强制」。

### 7.2 提交到 dbx-store

两条路径二选一：

**A. 自动同步（推荐）** —— 仓库已在 `t8y2/dbx-store` 的 `automation/plugin-sources.json` 注册且 `autoUpdate: true` 时，商店的同步 Workflow 每小时扫一次公开 Release，自动创建/更新 `candidates/<plugin-id>.json` 的 PR。作者无需手工写候选文件。

**B. 手工提 PR** —— fork `t8y2/dbx-store`，在**同一个 PR** 里提交：

- `publishers/vyrnax.json` —— **仅首次上架**，格式 `{"id":"vyrnax","name":"vyrnax","status":"unverified"}`（`verified` 由维护者改，作者必须留 `unverified`）
- `candidates/com.vyrnax.translation.json` —— 内容取自 CI 的 `dbx-store-candidate` artifact

`plugins/*.json` 与 `catalog/index.json` **永不手工编辑**，由签名 Workflow 生成。PR 模板里必须写清能力、权限、数据访问、**网络访问**与**原生侧车行为**（见 7.3）。

提交后 CI 会一直红着（`open candidate(s) awaiting DBX Store signing`），这是设计如此。维护者审核 → 跑受保护的签名 Workflow → 回写目录 → CI 变绿 → 合并。

### 7.3 审核必答题（本插件的披露口径）

| 审核项 | 本插件的答复 |
|---|---|
| **网络访问** | Manifest 只声明 `host.events`，**未声明** `host.network:*`。但 `host.network` 白名单**不约束原生侧车** —— 侧车会直连用户在引擎配置中填写的**任意 HTTP/HTTPS 端点**（除内置引擎外，8 类可配引擎的端点均可自定义）。唯一不由用户填写的端点是内置免费引擎的 `https://index-translate.bilibili.com/v1/chat/completions`（HTTPS，选中该引擎时原文发往它）。侧车**不监听任何端口**。默认严格校验 TLS；`skipTlsVerify` 是按引擎的显式开关，开启时 UI 红色警示 |
| **隐私** | 翻译原文会发往用户所选的第三方服务（含内置免费引擎的公益端点）。原文与译文以**明文 SQLite**（`<插件数据目录>/translation.db`）落盘，**卸载后仍保留**，可在「历史」页随时清空。API Key **不以明文落盘**（系统密钥库加密，降级时 UI 常驻警示）；密钥不进日志/事件/错误消息/RPC 返回值；导出默认不含密钥 |
| **原生侧车行为** | 1 个 Go 可执行文件（无 cgo），由宿主按生命周期启停，仅 stdio 通信。只写插件数据目录内的 `translation.db`(+`-wal`/`-shm`)、`config.json`、`sidecar-trace.log`。**`localcli` 引擎会执行本机命令**（`claude`/`codex`/自定义），用 `exec.CommandContext` 的**参数数组**直接 exec、**不经 shell**，`extraArgs` 逐条拒绝 shell 元字符，进程受超时控制后 kill；凭据由该 CLI 自持，插件不接触 |
| **代码签名** | 5 个 target 均为**未签名**候选（不含 `signature.json`）。`darwin` 侧车只有 Go 链接器附加的 **ad-hoc 签名，未做 Apple notarization**（需 Apple Developer 账号，暂无计划） |
| **provenance** | 全部 target 由对应平台 runner **真机构建**，源码检出自公开 tag，Go 工具链 pin 到 `1.21.13`（go.mod 声明 go 1.21，且该版本已被 v0.2.1 那次 CI 实证可构建全部 5 个 target）。`go version -m` 可见 `vcs.revision` = tag commit、`vcs.modified=false` |
| **许可** | Apache-2.0，仓库根目录含 `LICENSE` 全文，与候选文件的 `license` 一致 |

本地测试未签名包时，需在 DBX **插件中心**显式开启「允许安装未签名开发包」。

---

## 8. 用户手册（功能、安装、已知限制）

### 8.1 核心特性

**输入即译，流式输出** —— 在原文框打字，停顿后自动翻译，译文逐字出现。大模型引擎走 SSE 真流式，
传统机翻引擎自动降级为整段返回，前端体验完全一致。内置竞态防护：快速连续输入时旧请求的迟到结果
会被自动丢弃，不覆盖新译文；每次新输入前先取消上一个在飞任务，不浪费配额。

**9 类翻译引擎**

| 引擎 | 类型 | 流式 | 需要密钥 | 说明 |
|---|---|---|---|---|
| **免费翻译服务**（内置） | OpenAI 兼容 | ✅ | ❌ | 安装即存在、零配置，服务端为 B站公开部署的 Index-Translate 模型，国内直连。不可改名、不可删除，只能禁用或取消默认；公益端点不保证可用，翻译页有明确提示 |
| 大模型翻译 | OpenAI 兼容 | ✅ | ✅ | 覆盖 DeepSeek、通义千问、智谱、Kimi/Moonshot、OpenAI、Ollama、vLLM、LM Studio 等一切实现 `/v1/chat/completions` 的服务 |
| 本地 CLI 翻译 | 子进程代理 | ✅ | ❌ | 调用本机已登录的 `claude` / `codex`，复用现有登录态与订阅额度，无需申请 API Key |
| DeepL | REST | — | ✅ | 免费档 / 付费档、语气正式度、术语表 |
| Google 翻译 | REST | — | ✅ | Cloud Translation API v2 |
| 百度翻译 | REST | — | ✅ | 通用文本翻译，MD5 签名 |
| 有道翻译 | REST | — | ✅ | 有道智云，SHA256 签名 |
| 阿里云翻译 | REST | — | ✅ | 机器翻译 TranslateGeneral，ACS3-HMAC-SHA256 签名 |
| 腾讯云翻译 | REST | — | ✅ | TMT TextTranslate，TC3-HMAC-SHA256 签名 |

除内置引擎外，端点地址均可自定义，方便走企业代理或自建网关。

**内置免费引擎为什么存在** —— 此前的体验缺口是「装完必须先配好引擎才能用」，六个官方引擎都要申请凭据。
2026-10 实测 88 个免密钥端点后，只有 B站 Index-Translate 同时满足「免费 + 零配置 + 国内直连 + 质量可用」
（Google 系在国内是 TCP 层阻断，LibreTranslate / DeepLX / Lingva 公共实例已全面失效）。
它是公益享用端点、无 SLA，且服务端网关有 30 秒硬超时，所以单段上限被压到 400 字符；
选中它时翻译页会出现琥珀色提示与「去配置」入口，用户点过「不再提示」就写进偏好，不再重复打扰。

**引擎管理** —— 选类型自动补全可读默认名（「阿里云翻译」「DeepL 翻译」），手动改过之后切类型不会覆盖你的命名；
同类型可添加多个实例，靠名称区分。连通性测试返回耗时与可理解的中文错误（区分「Key 无效」「端点不可达」
「配额用尽」「限流」）。支持设为默认、启用/禁用、拖拽排序、删除（连带清除密钥）。

引擎名称有硬性长度上限（约 16 个汉字 / 32 个字母；全角字符按 2 个宽度单位计，上限 32 单位）：输入框实时限长，侧车 `engines/save` 再校验一次以挡住手工调用与旧备份导入。有了这个上限，下拉与列表里的名称必然放得下，不需要「省略号 + 悬浮看全称」的补救。

**顺序与默认是两件独立的事** —— 列表顺序只由你按住行首把手拖动决定，「设为默认」只改标记、不插队；翻译页的引擎下拉与引擎列表始终同序。

**31 种语言** —— `auto` 自动检测 + 30 种语言。语言下拉按当前引擎的能力**动态禁用**不支持的选项，
而不是等你选完了翻译失败。

**翻译历史** —— 分页、搜索（原文与译文）、收藏、单条/批量删除、一键用历史记录重新翻译。
默认保留 500 条（可调 100–5000），超出时淘汰最旧记录，**收藏的永不淘汰**。

**长文本自动分段** —— 超过引擎单次上限的文本按「段落 → 句子 → 子句 → 硬切」逐级降级切分，
并保护代码块、URL、`{placeholder}` 占位符不被切断。分段顺序执行、段间可配延时（默认 1100ms），
避免触发百度/有道免费档的 1 QPS 限制。

**完全跟随 DBX 外观** —— 主题（明暗）、调色板、圆角、UI 字体、编辑器字体、界面语言全部实时跟随
DBX 设置，改完立即生效，无需重开工作台。界面提供简体中文与 English 双语。

密钥安全模型见第 5 节。

### 8.2 安装

**推荐：官方商店**。本插件已上架 DBX 官方商店（`t8y2/dbx-store`），在 DBX **插件中心**搜索
「Translation 即时翻译」直接安装，由商店统一签名。

**自测：未签名本地包**。打开 **插件中心 → 设置**，开启「允许安装未签名开发包」，再安装
`dist/com.vyrnax.translation-<version>-<target>.dbxp`。target 取值：
`windows-x64` / `darwin-arm64` / `darwin-x64` / `linux-x64` / `linux-arm64`。
该开关不会放宽官方商店的安装校验。

安装后从**工具栏图标**、**命令面板**或**插件中心**打开工作台。

### 8.3 快速开始

1. 打开插件 → 切到「引擎」页 → 点「添加引擎」
2. 选择引擎类型，名称会自动填好
3. 填入端点与 API Key（本地 CLI 引擎只需确认命令已在 PATH 中，**无需密钥**）
4. 点「测试连接」确认可用 → 保存
5. 回到「翻译」页，选目标语言，直接在原文框打字

`Ctrl/Cmd + Enter` 手动触发翻译，`Esc` 取消进行中的翻译。

### 8.4 已知限制（宿主能力边界）

| 限制 | 原因 |
|---|---|
| 不支持编辑器划词翻译 | DBX 插件系统没有编辑器选区 API，`context-menu` 仅支持连接与表节点 |
| 不支持译文内联提示 | 插件 UI 运行在 iframe 沙箱内，无法覆盖宿主 DOM |
| 不支持自定义快捷键 | DBX 没有提供 keybinding 贡献点 |
| 浮动窗口「复制即译」尚未实现 | 需 `host.workbench` + `host.clipboard:read`；宿主能力已核实（见 `AGENTS.md` §1.2），属后续版本 |
| 表/字段注释批量翻译尚未实现 | 需 `host.data:read`（Host API 1.4），会抬高最低宿主版本，单独评估 |

### 8.5 排查

打开插件右上角「⋯ → 查看诊断报告」，一键复制后提交 Issue。报告含环境快照、侧车状态、
密钥保护方案、主题与语言的实际生效值，以及最近 150 条分步日志。**报告不含任何 API Key**
（自动脱敏），可以放心公开粘贴。

侧车日志在插件数据目录下的 `sidecar-trace.log`（Windows 通常在
`%APPDATA%\com.dbx.app\plugin-data\com.vyrnax.translation\`）。

---

## 9. 许可

Apache-2.0
