# Translation 即时翻译（DBX 插件）

多引擎翻译插件，运行在 [DBX](https://dbxio.com) 宿主里。支持 **OpenAI 兼容大模型**（DeepSeek、通义千问、智谱、Kimi、Ollama、vLLM、LM Studio…）、**DeepL**、**百度**、**有道**、**阿里云**与**本地 CLI 代理**（claude / codex），含流式输出、输入即译、引擎管理与翻译历史。

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
├── manifest.json          # 插件清单（id / 版本 / entrypoints / contributions / 权限）
├── dbx-plugin.toml        # 打包配置（后端语言、二进制名、include）
├── .dbx-store.json        # 商店元数据（name/permissions 必须与 manifest 一致）
├── assets/plugin.svg      # 图标
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
│   │   └── openai/        # OpenAI 兼容适配器（SSE 流式）
│   ├── secret/            # 密钥保护（DPAPI / Keychain / secret-tool / AES-GCM）
│   ├── store/             # SQLite 存储层（engines / secrets / history / prefs / meta）
│   ├── lang/              # 规范语言集 + 各引擎私有码映射
│   ├── chunk/             # 分段器（保护代码块 / URL / 占位符）
│   ├── trace/             # 脱敏文件日志
│   └── go.mod / go.sum
├── dist/                  # 构建产物（*.dbxp + artifact.json + release-candidates.json）
│   └── old/               # 历史版本归档（只挪不删）
├── _release.mjs           # 一键交叉编译 + 打包 + 校验 + 出 release-candidates.json
├── _buildpkg.js           # 单平台打包（zip + checksums + Unix 权限位 + 闸门）
├── _verify.mjs            # 单包校验（结构 / sha256 / 权限位 / 真跑侧车探针）
├── _e2e.mjs               # 侧车协议端到端测试（含假 OpenAI 端点，77 项断言）
├── _domcheck.py           # 前端静态闸门（DOM 悬空引用 / 关键元素 / 重复函数）
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

- Go 1.21+（本机 1.27.3 @ `C:\develop\Go`），`GOPROXY=https://goproxy.cn,direct`
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

1. 在本仓库创建版本 Tag 与 GitHub Release，包含各平台未签名 `.dbxp`、对应 `.artifact.json`、汇总的 `release-candidates.json`、Release Notes（**`.dbxp` 不进 Git 历史**）
2. `t8y2/dbx-store` 的同步 Workflow 读取最新 Release，自动创建或更新 `candidates/com.gaogao.translation.json` 候选 PR
3. 维护者审核源码 / Manifest / 权限 / SHA-256 / 大小 / 原生行为，通过后用仓库密钥加 Ed25519 签名
4. Workflow 回写 `plugins/<id>.json` 与 `catalog/index.json`，维护者合并

**插件上架 PR 提交到 `t8y2/dbx-store`，不是 `t8y2/dbx`。** 插件自身的功能代码留在本仓库。

本地测试未签名包时，需在 DBX **插件中心**显式开启「允许安装未签名开发包」。

---

## 8. 许可

Apache-2.0
