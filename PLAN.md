# DBX 翻译助手插件 —— 完整开发计划

> 本文件是项目的实施蓝图，与 `AGENTS.md`（强制开发规范）配套使用。
> 实施时必须同时遵守两者；`AGENTS.md` 的「铁律」优先级最高。

## 一、摘要

在 DBX 宿主内构建一个生产级多引擎翻译插件 `com.gaogao.translation`。前端为纯原生 JS 工作台（sandbox iframe），后端为 Go 原生侧车（JSON Lines over stdio），支持 6 类翻译引擎、流式输出、输入即译、引擎管理、翻译历史，API Key 由侧车自行加密保管。

**核心架构决策（已锁定）**：不使用 `connection-provider`，引擎配置与密钥全部自管；所有数据存 `DBX_PLUGIN_DATA_DIR`；密钥保护采用「平台密钥库优先 + 机器绑定 AES-GCM 降级」；引擎分三批交付。

**关键推论**：因选择自管配置，插件不依赖 Secret Store / clipboard / floating / queryData，开发宿主可覆盖约 85% 功能的自动化验证，仅 `command`/`menus` 入口与安装签名需桌面端手工验证。

---

## 二、已探明事实（计划的事实基础，不可推翻）

| 事实 | 来源 | 对设计的影响 |
|---|---|---|
| 开发宿主 CSP 为 `connect-src 'none'` | dev-runtime `sandboxDocument()` | 前端机制上无法发网络请求，侧车是唯一出口 |
| `child.stderr.resume()` | dev-runtime `Sidecar.start()` | stderr 被丢弃，日志必须写文件 |
| 侧车事件仅在声明 `host.events` 时转发 | dev-runtime `sidecar.on("event")` | 流式翻译**必须**声明 `host.events` |
| dev-runtime 仅实现 6 个桥接方法 | `bridge()` switch | `storage`/`clipboard`/`floating`/`queryData`/`saveFile`/`copy` 开发期抛 `Unsupported mock host method` |
| dev-runtime 仅识别 `connection-provider`+`workbench` | contribution 校验 | `command`/`menus` 开发期不生效，须桌面端验证 |
| `binding()` 默认：`password`→`secret`，其余→`config` | dev-runtime | 本项目不用 connection-provider，此规则不适用 |
| 桥接对象含 `stream()`，`closeMethod` 默认 `filesystem/stream/close` | `installBridge()` | 判定为文件流专用，**不依赖**，改用 `onEvent` |
| `protocolName` 正则 `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$` | dev-runtime + Go SDK | 自定义事件名 `translate/chunk` 合法 |
| 桥接 `invoke` 超时夹到 1–120000ms，默认 30000 | dev-runtime `bridge()` | 前端超时设置须在此区间 |
| 桥接参数 JSON 上限 2 MiB | `BRIDGE_LIMIT` | 大文本须分段或拒绝 |
| SDK 单条 JSON 上限 8 MB | `maxJSONBytes` | 侧车侧硬上限 |
| 诊断日志自动脱敏 `api.?key`/`token`/`secret` 等 | dev-runtime `redact()` | 宿主侧已有保护，但侧车自身仍须脱敏 |
| DPAPI 为纯 syscall 无 cgo | `golang.org/x/sys/windows` v0.49.0 | 密钥加密不破坏交叉编译 |
| 6 平台交叉编译实测通过 | 本机 Go 1.27.3 | 本地可出齐全部候选包 |
| DBX 不向插件暴露 AI 模型配置 | 官方文档「插件不会收到模型回复或模型配置」 | 复用已配置大模型**不可行**，改手填 + 本地 CLI 引擎 |
| `ai_configs` 表存在但当前记录 `apiKey`/`endpoint` 均空、provider 为 `*-cli` | 只读查表结构 | 印证 CLI 代理模式价值；**不直读该表** |
| 官方 Go SDK 在 plugin-cli 包内 | 本机 diff（411 行，与 dbx-todo vendor 版 diff=0） | 直接从 CLI 包复制 vendor |

---

## 三、功能范围

### 3.1 纳入范围

| 功能 | 说明 |
|---|---|
| 即时翻译 | 工作台内输入/粘贴文本，防抖后自动翻译，流式增量渲染译文 |
| 手动翻译 | 显式点击翻译，支持长文本分段 |
| 引擎管理 | 6 类引擎的增删改查、启用/禁用、设默认、排序 |
| 连通性测试 | 每个引擎可单独试译短词，返回耗时与可理解错误 |
| 密钥保管 | 平台密钥库加密，UI 仅显示掩码指纹，保存后不回传明文 |
| 翻译历史 | 分页、搜索、收藏、单条/批量删除、清空 |
| 语言对 | 30 种规范语言 + `auto`，按引擎能力动态禁用不支持项 |
| 数据导出/导入 | 引擎配置（可选含密钥）+ 历史 + 偏好 |
| 诊断报告 | 环境快照 + 分步日志 + 侧车状态，一键复制 |
| 主题/语言跟随 | 明暗双主题、zh-CN/en 双语 UI |

### 3.2 明确排除（宿主能力边界，不承诺）

| 排除项 | 原因 |
|---|---|
| 编辑器划词翻译 | 无选区 API；`context-menu` 仅支持 `connection`/`table` |
| 译文内联提示 | iframe 沙箱无法覆盖宿主 DOM |
| 自定义快捷键 | 无 keybinding 贡献点 |
| 复用 DBX 已配置大模型 | 官方明确不暴露模型配置 |
| 浮动窗口复制即译 | 本轮范围外（`floating` + `clipboard:read` 开发宿主不支持） |
| 表注释批量翻译 | 本轮范围外（需 `host.data:read`，抬高门槛至 Host API 1.4） |
| 术语表 / Prompt 市场 | 本轮范围外 |

---

## 四、技术架构

### 4.1 分层

```
DBX 宿主（桌面端 / 开发宿主）
 ├─ UI 工作台（sandbox iframe, opaque origin）
 │    index.html · app.js · engines.js · history.js · storage.js · styles.css
 │        │ invoke()  ↓↑  onEvent()
 └─ Go 侧车（独立进程，非 OS 沙箱，不受 CSP 约束）
      main.go        RPC 分发 + 生命周期
      dbxsdk/        vendor 的官方 SDK（禁改）
      engine/        Engine 接口 + 注册表
      engine/openai  engine/deepl  engine/baidu
      engine/youdao  engine/aliyun engine/localcli
      secret/        DPAPI / Keychain / secret-tool / AES-GCM
      store/         SQLite（modernc，无 cgo）
      lang/          规范语言 + 各引擎映射
      chunk/         分段器
      trace/         脱敏文件日志
```

### 4.2 技术选型

| 层 | 选型 | 理由 |
|---|---|---|
| 前端 | 原生 HTML/CSS/JS（ES5 风格 IIFE） | 无构建步骤 → 规避 Vite `base` 配错导致 chunk 404；产物即源码，静态闸门可直接 grep |
| 侧车 | Go 1.21+（`go.mod` 声明），本机 1.27.3 | 纯 Go 无 cgo → 本地交叉编译 6 平台 |
| SDK | vendor 官方 `dbx-plugin-sdk`（字节级复制） | 官方未发布到 Go module proxy，`go get` 失败 |
| 传输 | JSON Lines（**不用 framed**） | framed 是 Rust SDK 能力；manifest v1 不接受 `transport` 字段 |
| 存储 | SQLite via `modernc.org/sqlite` | 纯 Go 无 cgo；`mattn/go-sqlite3` 需 cgo 会破坏交叉编译 |
| HTTP | 标准库 `net/http` | 无第三方依赖，SSE 手写解析 |
| 加密 | `golang.org/x/sys/windows`（DPAPI）+ 标准库 `crypto/aes`+`crypto/cipher`（GCM） | DPAPI 纯 syscall 无 cgo |
| 打包 | Node.js 脚本，仅内置模块 | 无 `node_modules` |

### 4.3 权限与宿主门槛（最小化）

```json
"engines": { "dbx": ">=0.6.21", "host_api": "1" },
"permissions": ["host.events"]
```

| 权限 | 是否声明 | 理由 |
|---|---|---|
| `host.events` | 必须 | 流式翻译依赖侧车事件转发；不声明则前端收不到 chunk |
| `host.workbench` | 否 | `command` 的 `open-workbench` 由宿主处理；不用 `setTitle` |
| `host.storage` | 否 | 全部状态走侧车 SQLite，规避 256 KiB/1 MiB 限制 |
| `host.clipboard:read` | 否 | 本轮不做浮动窗口；粘贴走浏览器原生 paste 事件 |
| `host.data:read` | 否 | 本轮不做注释翻译；避免抬高至 Host API 1.4 |
| `host.ai` | **禁止** | 仅 >=0.6.20 存在，声明会让老宿主安装阶段直接拒装 |
| `host.network:*` | **禁止** | 静态白名单最多 8 个 origin，无法满足「用户自填端点」；网络全在侧车 |
| `host.binary` | 否 | 不用二进制通道 |

`engines.dbx >= 0.6.21` 是 `command`/`menus` 贡献点的硬性要求。

### 4.4 贡献点

```json
"contributions": [
  { "type": "workbench", "id": "com.gaogao.translation.main",
    "label": "翻译助手", "description": "多引擎翻译工作台", "icon": "assets/plugin.svg" },
  { "type": "command", "id": "com.gaogao.translation.cmd.main",
    "label": "翻译助手", "description": "打开翻译工作台", "icon": "assets/plugin.svg",
    "action": { "type": "open-workbench",
                "workbench": "com.gaogao.translation.main", "presentation": "tab" } },
  { "type": "menus", "id": "com.gaogao.translation.menus.main", "items": [
    { "location": "appToolbar", "command": "com.gaogao.translation.cmd.main",
      "group": "navigation", "order": 1, "default_visible": true },
    { "location": "commandPalette", "command": "com.gaogao.translation.cmd.main",
      "group": "navigation", "order": 1 } ] }
]
```

`localizations.en` 必须覆盖上述全部 3 个 id（宿主会把对不上的键判为无效清单）。
---

## 五、详细设计

### 5.1 侧车骨架与协议

**身份解析**：常量 `pluginID`/`pluginVersion` 仅作兜底；`resolveMetadata()` 从 `os.Executable()` 向上最多 6 层查找 `manifest.json`，校验 `id` 匹配后取其 `version`。避免版本号漂移导致 `Sidecar identity or protocol does not match manifest`。

**数据目录三级回退**：
```
DBX_PLUGIN_DATA_DIR → DBX_PLUGIN_SPACE → os.UserConfigDir()/dbx/plugins/<id> → ./data
```

**启动序列**：`MkdirAll(dataDir)` → `loadConfig()`（还原上次配置，解决「重启后数据消失」）→ `store.Open()`（SQLite + migrate，`busy_timeout(5000)`，失败重试一次）→ `trace` 记录启动信息 → `NewServer(metadata, handler).Serve()`。

**分发**：`Handle()` 用 `switch method` 平铺，`default` 返回 `MethodNotFound`。进入 switch 前统一 `absorbParams()` 递归吸收配置键变体。

**追踪日志**：`trace.Log(format, args...)` 写 `<dataDir>/sidecar-trace.log`，>64 KiB 自动截断。**内置脱敏**：对匹配 `(?i)(api.?key|secret|token|password|authorization)` 的键值替换为 `[REDACTED]`；对长度 >16 的疑似密钥字符串仅保留前 4 后 4。

**错误码**：

| Code | 含义 | 是否重试 |
|---|---|---|
| `-32601` | 方法不存在 | 否 |
| `-32602` | 参数非法 | 否 |
| `-32001` | 协议版本不匹配（SDK 自动） | 否 |
| `-32002` | 存储读写失败 | 是（一次） |
| `-32004` | 导出失败 | 否 |
| `-32006` | 导入失败 | 否 |
| `-32012` | 偏好写入失败 | 否 |
| `-32020` | rev 冲突 | 是（刷新 rev 后一次） |
| `-32030` | 引擎网络失败/超时 | 用户手动 |
| `-32031` | 引擎鉴权失败（401/403） | **否**（重试无意义且浪费配额） |
| `-32032` | 引擎限流（429） | 否，提示稍后 |
| `-32033` | 引擎响应格式非法 | 否，附截断原始片段 |
| `-32034` | 翻译被取消 | 否 |
| `-32035` | 密钥解密失败（密钥库不可用/机器变更） | 否，提示重新输入 |
| `-32036` | 文本超出引擎上限且无法分段 | 否 |
| `-32037` | 引擎未配置或已禁用 | 否 |

**业务失败 vs 协议失败**：可预期失败（如「端点不可达」）返回 `{ok:false, code, message}` 作为正常 result，让前端能展示可操作文案；不可恢复的才用 `PluginError`。

### 5.2 密钥保护（`secret/` 包）

**接口**：
```go
type Protector interface {
    Scheme() string                      // dpapi | keychain | secret-tool | aesgcm-machine
    Secure() bool                        // 是否达到「真加密」而非「仅混淆」
    Protect(plaintext []byte) (cipher string, err error)
    Unprotect(cipher string) ([]byte, error)
}
```

**平台实现**（构建标签隔离，保证交叉编译）：

| 平台 | 主方案 | 降级 |
|---|---|---|
| Windows | DPAPI `CryptProtectData`（`CRYPTPROTECT_UI_FORBIDDEN`，entropy = 插件 ID 派生） | 机器绑定 AES-GCM |
| macOS | `security add-generic-password` / `find-generic-password`（`exec.Command`，非 shell 拼接） | 机器绑定 AES-GCM |
| Linux | `secret-tool store/lookup`（libsecret CLI） | 机器绑定 AES-GCM |

**机器绑定降级方案**：密钥 = `HKDF-SHA256(machineID + pluginID + salt)`，AES-256-GCM 加密，nonce 随机前置。`machineID` 来源：Windows 注册表 `MachineGuid`、Linux `/etc/machine-id`、macOS `IOPlatformUUID`。**这是混淆而非加密**——能读磁盘的本地进程即可解密。

**强制 UI 披露**：`translate/ping` 返回 `{secretScheme, secretSecure}`；`secretSecure === false` 时前端在引擎管理页顶部常驻黄色警示条：「当前系统密钥库不可用，API Key 仅以机器绑定方式混淆存储，安全性降低」。

**密钥流转铁律**：
1. 前端 `engines/save` 传入明文 → 侧车立即加密 → **返回值只含 `fingerprint`（如 `sk-****abcd`），绝不回传明文**
2. `engines/list` / `engines/get` 只返回 `hasSecret` + `fingerprint`
3. 密钥明文仅存在于「加密前」和「构造 HTTP 请求头时」两个瞬间，不写入任何日志/事件/错误消息
4. `data/export` 默认**不含**密钥；含密钥需显式 `includeSecrets: true` 且导出文件头部写入明文警告
5. 解密失败返回 `-32035`，提示「密钥库不可用或机器已变更，请重新输入 API Key」，不静默清空

### 5.3 引擎抽象与适配器（`engine/` 包）

**统一接口**：
```go
type Request struct {
    Source, SourceLang, TargetLang string
    Glossary map[string]string     // 预留，本轮不用
    MaxChars int
}
type Chunk struct { Text string; Index, Total int }
type Result struct {
    Target string; DetectedLang string
    ElapsedMs int64; Chunks int; Engine, Model string
}
type Engine interface {
    Kind() string
    SupportsStreaming() bool
    MaxChars() int                              // 单次请求上限
    SupportedLangs() []string                   // 规范语言码子集
    Test(ctx context.Context) error             // 连通性 + 鉴权
    Translate(ctx context.Context, r Request) (Result, error)
    Stream(ctx context.Context, r Request, emit func(Chunk) error) (Result, error)
}
type Factory func(cfg Config, sec Secrets, opts Options) (Engine, error)
```

**注册表**：`engine.Register(kind, factory, schema)`。新增引擎 = 新增一个包 + 一行注册，**前端零改动**（表单由 `translate/schema` 动态渲染）。

**配置 Schema（单一事实来源在侧车）**：`translate/schema` 返回各引擎字段定义，结构对齐 DBX 表单语义（`key`/`label`/`type`/`required`/`default`/`options`/`secret`/`visible_when`/`description`/`placeholder`），前端通用渲染器据此生成表单。

**六类适配器要点**：

| kind | 端点 | 鉴权 | 流式 | 单次上限 | 签名/协议要点 |
|---|---|---|---|---|---|
| `openai` | 用户填完整 URL | `Authorization: Bearer` | 是（SSE） | 可配（默认 20000 字符） | `stream:true`，解析 `choices[0].delta.content`，`[DONE]` 结束 |
| `deepl` | `api-free.deepl.com` / `api.deepl.com` / 自定义 | `Authorization: DeepL-Auth-Key` | 否 | 128 KiB | `text`+`target_lang`+可选 `formality`/`glossary_id` |
| `baidu` | `fanyi-api.baidu.com/api/trans/vip/translate` | `sign` 参数 | 否 | 6000 字节 | `sign = MD5(appid + q + salt + secretKey)`，`salt` 随机 |
| `youdao` | `openapi.youdao.com/api` | `sign` 参数 | 否 | 5000 字符 | `signStr = appKey + truncate(q) + salt + curtime + appSecret`，`sign = SHA256(signStr)`；`truncate`：长度 >20 时取 `q[:10] + len(q) + q[-10:]`；`signType=v3` |
| `aliyun` | `mt.<region>.aliyuncs.com` | ACS3-HMAC-SHA256 | 否 | 5000 字符 | RPC 风格 `Action=TranslateGeneral`，`Version=2018-10-12`，规范请求 → 待签名串 → HMAC |
| `localcli` | 本地进程 | 无（CLI 自持凭据） | 是（行缓冲） | 可配（默认 20000） | `claude -p <prompt> --output-format stream-json` / `codex exec <prompt>`；**`exec.Command` 参数数组，绝不 shell 拼接** |

**`localcli` 安全约束**：
- `cliPath` 留空则从 `PATH` 查找；填写则校验为存在的可执行文件
- `extraArgs` 按行分割为参数数组，逐条校验不含 shell 元字符
- 工作目录可配，默认临时目录
- 超时默认 120s（CLI 冷启动慢）
- 输出按行缓冲 emit，给出流式观感

**LLM 引擎 Prompt 模板**（`openai`/`localcli` 共用，可按引擎覆盖）：
```
系统：你是专业翻译引擎。将用户文本翻译为{targetLang}。
只输出译文，不要解释、不要引号、不要添加任何额外内容。
保留原文的换行、缩进、Markdown 标记与 {placeholder} 占位符。
代码块内容保持原样不翻译。
```
`{targetLang}` 由规范语言码映射为自然语言名（如 `zh` → `简体中文`）。

**并发与限流**：
- 每引擎一个信号量（默认并发 1，可配 1–4），尊重上游 QPS（百度/有道免费档 1 QPS）
- 全局在飞任务上限 4，超出返回 `-32032` 提示稍后
- 所有请求经 `context.WithTimeout`，取消立即中断 HTTP body 读取

**TLS**：默认严格校验。`skipTlsVerify` 为按引擎显式开关，开启时 UI 红色警示 + 追踪日志记录（不含密钥）。仅 HTTPS 或 localhost 允许；非 HTTPS 且非 localhost 时保存即警告。
### 5.4 流式与即时翻译

**通道选择**：使用 `dbxPlugin.onEvent()` + 自定义事件，**不使用 `stream()`**（后者 `closeMethod` 默认 `filesystem/stream/close`，为文件流设计，桌面端对非文件方法行为无文档保证）。

**事件契约**（侧车 → 前端，需 `host.events`）：
```
translate/chunk  { taskId, text, index, total }
translate/end    { taskId, target, elapsedMs, engine, detectedLang, chunks }
translate/error  { taskId, code, message }
```

**RPC**：
```
translate/start   { engineId?, text, sourceLang, targetLang, stream? }
                  → { ok, taskId }        # 立即返回，随后发事件
translate/cancel  { taskId }              → { ok, taskId }
translate/run     { engineId?, text, sourceLang, targetLang }
                  → { ok, target, elapsedMs, engine, detectedLang, chunks }   # 非流式
```

**任务生命周期**：
1. `translate/start` 生成单调递增 `taskId`（`atomic.AddUint64`），注册到 `tasks` map（mutex 保护），创建 `context.WithCancel`
2. 立即返回 `{ok, taskId}`，goroutine 异步执行
3. goroutine 内：分段 → 逐段调 `engine.Stream()` → 每 chunk 经 `emitter.Event("translate/chunk", ...)` 发出
4. 完成发 `translate/end`，失败发 `translate/error`，**两者都从 map 删除任务**（防泄漏）
5. `translate/cancel` 调 `cancel()`，goroutine 检测 `ctx.Err()` 后发 `translate/error{code:-32034}` 并清理

**竞态防护（即时翻译核心）**：前端维护 `currentTaskId`。收到事件时**若 `taskId !== currentTaskId` 则丢弃**——解决「快速输入时旧响应晚到覆盖新结果」。新翻译启动前先 `translate/cancel` 旧任务。

**即时翻译参数**（用户可配，存 prefs）：
- `instantEnabled`（默认 true）
- `instantDebounceMs`（默认 600，范围 200–3000）
- `instantMinChars`（默认 2，低于此长度不触发）
- `instantMaxLength`（默认 2000，超出则不自动翻译，提示手动点击）

**降级**：引擎 `SupportsStreaming() === false`（DeepL/百度/有道/阿里）时，`translate/start` 内部走 `Translate()` 并把完整结果作为单个 chunk 发出，再发 `end`——**前端代码路径统一，无需分支**。

### 5.5 语言映射与分段

**规范语言集**（30 种 + `auto`）：
`auto, zh, zh-TW, en, ja, ko, fr, de, es, ru, pt, it, ar, th, vi, id, ms, tr, pl, nl, sv, da, fi, no, cs, el, he, hi, uk, ro, hu`

**`lang/` 包**：
- `Canonical` 列表（码 + 中文名 + 英文名 + LLM 自然语言名）
- 每引擎映射表 `map[kind]map[canonical]engineCode`
- `Map(kind, canonical) (code string, ok bool)`；`ok === false` → 该引擎不支持此语言
- `SupportedLangs(kind)` 供 schema 与 UI 动态禁用下拉项

各引擎码系差异示例：DeepL 用 `ZH`/`EN-US`（大写含地区）；百度用 `zh`/`jp`/`kor`/`cht`；有道用 `zh-CHS`/`en`；阿里用 `zh`/`en`。映射表集中管理，避免散落。

**`chunk/` 包**：
- `Split(text string, maxChars int) ([]string, error)`
- 分割优先级：段落（`\n\n`）→ 句子（`。！？.!?\n`）→ 子句（`，,;；`）→ 硬切
- 保护不可分割单元：代码块 ``` 围栏内、URL、`{placeholder}`
- 单句仍超 `maxChars` → 硬切并在结果中标记 `truncatedSentence`
- 分段**顺序执行**（保序 + 避免触发限流），结果按序拼接
- 超过引擎上限且无法分段（如单个超长 token）→ `-32036`

### 5.6 存储层（`store/` 包）

**位置**：`<dataDir>/translation.db`（SQLite，`modernc.org/sqlite`，`SetMaxOpenConns(1)`，`busy_timeout(5000)`）。

**表结构**：
```sql
CREATE TABLE meta    (key TEXT PRIMARY KEY, value TEXT);          -- rev / schema_version
CREATE TABLE engines (id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL,
                      config_json TEXT NOT NULL, has_secret INTEGER NOT NULL DEFAULT 0,
                      fingerprint TEXT, is_default INTEGER NOT NULL DEFAULT 0,
                      enabled INTEGER NOT NULL DEFAULT 1, ord INTEGER NOT NULL DEFAULT 0,
                      created_at TEXT, updated_at TEXT);
CREATE TABLE secrets (engine_id TEXT PRIMARY KEY, cipher TEXT NOT NULL,
                      scheme TEXT NOT NULL, created_at TEXT, updated_at TEXT);
CREATE TABLE history (id TEXT PRIMARY KEY, engine_id TEXT, engine_name TEXT, engine_kind TEXT,
                      src_lang TEXT, dst_lang TEXT, source TEXT NOT NULL, target TEXT NOT NULL,
                      chars INTEGER, elapsed_ms INTEGER, chunks INTEGER,
                      starred INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE prefs   (key TEXT PRIMARY KEY, value TEXT);
CREATE INDEX idx_history_created ON history(created_at DESC);
CREATE INDEX idx_history_starred ON history(starred);
```

**迁移**：全部 `CREATE TABLE IF NOT EXISTS` + 幂等 `ALTER TABLE ADD COLUMN`（忽略 duplicate column 错误）。`meta` 存 `schema_version` 供未来迁移分支。

**rev 乐观锁**：`engines/*` 与 `prefs/set` 的写操作校验前端回声的 `rev`；不匹配返回 `{conflict:true, newRev}` + `-32020`，**绝不覆盖**。前端刷新 rev 后重试一次。`history` 为追加型，不参与 rev。

**整体替换在单事务内** `DELETE` + `INSERT`，避免半份快照。

**历史保留策略**：`prefs.historyLimit`（默认 500，范围 100–5000）；写入后超限则删除最旧的非收藏记录。

**配置持久化**：`prefs` 表存 UI 偏好；`config.json`（dataDir 下）存 dataDir 自身路径以便重启还原（对齐 dbx-todo 的 `loadConfigDir` 教训）。

### 5.7 前端

**文件划分**：

| 文件 | 职责 |
|---|---|
| `index.html` | 结构骨架，全部 id 静态声明 |
| `storage.js` | 桥接层：`invoke`/`onEvent` 封装、超时、诊断、看门狗、rev 管理 |
| `engines.js` | 引擎管理页：schema 驱动的动态表单、密钥掩码、测试连通性 |
| `history.js` | 历史页：分页、搜索、收藏、删除 |
| `app.js` | 主翻译页、视图切换、即时翻译防抖、流式渲染、事件绑定 |
| `styles.css` | 明暗双主题 CSS 变量 |

**桥接层强制模式**（对齐 AGENTS.md 铁律 5/6/7）：
```js
// API 永远是 window.dbxPlugin 本身；ready 只用于等待 init
await window.dbxPlugin.ready;
// 侧车走 invoke，宿主走 request，绝不混用
window.dbxPlugin.invoke(method, params, { timeoutMs });
// 事件监听（流式）
const off = window.dbxPlugin.onEvent(function (m) {
  if (m.method === "translate/chunk") { /* ... */ }
});
```

**安全 DOM 助手（强制）**：`$()` / `val(id)` / `setVal(id,v)` / `click(id,fn)` / `on(id,ev,fn)` / `h(tag,props,children)`。**禁止 `$("字面量").属性`**——静态闸门会拒绝打包。

**超时配置**（须在宿主 1–120000ms 区间内）：
```js
RPC_TIMEOUT=30000, READY_TIMEOUT=8000, CTX_TIMEOUT=2500,
PING_TIMEOUT=6000, BRIDGE_TIMEOUT=8000, TEST_TIMEOUT=30000
```

**启动序列**：`waitBridge(8000)` 轮询 25ms 等注入 → `waitReady` → `readContext`（同步 `dbxPlugin.context` 优先，`request("host.getContext")` 兜底）→ `invoke("translate/ping")` 握手 → 成功则 `sidecarBackend`，失败则 `failHard()` + 会话内存后端（**UI 明确标注「未持久化」，绝不静默退化**）→ 注册 `onEvent` → 加载引擎列表与偏好 → 首屏渲染。

**诊断**：`note(step, ok, detail)` 记录每步（上限 150 条），`report()` 输出环境快照（`origin`/是否 sandbox/`typeof dbxPlugin`/`invoke`/`request`/`ready` 类型/`Object.keys` 前 30/`readyState`）+ 状态 + 分步日志。菜单提供「查看诊断报告」按钮，一键 `dbxPlugin.copy()` 复制。

**看门狗**：启动 15s 内未完成则在 UI 显示「卡在哪一步」。

**页面级错误兜底**：`window.addEventListener("error")` + `unhandledrejection`；所有事件回调包 `try/catch` → `reportErr()` → toast。

**主题**：自带完整明暗两套 CSS 变量；`applyTheme()` 从 `dbxPlugin.theme` 读取（**兼容属性与函数两种形态**），设 `document.body.dataset.theme`；监听 `dbx-plugin-env` 事件实时刷新。不依赖父页面 CSS 穿透。

**i18n**：UI 文本自建字典（`zh-CN`/`en`），从 `dbxPlugin.locale` 初始化 + `dbx-plugin-env` 刷新。`localizations` 只覆盖 manifest 层文案，**不会翻译 UI 文本**。

**流式渲染**：译文区增量 append chunk；自动滚动到底（除非用户手动上滚）；显示实时字符数与耗时；取消按钮在飞行中可见。

**`[hidden]` 显式 CSS**：`.menu[hidden] { display: none; }`（`display:flex` 会顶掉默认隐藏）。

**导出**：sandbox 内 blob 下载被宿主无声取消 → 侧车返回 base64，前端经 `host.saveFile` 弹原生保存对话框；**开发宿主不支持 `saveFile`**，须 `hasHostSave()` 特性检测，缺失时降级为「复制到剪贴板」并提示。base64 传输守 `MAX_UPSTREAM_BYTES = 1.5 MB`。

### 5.8 RPC 全表

| 方法 | 入参 | 返回 |
|---|---|---|
| `translate/ping` | — | `{ok, plugin, version, dataDir, secretScheme, secretSecure, dbOk, inflight}` |
| `translate/schema` | — | `{kinds:[{kind,label,description,fields:[...],streaming,maxChars,languages:[]}], languages:[{code,zh,en,llm}]}` |
| `engines/list` | — | `{engines:[{id,name,kind,isDefault,enabled,hasSecret,fingerprint,config,ord}], rev}` |
| `engines/get` | `{id}` | `{engine, rev}`（不含密钥明文） |
| `engines/save` | `{id?, name, kind, config, secrets?, ord?}` | `{ok, id, rev}`（返回 fingerprint，不返回明文） |
| `engines/delete` | `{id, rev}` | `{ok, rev}` |
| `engines/setDefault` | `{id, rev}` | `{ok, rev}` |
| `engines/toggle` | `{id, enabled, rev}` | `{ok, rev}` |
| `engines/reorder` | `{ids:[], rev}` | `{ok, rev}` |
| `engines/test` | `{id}` 或 `{kind, config, secrets}` | `{ok, message, elapsedMs, detail}` |
| `translate/run` | `{engineId?, text, sourceLang, targetLang}` | `{ok, target, elapsedMs, engine, detectedLang, chunks}` |
| `translate/start` | `{engineId?, text, sourceLang, targetLang, stream?}` | `{ok, taskId}` |
| `translate/cancel` | `{taskId}` | `{ok, taskId}` |
| `history/list` | `{limit?, offset?, q?, starred?}` | `{items, total}` |
| `history/star` | `{id, starred}` | `{ok}` |
| `history/delete` | `{ids:[]}` | `{ok, deleted}` |
| `history/clear` | `{keepStarred?}` | `{ok, deleted}` |
| `prefs/get` | — | `{prefs}` |
| `prefs/set` | `{prefs, rev}` | `{ok, prefs, rev}` |
| `data/export` | `{includeSecrets?}` | `{ok, fileName, dataBase64, bytes, counts}` |
| `data/import` | `{dataBase64, mode}` | `{ok, counts, rev}` |

**事件**（需 `host.events`）：`translate/chunk`、`translate/end`、`translate/error`。
---

## 六、开发演进路线

### 阶段 0：地基与工具链

**目标**：空壳插件在开发宿主与桌面端都能打开，打包/校验流水线可用。

**步骤**：
1. 写 `.gitignore`（AGENTS.md §10.1 全文，含 `.dbx-dev/`、`_gocache/`、`_gotmp/`、`_xbuild/`、`dist/`、`*.exe`、`node_modules/`、`.idea`、`.vscode`）
2. vendor SDK：从 `C:\Users\Lenovo\AppData\Roaming\npm\node_modules\@dbx-app\plugin-cli\sdk-root\plugins\sdk\go\dbx-plugin-sdk\sdk.go` **字节级复制**到 `backend/dbxsdk/sdk.go`，写 `backend/dbxsdk/VENDOR.md` 说明来源/为何 vendor/传输方式/禁改
3. `backend/go.mod`：`module github.com/gaogao/translation`，`go 1.21`，依赖 `modernc.org/sqlite` + `golang.org/x/sys`
4. `manifest.json`（§4.3/§4.4 全文，含 `localizations.en` 覆盖全部 3 个贡献点 id）
5. `dbx-plugin.toml`：`schema_version=1`，`[backend] language="go" directory="backend" binary="dbx-plugin-translation"`，`[package] include=["assets","ui"]`
6. `.dbx-store.json`：`name`/`permissions` 必须与 manifest 一致
7. `assets/plugin.svg`：翻译主题图标（SVG，128x128 viewBox）
8. `backend/main.go` 最小骨架：`resolveMetadata()` / `dataDir()` / `trace` / `Handle()` 仅含 `translate/ping`
9. `ui/` 最小骨架：`index.html` + `storage.js`（桥接 + 诊断）+ `app.js`（握手 + 显示 ping 结果）+ `styles.css`（明暗主题）
10. 工具链脚本：`_testutil.mjs`（异步 spawn）、`_buildpkg.js`、`_verify.mjs`、`_domcheck.py`、`_release.mjs`
11. `README.md` 骨架（架构图 + RPC 表 + 构建说明）
12. 首次 git 提交

**验收**：
- `python _domcheck.py` 通过
- `go build -C backend` 通过；`gofmt -l backend` 无输出
- `node _release.mjs windows-x64` 产出 `.dbxp`，`node _verify.mjs` 全 PASS（含真跑侧车探针 `plugin/initialize` + `translate/ping`）
- `dbx-plugin dev --port 5190` 打开后 UI 显示侧车版本与 dataDir
- `sidecar-trace.log` 有启动记录

### 阶段 1：核心翻译链路 → 0.1.0

**目标**：OpenAI 兼容引擎全链路可用，含流式、即时翻译、引擎管理、密钥加密、翻译历史。

**步骤**：
1. `backend/store/`：SQLite 打开/迁移/`engines`/`secrets`/`history`/`prefs`/`meta` CRUD + rev 乐观锁 + 单事务整体替换
2. `backend/secret/`：`Protector` 接口 + Windows DPAPI（构建标签 `windows`）+ macOS `security`（`darwin`）+ Linux `secret-tool`（`linux`）+ 机器绑定 AES-GCM 降级（全平台）+ 脱敏 fingerprint 生成
3. `backend/lang/`：规范语言集（30+auto）+ `openai` 映射 + `Map`/`SupportedLangs`
4. `backend/chunk/`：分段器（段落→句子→子句→硬切，保护代码块/URL/占位符）
5. `backend/engine/`：`Engine` 接口 + 注册表 + `Config`/`Secrets`/`Options` + schema 定义结构
6. `backend/engine/openai/`：SSE 解析器（`data:` 行、`[DONE]`、多行 data 合并、心跳注释行忽略）+ `Translate` + `Stream` + `Test` + 错误分类（401/403→`-32031`，429→`-32032`，超时/连接→`-32030`，非法 JSON→`-32033`）
7. `backend/trace/`：脱敏日志（键名正则 + 长串掩码 + 64 KiB 截断）
8. `backend/main.go`：`translate/schema`、`engines/*` 全套、`translate/run`、`translate/start`/`cancel` + 任务表（mutex + `atomic` taskId + context 取消 + 完成即清理）+ `history/*`、`prefs/*`、`data/export`、`data/import`
9. 前端 `ui/engines.js`：schema 驱动动态表单（含 `visible_when` 求值、`secret` 字段掩码显示、保存后只显示 fingerprint）、引擎列表（默认标记/启用开关/排序/删除确认对话框）、测试连通性按钮（显示耗时与错误）
10. 前端 `ui/app.js`：主翻译页（源/目标语言下拉按引擎能力禁用、源文本区、译文流式区、字符数/耗时、取消按钮）、即时翻译（防抖 + 最小长度 + 启动前取消旧任务 + `taskId` 竞态过滤）
11. 前端 `ui/history.js`：分页列表、搜索、收藏、删除、清空
12. 前端 `ui/storage.js`：`onEvent` 订阅与分发、rev 管理与冲突重试、`secretSecure===false` 警示条
13. Go 单元测试（见 §7.1）
14. 版本号三处同步至 `0.1.0`（`manifest.json` / `backend/main.go` 的 `pluginVersion` / `ui/storage.js` 的 `UI_VERSION`），git 提交并打 tag

**验收**：
- 全部 Go 单元测试通过；`httptest` 模拟的 OpenAI 端点上流式/非流式/取消/超时/401/429/非法 SSE 六类路径均按预期返回对应错误码
- 侧车协议 e2e 脚本通过：`initialize → ping → schema → engines/save → engines/test → translate/run → translate/start(+chunks) → translate/cancel → history/list`
- 开发宿主手工验证：新建 OpenAI 兼容引擎 → 测试连通 → 输入即译流式出字 → 快速连续输入无旧结果覆盖 → 取消立即停止 → 历史正确记录 → 重启侧车后引擎与历史仍在
- **密钥安全验证**：`sidecar-trace.log` 全文 grep 无密钥明文；`engines/list` 返回无明文；保存后前端拿不到明文；`translation.db` 中 `secrets.cipher` 非明文
- `_verify.mjs` 全 PASS；`node _release.mjs` 出 6 平台包

### 阶段 2：引擎扩展 → 0.2.0

**目标**：补齐 DeepL 与本地 CLI 代理，验证「新增引擎零前端改动」的架构自洽性。

**步骤**：
1. `backend/lang/`：DeepL 语言映射（大写含地区码，`EN`→`EN-US` 等）
2. `backend/engine/deepl/`：REST 调用、`free`/`pro`/自定义端点、`formality`、`glossary_id`、`Translate`（无流式，走单 chunk 降级）、`Test`、错误分类（456 配额耗尽→`-32032`，403→`-32031`）
3. `backend/lang/`：`localcli` 映射（复用 LLM 自然语言名）
4. `backend/engine/localcli/`：`claude`/`codex`/`custom` 三种；`exec.CommandContext` 参数数组；`claude --output-format stream-json` 逐行 JSON 解析提取 assistant 文本增量；`codex exec` 行缓冲；stdout/stderr 分离（stderr 进 trace，不进译文）；进程超时 kill；`cliPath` 存在性校验；`extraArgs` 逐条校验
5. `translate/schema` 自动包含新引擎（验证注册表机制）
6. 前端：验证动态表单无需改动即可渲染新引擎字段
7. Go 单元测试：DeepL 用 `httptest`；`localcli` 用假 CLI 脚本（测试参数数组构造、流式解析、超时 kill、非零退出码处理）
8. 版本号三处同步至 `0.2.0`，提交打 tag

**验收**：
- DeepL 端点选择与参数透传正确；配额耗尽返回 `-32032` 且文案可理解
- `localcli` 用本机真实 `claude`/`codex` 各试译一次通过，流式逐行出字
- **shell 注入测试**：`extraArgs` 填入 `; rm -rf /` 与 `$(whoami)` 等，确认作为字面参数传递、不被解释
- 前端零改动即渲染出新引擎表单（架构自洽性验证）
- 全部既有测试仍通过；`_verify.mjs` 全 PASS

### 阶段 3：传统机翻三引擎 → 0.3.0

**目标**：百度/有道/阿里三套签名算法正确实现，分段器在低上限引擎上生效。

**步骤**：
1. `backend/lang/`：百度（`zh`/`jp`/`kor`/`cht`/`wyw`/`yue`…）、有道（`zh-CHS`/`en`…）、阿里（`zh`/`en`…）三套映射
2. `backend/engine/baidu/`：`sign = MD5(appid + q + salt + secretKey)`，`salt` 随机数，表单编码 POST，响应 `error_code` 映射（52003 未授权→`-32031`，54003 限流→`-32032`，58000 签名错误→`-32031`）
3. `backend/engine/youdao/`：`curtime` 秒级时间戳，`truncate(q)` 规则（>20 时 `q[:10]+len(q)+q[-10:]`，**按 `[]rune` 字符数而非字节数**），`sign = SHA256(appKey+truncated+salt+curtime+appSecret)` hex，`signType=v3`，错误码映射（108 无效 Key→`-32031`，109 批量频率限制→`-32032`，110 签名无效→`-32031`）
4. `backend/engine/aliyun/`：ACS3-HMAC-SHA256 签名 V3——构造规范请求（`HTTPRequestMethod`/`CanonicalURI`/`CanonicalQueryString`/`CanonicalHeaders`/`SignedHeaders`/`HashedRequestPayload`）→ 待签名串 → HMAC → `Authorization` 头；`Action=TranslateGeneral`、`Version=2018-10-12`、`FormatType`/`SourceLanguage`/`TargetLanguage`/`SourceText`/`Scene`；错误码映射（`InvalidAccessKeyId.NotFound`/`SignatureDoesNotMatch`→`-32031`，`Throttling.User`→`-32032`）
5. `backend/chunk/`：在 5000/6000 字符上限下验证分段正确性；分段顺序执行 + 结果按序拼接 + 段间延时（尊重 1 QPS 免费档，`prefs.chunkDelayMs` 默认 1100）
6. 每引擎并发信号量默认 1
7. Go 单元测试：**用官方文档公布的签名示例做已知向量断言**（百度/有道/阿里各至少一组），确保签名算法字节级正确；`httptest` 验证请求体与错误码映射
8. 版本号三处同步至 `0.3.0`，提交打 tag

**验收**：
- 三引擎签名已知向量测试全部通过（这是签名正确性的唯一可靠证明）
- 超长文本（>5000 字符）自动分段、顺序正确、无丢字、段间延时生效不触发限流
- 错误凭据 → `-32031` 且**不重试**；触发限流 → `-32032` 且文案提示稍后
- 全部既有测试仍通过

### 阶段 4：生产化加固与上架 → 1.0.0

**目标**：达到健壮性/可维护性/鲁棒性/安全性生产标准，完成官方商店上架准备。

**健壮性**：
1. 侧车崩溃恢复：前端 ping 失败 → `failHard()` + 会话内存后端 + UI 明确「未持久化」+ 自动重连探测（指数退避，上限 5 次）
2. 侧车重启后状态还原：`loadConfig()` 生效，引擎/历史/偏好完整
3. SQLite 瞬态锁：`busy_timeout(5000)` + migrate 重试一次（间隔 150ms）+ 错误带语句序号与库路径
4. 全部外部调用 `context.WithTimeout`；HTTP body 读取也受 ctx 控制（防慢速上游挂死）
5. goroutine 泄漏防护：任务完成/失败/取消三条路径都从 map 删除；`translate/ping` 返回在飞任务数供诊断
6. 大文本防护：桥接 2 MiB 前置检查 + SDK 8 MB 硬上限 + 超限明确报错
7. 原子写：临时文件 + rename

**鲁棒性**：
8. 上游异常响应全覆盖测试：空 body、非 JSON、截断 SSE、超长单行、HTTP 5xx、连接重置、DNS 失败、TLS 失败、重定向
9. 非法输入全覆盖：空文本、纯空白、超长单行、含 NUL 字节、含控制字符、非法语言码、不存在的 engineId、已禁用引擎
10. 并发压力：20 个并发 `translate/start` + 随机 cancel，验证无泄漏无死锁（`-race` 跑测试）
11. 前端竞态：快速连续输入 50 次，验证只有最后一次结果被渲染

**安全性**：
12. 密钥泄露审计：自动化脚本 grep `sidecar-trace.log`、全部 RPC 返回值、`translation.db` 明文列、前端诊断报告，确认无密钥
13. `data/export` 默认不含密钥；`includeSecrets:true` 时导出文件首行写入明文警告
14. `skipTlsVerify` 开启时 UI 红色警示 + trace 记录
15. 非 HTTPS 且非 localhost 端点保存时警告
16. `localcli` shell 注入测试（阶段 2 已做，此处回归）
17. 译文按纯文本渲染，**绝不 `innerHTML`**（防上游返回内容 XSS）
18. 权限闸门固化进 `_buildpkg.js`：声明 `host.ai` 或 `host.network:*` 直接拒绝打包

**可维护性**：
19. `README.md` 完整化：架构图、目录结构、RPC 全表、事件契约、错误码表、构建/打包/发布流程、开发调试、验收清单
20. `AGENTS.md` 回填本轮新踩的坑（症状→根因→对策格式）
21. 代码注释审查：所有非显然逻辑必须有「为什么」注释
22. 版本号三处同步至 `1.0.0`
23. `_verify.mjs` 校验项补全至 AGENTS.md §8.1 全清单 + 本项目 §8.4 专属项

**i18n 与主题**：
24. zh-CN/en 双语全覆盖审查（不依赖宿主翻译 UI 文本）
25. 明暗主题全覆盖审查（含新增的警示条、错误态、流式光标）

**上架准备**：
26. `node _release.mjs` 出 6 平台候选包 + `release-candidates.json`
27. GitHub Release：候选包 + `.artifact.json` + `release-candidates.json` + Release Notes + 源码 Tag（**`.dbxp` 不进 Git 历史**）
28. 按 `t8y2/dbx-store` 贡献指南提交首次上架信息（`publisher` + 候选信息）
29. 等待同步 Workflow 创建 `candidates/com.gaogao.translation.json` 候选 PR
30. 配合维护者审核（源码/Manifest/权限/SHA-256/大小/原生行为），签名后合并

**验收**：全部自动化测试通过；`-race` 无告警；密钥审计零泄露；6 平台包 `_verify.mjs` 全 PASS（本机平台真跑探针，其余标 SKIP 并注明需真机冒烟）；桌面端完整手工验收清单通过。
---

## 七、测试计划

### 7.1 Go 单元测试（`backend/**/*_test.go`）

| 包 | 测试要点 |
|---|---|
| `engine/openai` | `httptest` 模拟：正常流式（多 chunk）、`[DONE]` 终止、多行 `data:` 合并、注释心跳行忽略、非法 JSON→`-32033`、401/403→`-32031`、429→`-32032`、超时→`-32030`、ctx 取消→`-32034`、空 body、HTTP 5xx |
| `engine/deepl` | free/pro 端点选择、`formality`/`glossary_id` 透传、456 配额→`-32032`、403→`-32031` |
| `engine/baidu` | **MD5 签名已知向量**、`salt` 随机性、`error_code` 全映射、表单编码正确性 |
| `engine/youdao` | **SHA256 签名已知向量**、`truncate` 边界（长度 20/21、多字节字符按字符数计）、`curtime` 格式、错误码全映射 |
| `engine/aliyun` | **ACS3-HMAC-SHA256 签名已知向量**、规范请求构造（头排序、URL 编码、payload hash）、错误码全映射 |
| `engine/localcli` | 假 CLI 脚本：参数数组构造正确、`stream-json` 逐行解析、超时 kill、非零退出码、stderr 不混入译文、**shell 元字符作为字面量传递** |
| `secret` | 各平台 Protector 加解密往返、降级路径、fingerprint 掩码正确、解密失败→`-32035`、**cipher 不含明文子串** |
| `chunk` | 段落/句子/子句/硬切优先级、代码块保护、URL 保护、占位符保护、单句超限、空文本、恰好等于上限 |
| `lang` | 各引擎映射正确、不支持语言返回 `ok=false`、`auto` 全引擎可用 |
| `store` | CRUD、rev 冲突→`-32020`、事务原子性（中途失败无半份快照）、迁移幂等（重复执行）、`historyLimit` 淘汰保留收藏、并发写（`-race`） |
| `trace` | 脱敏正则覆盖 `apiKey`/`secret`/`token`/`password`/`authorization`、长串掩码、64 KiB 截断 |
| `main` | `Handle` 全方法分发、未知方法→`-32601`、参数非法→`-32602`、`absorbParams` 递归吸收、任务表增删（完成/失败/取消三路径均清理）、`resolveMetadata` 版本反读 |

### 7.2 侧车协议 e2e（Node 脚本驱动已构建二进制）

用 `_testutil.mjs` 的异步 `runProcess`（**`spawnSync` 在本机沙箱对任何 exe 立即 `EBUSY`**），通过 JSON Lines 依次发送并断言：

```
plugin/initialize → 校验 protocolVersion=1 且 plugin.id/version 与 manifest 一致
translate/ping    → ok=true, dbOk=true, secretScheme 非空
translate/schema  → kinds 数量与已实现引擎一致，每 kind 有 fields/languages
engines/save      → ok, 返回 fingerprint 且不含明文
engines/list      → hasSecret=true, 无明文密钥字段
engines/test      → 对 httptest 假端点返回 ok
translate/run     → 非流式返回完整 target
translate/start   → 返回 taskId，随后收到 chunk 事件与 end 事件
translate/cancel  → 收到 error{code:-32034}
history/list      → 含前述翻译记录
prefs/set + get   → 往返一致
rev 冲突          → 用旧 rev 保存 → -32020 + newRev
data/export       → dataBase64 可解码，默认不含密钥
```

**密钥泄露断言**：把全部 stdout 输出与 `sidecar-trace.log` 拼接后 grep 测试用密钥字符串，必须零命中。

### 7.3 打包校验（`_verify.mjs`）

覆盖 AGENTS.md §8.1 全清单，重点：
- `manifest.version`/`id`/`publisher` 与源码一致（防「验的是旧包」假绿）
- `executable` == `bin/<target>/<binary>[.exe]`（**仅 windows 带 `.exe`**）且包内真实存在
- `checksums.algorithm == "sha256"`、精确覆盖、全部 sha256 匹配
- 制作系统声明为 Unix（`(madeBy >> 8) === 3`），可执行文件 `0755`、普通文件 `0644`
- 无 `_` 前缀临时文件混入
- 权限闸门：未声明 `host.ai` / `host.network:*`；声明了 `host.events`
- `command`/`menus` 存在时 `engines.dbx === ">=0.6.21"`；引用关系合法
- `localizations.en.contributions` 键全部对上真实贡献点 id
- `.dbx-store.json` 的 `name`/`permissions` 与 manifest 一致
- `ui/storage.js` 的 `UI_VERSION` 与 manifest 一致
- **真跑包内侧车探针**（仅本机平台；跨平台标 SKIP 并注明需真机冒烟）

### 7.4 前端静态闸门（`_domcheck.py` + `_buildpkg.js` 内建）

- `$("字面量").` 悬空引用 → 拒绝打包
- 重复函数声明 → 拒绝打包
- 关键元素清单齐备（翻译区/引擎管理/历史/设置/诊断/toast/对话框）
- 功能开关与 HTML 元素双向一致
- 捕获页面级未处理异常
- 不用 `dbxPlugin.ready` 卡住启动

### 7.5 手工验收清单

**开发宿主可测**（`dbx-plugin dev`）：引擎 CRUD、密钥加密、连通性测试、流式翻译、即时翻译与竞态、取消、历史、偏好、导出（降级路径）、诊断报告、侧车重启恢复

**仅桌面端可测**：安装未签名包、命令面板入口、工具栏快捷入口、明暗主题、zh-CN/en 切换、关闭重开工作台、`host.saveFile` 原生保存对话框、真实数据目录持久化

**必测失败路径**：空配置、错误 API Key、端点不可达、超时、断网、侧车重启、密钥库不可用（降级警示条）、超 2 MiB 文本、rev 冲突

---

## 八、生产级要求落实对照

| 要求 | 落实措施 |
|---|---|
| **健壮性** | 侧车崩溃自动降级 + 重连探测；SQLite `busy_timeout` + 重试；全外部调用 ctx 超时；任务三路径清理防泄漏；原子写；rev 乐观锁防覆盖 |
| **可维护性** | `Engine` 接口 + 注册表（新增引擎零前端改动）；schema 单一事实来源在侧车；语言映射集中管理；错误码分区表；「为什么」注释强制；AGENTS.md 坑位回填机制；版本号三处一致性闸门 |
| **鲁棒性** | 上游异常 9 类全覆盖测试；非法输入 8 类全覆盖；`-race` 并发压测；前端 `taskId` 竞态过滤；分段器保护不可分割单元；流式降级为单 chunk 统一前端路径 |
| **安全性** | 平台密钥库加密 + 降级明确披露；密钥五道铁律（不回传/不日志/不入事件/导出默认不含/仅两瞬间明文）；自动化泄露审计；`localcli` 参数数组防注入；译文纯文本渲染防 XSS；`skipTlsVerify` 与非 HTTPS 显式警示；权限最小化 + 打包闸门固化 |

---

## 九、风险与对策

| 风险 | 概率 | 影响 | 对策 |
|---|---|---|---|
| 阿里 ACS3-HMAC-SHA256 签名实现有误 | 中 | 该引擎不可用 | 用官方文档已知向量做单元测试断言；失败时返回原始 `SignatureDoesNotMatch` 详情便于定位 |
| 有道 `truncate` 按字节而非字符计数 | 中 | 多字节文本签名错误 | 明确按 `[]rune` 计数；单元测试覆盖中英混排边界（长度 20/21） |
| 桌面端事件转发行为与开发宿主不一致 | 低 | 流式失效 | 阶段 1 即在桌面端验证流式；保留 `translate/run` 非流式作为降级开关（`prefs.streamEnabled`） |
| `command`/`menus` 在桌面端表现与预期不符 | 低 | 入口缺失 | 阶段 0 即在桌面端验证入口；`engines.dbx >= 0.6.21` 已声明 |
| Linux 无 `secret-tool` | 中 | 降级为混淆 | 明确 UI 警示 + `secretSecure=false`；文档说明安装 libsecret |
| 机器变更导致密钥无法解密 | 低 | 引擎需重配 | `-32035` 明确提示重新输入，不静默清空配置 |
| 交叉编译产物未在真机验证 | 高 | 上架后 macOS/Linux 用户遇问题 | `_verify.mjs` 标 SKIP 并注明；上架前尽量在真机/虚拟机各跑一次冒烟；README 记录已验证平台 |
| 上游 LLM 不遵守「只输出译文」 | 中 | 译文含解释 | Prompt 模板强化 + 后处理剥离常见包裹（```、引号、"译文："前缀）；允许用户自定义 Prompt |
| 免费档 1 QPS 触发限流 | 高 | 分段翻译失败 | 每引擎信号量默认 1 + `chunkDelayMs` 默认 1100 + `-32032` 明确提示 |
| DBX 版本升级改变内部行为 | 中 | 功能回归 | `engines.dbx` 声明下限；`capabilities` 运行时探测；不依赖任何未文档化行为（**明确不读 `dbx.db`**） |

---

## 十、假设与默认值

**已确认决策**（用户明确选择）：
1. 引擎范围：OpenAI 兼容 + DeepL + 百度 + 有道 + 阿里 + 本地 CLI，共 6 类
2. 密钥架构：自管配置 + 侧车加密文件（**不使用 `connection-provider`**）
3. 功能范围：核心翻译 + 翻译历史（不含浮动窗口、不含注释翻译）
4. AI 配置来源：手填 + 本地 CLI 代理引擎（**不读 `dbx.db`**）
5. 即时翻译形态：workbench 内输入即译
6. 密钥保护：平台密钥库优先 + 机器绑定 AES-GCM 降级
7. 数据存储：全存 `DBX_PLUGIN_DATA_DIR`
8. 引擎节奏：分三阶段交付

**本计划设定的默认值**（实现时按此执行，无需再决策）：
- 插件 ID `com.gaogao.translation`；名称 `翻译助手`；publisher `gaogao`；Go module `github.com/gaogao/translation`；二进制 `dbx-plugin-translation`
- `engines.dbx = ">=0.6.21"`，`engines.host_api = "1"`，`permissions = ["host.events"]`
- 即时翻译：防抖 600ms（200–3000）、最小 2 字符、自动翻译上限 2000 字符
- 历史保留 500 条（100–5000），超限淘汰最旧非收藏
- 全局在飞任务上限 4；每引擎并发默认 1（可配 1–4）
- 分段延时默认 1100ms（尊重 1 QPS 免费档）
- LLM 引擎默认单次上限 20000 字符；`temperature` 默认 0.3
- `localcli` 超时默认 120s；其余引擎默认 60s
- 前端超时：`RPC=30000` `READY=8000` `CTX=2500` `PING=6000` `BRIDGE=8000` `TEST=30000`
- `MAX_UPSTREAM_BYTES = 1.5 MB`（宿主 2 MiB 留余量）
- 规范语言 30 种 + `auto`
- 流式通道用 `onEvent` + 自定义事件 `translate/chunk|end|error`，**不用 `stream()`**
- 导出默认不含密钥
- 版本演进：阶段 1 → `0.1.0`，阶段 2 → `0.2.0`，阶段 3 → `0.3.0`，阶段 4 → `1.0.0`

**待实现时确认的外部事实**（不阻塞计划，实现该引擎时查官方文档）：
- 百度/有道/阿里签名算法的官方已知测试向量（用于单元测试断言）
- 各引擎最新错误码表
- `claude --output-format stream-json` 的事件结构（阶段 2 实测确定）

---

## 十一、本机环境（已就绪）

| 组件 | 版本 / 路径 |
|---|---|
| Go | `go1.27.3 windows/amd64` @ `C:\develop\Go`（PATH 已加 `C:\develop\Go\bin`，需新开终端生效） |
| GOPROXY | `https://goproxy.cn,direct`；GOSUMDB `sum.golang.google.cn` |
| Node.js / npm | v24.18.0 / 11.16.0 |
| `dbx-plugin` | 0.1.9（`@dbx-app/plugin-cli`） |
| `dbx` CLI | 0.4.127（`@dbx-app/cli`） |
| DBX 桌面端 | `C:\develop\DBX\dbx.exe` |
| 官方 Go SDK | `C:\Users\Lenovo\AppData\Roaming\npm\node_modules\@dbx-app\plugin-cli\sdk-root\plugins\sdk\go\dbx-plugin-sdk\sdk.go` |
| 参考插件 | `C:\Project\backend\me\dbx-todo`（已上架官方商店，Go 实现） |
| 项目仓库 | `C:\Project\backend\me\dbx-plugin-translation`，origin `https://github.com/Gaogao105/dbx-plugin-translation.git`，分支 `main` |

交叉编译已实测 6 平台全通过（`CGO_ENABLED=0`）：windows/amd64、windows/arm64、linux/amd64、linux/arm64、darwin/amd64、darwin/arm64。