# AGENTS.md —— DBX 翻译插件开发规范手册

> 本手册是 `dbx-plugin-translation` 项目的**强制开发规范**。所有代码生成、修改、评审都必须遵循本文档。
>
> 规范来源：
> 1. DBX 官方文档 <https://dbxio.com/cn/docs/plugin-development>
> 2. 已上架官方商店的参考实现 `dbx-todo`（<https://github.com/lukelwai/dbx-todo>，本地路径 `C:\Project\backend\me\dbx-todo`）——其源码注释沉淀了大量真实事故，本手册的「踩坑清单」章节逐条对应。
>
> **违反「铁律」章节中任意一条，插件会表现为「装上了但什么都不工作」，且宿主不报错。这类问题极难排查，因此铁律不可协商。**

---

## 1. 项目定位

在 DBX 宿主内提供多引擎翻译能力：

- **传统机器翻译**：DeepL、百度、有道、阿里云等（各自签名算法不同）
- **自定义大模型**：用户填写任意 OpenAI 兼容端点地址 + API Key + 模型名
- **引擎管理**：增删改查多个引擎，可设默认引擎，可单独测试连通性
- **DBX 场景特化**：表/字段注释批量翻译、查询结果文本列翻译

### 1.1 明确不做的功能（宿主能力边界）

以下 IDEA Translation 插件的功能，DBX 插件系统**不支持**，不要尝试实现，也不要向用户承诺：

| 功能 | 不支持原因 |
|---|---|
| 编辑器划词翻译 | 无编辑器选区 API；`context-menu` 只支持 `connection` 和 `table` 两种菜单 |
| 译文内联提示（inlay hint） | 插件 UI 跑在 iframe 沙箱，无法覆盖宿主 DOM |
| 自定义快捷键 | 无 keybinding 贡献点，仅 `closeWorkbench` 复用 Ctrl/Cmd+W |

**替代方案**：用 `host.clipboard:read` 读剪贴板 + `floating` 浮动窗口，实现「复制即翻译」的桌面卡片。

### 1.2 官方已确认、但本轮未做的能力：浮动窗口（「复制即译」桌面卡片）

已核对官方文档 <https://dbxio.com/cn/docs/plugin-development#%E6%B5%AE%E5%8A%A8%E7%AA%97%E5%8F%A3>（2026-10-28）。
**这不是「宿主不支持」，而是「本轮没做」**——下表是照做即可的硬事实，后续版本实现时不要再猜：

| 要点 | 官方口径 | 对本项目的影响 |
|---|---|---|
| 能力探测 | 使用前先判断 `window.dbxPlugin.capabilities.floating`：Web 宿主没有窗口，桌面宿主也可能不支持窗口管理 | 必须**运行期探测**；探测不到就把入口隐藏，不要报错也不要退化成一个假窗口 |
| 打开窗口 | `floating.open({ contributionId, title, width, height, x, y, alwaysOnTop, skipTaskbar, resizable, context })` → `{ windowId, reused }`，**需要 `host.workbench`** | 当前 manifest 只有 `host.events` → 要做这个功能必须新增 `host.workbench`，并同步 `.dbx-store.json`（铁律 13） |
| `contributionId` | 指向**本插件的某个 workbench contribution**；一个（插件, contribution）只有一个窗口，重复 `open` 只是把已有窗口带到前台 | 要新增一个独立的工作台贡献点（如 `…floating`）。复用主工作台 id 会弹出整个工作台，而不是小卡片 |
| 窗口内身份 | `context.surface === "window"`，`context.workbenchId` 是窗口 id | UI 必须按 `surface` 分支渲染成胶囊卡片；`surface` 未知取值要退化成 `"tab"` 的行为 |
| 拖动 | `beginDrag()` / `endDrag({ snap })` / `setSize(w, h)` **只在浮动窗口内有效**，在标签页调用会被拒绝。插件只决定「何时开始拖」与哪个元素是把手；窗口移动、边缘吸附、位置记忆都归宿主，**没有逐帧桥调用** | 别自己实现拖动与位置持久化；`pointerup` 丢了也不会把窗口粘在光标上（宿主在光标静止后自行收尾） |
| 状态共享 | 浮动窗口是 UI 的**另一份实例**，桥、权限、上下文契约、sidecar RPC 都与标签页相同；共享状态必须走 sidecar 或 `host.storage`，不能指望模块内存 | 直接复用现有侧车 RPC；两个实例都写 prefs 时要走 rev 乐观锁，否则会互相覆盖 |
| 导航转发 | 在该表面发起的 `openWorkbench` / `openFilesystem` 会在 DBX 主窗口打开并前置；**`executeCommand` 在这个表面完全不提供**（调用会成功返回但什么都看不见） | 卡片上的「打开完整工作台」照常可用；不要在这个表面用 executeCommand 做按钮 |
| 窗口外观 | 宿主不画任何外壳，没有标签栏/dock；**刻意不支持逐像素透明**（各平台对透明区域的鼠标事件不一致）。位置跨重启保留，记录的位置不在任何已连接显示器上时丢弃 | 自己给背景 + 圆角，否则就是一块直角矩形；不要设计镂空造型 |
| 关闭 | `closeWorkbench()` 与 `floating.close()` 都能关，与 Ctrl/Cmd+W 同一条路径 | 卡片自己提供关闭按钮 |
| 读剪贴板 | 读系统剪贴板需要 `host.clipboard:read`（**写**剪贴板不需要权限） | 「复制即译」需再加这一项；两者都是老权限，不会把最低宿主版本抬过 `>=0.6.21` |
| 开发宿主 | 开发宿主不模拟窗口管理，`floating` 系列方法不可用 | **只能在桌面端验收**，与「跟随宿主」类功能同属开发宿主盲区（见 6.10） |

---

## 2. 技术栈

| 层 | 技术 | 版本约束 |
|---|---|---|
| 前端 UI | 原生 HTML + CSS + JavaScript（ES5 风格 IIFE） | 不引入构建工具，不用框架 |
| 原生侧车 | Go | `go 1.21`（`go.mod` 声明），本机 Go 1.27.3 |
| 侧车 SDK | vendor 的官方 `dbx-plugin-sdk`（Go） | 字节级复制，**禁止修改** |
| 传输协议 | JSON-RPC 2.0 over stdio，**JSON Lines** | Go 侧车只能用 JSON Lines |
| 打包 | Node.js 脚本（仅用内置模块） | 不依赖 `node_modules` |
| 数据存储 | SQLite（`modernc.org/sqlite`，纯 Go 无 cgo） | 必须无 cgo，否则无法交叉编译 |

### 2.1 为什么前端不用框架

参考插件 `dbx-todo` 的前端是**纯原生 JS**（`app.js` 1442 行 + `storage.js` 803 行），没有 Vite、没有 npm 依赖。这样做的好处：

- 打包脚本只用 Node 内置模块，`node_modules` 可以直接 `.gitignore`
- 不存在「Vite `base` 配错导致 chunk 404」这类问题（官方文档明确警告过）
- 产物即源码，`_verify.mjs` 可以直接 grep 包内 JS 做静态闸门

**本项目沿用此方案。** 如果后续确实需要框架，必须设置 `base: "./"`（Vite）或 `publicPath: "./"`（webpack），否则代码分割的 chunk 会 404。

### 2.2 为什么必须无 cgo

官方 `dbx-plugin package` **拒绝为非当前宿主平台打包**：

```
Native plugin target 'X' does not match build host 'Y';
run this package command on the target platform
```

官方推荐做法是 CI 平台矩阵。但侧车若是**纯 Go 无 cgo**，就能在 Windows 上直接交叉编译出 darwin/linux 的合法包，本地即可产出全部 6 个平台。

已实测通过（本机 Go 1.27.3，`CGO_ENABLED=0`）：

```
windows/amd64  windows/arm64  linux/amd64  linux/arm64  darwin/amd64  darwin/arm64
```

**因此：任何引入 cgo 的依赖（如 `mattn/go-sqlite3`）一律禁止。** SQLite 必须用 `modernc.org/sqlite`。

> ⚠️ 交叉编译只保证**字节正确**，不等于在目标机上验证过。darwin/linux 包的真机冒烟仍需在对应平台各跑一次。

---

## 3. 目录结构

```
dbx-plugin-translation/
├── AGENTS.md              # 本手册
├── PLAN.md                # 实施蓝图（功能/技术架构、详设、分阶段任务）
├── README.md              # 面向用户的说明 + 架构总览 + RPC 参考 + 发布流程
├── LICENSE                # Apache-2.0 全文（商店首次上架强制要求，_buildpkg.js 有闸门）
├── RELEASE_NOTES.md       # 【只写当前版本】的变更说明（Release body）；不累积历史
├── manifest.json          # 插件清单（运行时契约）
├── dbx-plugin.toml        # 打包配置（后端语言、二进制名、include）
├── .dbx-store.json        # 商店元数据（name/permissions 必须与 manifest 一致）
├── .gitattributes         # 强制 LF（打包字节可重现的前提，见 6.11）
├── .gitignore
├── .github/workflows/
│   ├── ci.yml             # 单测 + -race + 前端闸门 + e2e + 多平台打包校验
│   └── release.yml        # 【发版入口】推 tag 触发，复用官方 reusable workflow 从 tag 真机构建
├── assets/                # 图标（plugin.svg + 各引擎 logo，文件名全小写、换行符全 LF）
├── ui/                    # 前端（sandbox iframe 内运行，无构建步骤）
│   ├── index.html
│   ├── app.js             # 主翻译页、视图切换、即时翻译、流式渲染
│   ├── engines.js         # 引擎管理：schema 驱动的动态表单
│   ├── history.js         # 历史：分页、搜索、收藏、删除
│   ├── storage.js         # 桥接层：invoke/onEvent、超时、诊断、看门狗、rev 管理
│   ├── i18n.js            # zh-CN / en 双语字典（语言跟随宿主 dbxPlugin.locale）
│   └── styles.css         # 明暗双主题 + 消费宿主 --color-*/--font-*/--radius-* token
├── backend/               # 原生侧车（Go，纯标准库 + vendor SDK + modernc.org/sqlite）
│   ├── main.go            # RPC 分发、流式任务表、生命周期
│   ├── dbxsdk/            # vendor 的官方 Go SDK（sdk.go 字节级复制禁改 + VENDOR.md）
│   ├── engine/            # Engine 接口、注册表、schema、HTTP 客户端、错误分类
│   │   ├── openai/        # OpenAI 兼容（SSE 真流式）
│   │   ├── localcli/      # 本地 CLI 代理（claude / codex / 自定义）
│   │   ├── free/          # 内置免费引擎（复用 openai 适配器 + 锁定配置，零字段）
│   │   ├── deepl/ google/ baidu/ youdao/ aliyun/ tencent/
│   ├── secret/            # 密钥保护（DPAPI / Keychain / secret-tool / AES-GCM + HKDF）
│   ├── store/             # SQLite 存储层（engines / secrets / history / prefs / meta）
│   ├── lang/              # 规范语言集 + 各引擎私有码映射
│   ├── chunk/             # 分段器（保护代码块 / URL / 占位符）
│   ├── trace/             # 脱敏文件日志
│   └── go.mod / go.sum
├── dist/                  # 构建产物（gitignore，不进版本库）
│   ├── *.dbxp             # 各平台未签名候选包
│   ├── *.artifact.json    # 官方同名元数据（target/url/sha256/size）
│   ├── release-candidates.json      # {plugin, artifacts} → 上传到 GitHub Release
│   ├── store-candidate/<id>.json    # 扁平 schema → 手工提 dbx-store PR 时用
│   └── old/               # 历史版本归档（只挪不删）
├── _release.mjs           # 一键构建 + 打包 + 校验 + 出两份候选元数据
├── _buildpkg.js           # 单平台打包（zip + checksums + Unix 权限位 + 全部闸门）
├── _verify.mjs            # 单包校验（结构 / sha256 / 权限位 / 真跑侧车探针）
├── _e2e.mjs               # 侧车协议端到端测试（含假上游端点与密钥泄露审计）
├── _domcheck.py           # 前端静态闸门（DOM 悬空引用 / 关键元素 / 重复函数）
├── _storecandidate.mjs    # 生成 dbx-store 扁平 schema 候选文件（内置 validate.mjs 规则自检）
└── _testutil.mjs          # 异步 spawn 封装（绕过沙箱 spawnSync EBUSY）
```

### 3.1 命名约定

- **下划线前缀 `_`**：所有构建/校验脚本。打包时会跳过 `_` 前缀和 `.` 前缀的文件，防止脚本混入发布包。
- **插件 ID**：`com.vyrnax.translation`（小写字母、数字、`.` `_` `-`；发布后**永不更改**，改了会被视为新入口）。
- **侧车二进制名**：`dbx-plugin-translation`，必须与 `dbx-plugin.toml` 的 `[backend].binary` 一致。
- **资源文件名一律小写**（`assets/deepl.svg`，不是 `DeepL.svg`）。
  为什么：Windows 与 macOS 默认文件系统**大小写不敏感**，Linux **大小写敏感**。
  混用大小写会导致「本地能打开、CI 或 Linux 用户装完图标 404」这类只在特定平台复现的问题；
  而且 `git mv` 改大小写在 Windows 上需要 `-f`，容易漏提交成「重命名没生效」。
- **文本文件换行符一律 LF**（`.gitattributes` 已声明 `* text=auto eol=lf`）。
  但注意：该属性只在**提交时**归一化，Windows 编辑器仍可能把工作区文件写成 CRLF，
  而 `_buildpkg.js` 读的是**工作区文件** → 本地包与 CI 包字节不同。
  因此 `_buildpkg.js` 有 CRLF 闸门（见 6.11），发现即拒绝打包。

---

## 4. 铁律（违反即静默失败）

### 铁律 1：侧车 stdout 只能输出协议消息

任何 `fmt.Println` / `log.Print` 写到 stdout 都会污染 JSON Lines 流，宿主解析失败后**直接丢弃侧车**，前端表现为「数据永远存不上」，且没有任何报错。

```go
// ✅ 正确：日志写 stderr
fmt.Fprintf(os.Stderr, "[translation] %v\n", err)

// ❌ 错误：污染协议流
fmt.Println("debug:", err)
log.Println("debug:", err)   // log 默认写 stderr，但一旦 SetOutput(os.Stdout) 就完蛋
```

本项目统一用 `sidecarTrace()` 写文件日志（`<DBX_PLUGIN_DATA_DIR>/sidecar-trace.log`，>64 KiB 自动截断），排查问题时比 stderr 更可靠——因为宿主不一定把 stderr 显示出来。

### 铁律 2：Go 侧车必须用 JSON Lines，不能用 framed

`dbxpluginsdk.NewServer(metadata, handler)` 默认从 stdin 按行读 JSON、按行写 JSON，这正好匹配 manifest v1。

- **manifest v1 已不接受 `entrypoints.backend.transport` 字段**，写了会被判为无效清单。
- **framed 传输（5 字节头 + payload）是 Rust SDK 的能力**，Go 侧车必须留在 JSON Lines。

### 铁律 3：`plugin.id` / `version` 必须与 manifest 完全一致

宿主在 `plugin/initialize` 后会校验侧车自报的身份。不一致时报：

```
Sidecar identity or protocol does not match manifest
```

然后丢弃侧车。参考插件的做法是**运行时从包内 `manifest.json` 反读版本号**（`resolveMetadata()` 向上最多找 6 层目录），常量只作兜底：

```go
const (
    pluginID      = "com.vyrnax.translation"
    pluginVersion = "0.1.0"   // 仅兜底；运行时以包内 manifest.json 为准
)
```

### 铁律 4：前端不碰磁盘，也不碰 localStorage

沙箱 iframe 是 **opaque origin**（`window.origin === "null"`）：

- `localStorage` / `sessionStorage` 一碰就抛 `SecurityError`
- `navigator.clipboard` 不可用（需走宿主桥接 `dbxPlugin.copy()`）
- `alert` / `confirm` / `prompt` 不可用（必须自己写对话框组件）
- blob 下载会被宿主**无声取消**

**唯一落盘通道**：`dbxPlugin.invoke(method, params)` → Go 侧车 → 磁盘。

**绝不静默退化到 localStorage**——那会让用户以为存上了，实际重启就丢。侧车不可用时应显式降级到**会话内存后端**并在 UI 上标明「未持久化」。

### 铁律 5：`invoke` 与 `request` 不可混用

| 方法 | 目标 | 例子 |
|---|---|---|
| `dbxPlugin.invoke(method, params, {timeoutMs})` | **侧车**（自己的 Go 进程） | `translate/execute`、`engines/list` |
| `dbxPlugin.request(method, params)` | **宿主** | `host.getContext`、`host.saveFile` |

调错方向不会报错，只会静默无响应或超时。

### 铁律 6：`dbxPlugin.ready` resolve 的是纯数据，不是 API

```js
// ❌ 错误：把 ready 的结果当宿主对象用
const host = await window.dbxPlugin.ready;
host.invoke("translate/execute", {...});   // undefined is not a function

// ✅ 正确：API 永远是 window.dbxPlugin 本身
await window.dbxPlugin.ready;              // 只是等 init 消息到达
window.dbxPlugin.invoke("translate/execute", {...});
```

`ready` 的 Promise 解出来的是**工作台上下文（纯数据快照）**。

### 铁律 7：绝不对 `$("id")` 的结果直接取属性

```js
// ❌ 元素缺失时 = 对 null 赋值 = TypeError
//    而它通常位于 bindEvents/boot 开头，会把整个界面连坐搞死
$("btn-translate").onclick = doTranslate;

// ✅ 安全绑定：缺失元素只告警不致命
click("btn-translate", doTranslate);
on("src-text", "input", onInput);
val("src-text");        // 读
setVal("src-text", v);  // 写
```

这是参考插件记录的真实事故：**所有按钮失效 + 数据从不落盘**，根因是一个 DOM id 拼错。

`_buildpkg.js` 和 `_domcheck.py` 会静态扫描 `\$("...")\.` 模式并**拒绝打包**。

### 铁律 8：不允许重复的函数声明

JS 中后声明的同名函数会**静默覆盖**前面的，没有任何警告。

参考插件的真实事故：`confirmModal` 定义两次 → 删除按钮里传字符串进去 → `lines.join is not a function` 抛错。

打包脚本会扫描 `^  function (\w+)\s*\(` 并拒绝重复声明。

### 铁律 9：密钥绝不进入日志、事件、context

- API Key 只能通过 `connection-provider` 的 `binding: "secret"` 字段进入 **Secret Store**
- **绝不**把密钥放进 `binding: "config"`（那是明文 `external_config`）
- 侧车日志、`sidecarTrace()`、错误消息、RPC 返回值里**一律不得出现** API Key
- 前端拿不到密钥：只有侧车在 `connection/*` 生命周期请求中收到宿主补齐 Secret 的连接

省略 `binding` 的 `password` 字段默认按 `secret` 处理，但仍应显式声明。

### 铁律 10：权限最小化，且注意权限会抬高最低宿主版本

`permissions` 是**静态声明**，声明了老宿主不认识的权限，会在**安装阶段直接拒绝整个包**。

| 权限 | 引入版本 | 本项目是否需要 |
|---|---|---|
| `host.workbench` | 早期 | ✅ 需要（打开工作台） |
| `host.storage` | 早期 | ✅ 需要（翻译历史、UI 偏好） |
| `host.clipboard:read` | 早期 | ✅ 需要（复制即翻译） |
| `host.data:read` | **Host API 1.4** | ⚠️ 仅注释翻译功能需要，会抬高门槛 |
| `host.connections:read` | 较新 | ⚠️ 按需 |
| `host.ai` | **DBX ≥ 0.6.20** | ❌ **禁止**（本项目不依赖宿主内置 AI） |
| `host.network:https://...` | 早期 | ❌ **禁止**（网络全在侧车，见下） |

**为什么禁止 `host.network`**：它是静态白名单，最多 8 个 origin、不允许路径/通配符/Token，且仍受目标服务 CORS 约束。用户「自己填大模型地址」的需求根本无法预先声明。所有 HTTP 请求一律由侧车发出——**侧车不是操作系统沙箱，不受 CSP 约束**。

参考插件把这条固化成了打包闸门，防止日后被顺手加回：

```js
if ((mani.permissions || []).indexOf("host.ai") >= 0) {
  console.error("[FATAL] manifest 声明了 host.ai ...");
  process.exit(1);
}
```

**本项目照做，闸门清单为 `host.ai` 和 `host.network:*`。**

### 铁律 11：`command` / `menus` 贡献点要求 `engines.dbx >= 0.6.21`

这两个贡献点是 DBX 0.6.21 才引入的。声明了它们却没抬高 `engines.dbx`，老宿主会在安装阶段直接拒装。

```json
"engines": { "dbx": ">=0.6.21", "host_api": "1" }
```

同时：
- `command` 的 `action.type` 必须是 `open-workbench`，且 `action.workbench` 必须引用**同一 manifest 中已声明的** workbench id
- `menus` 每项的 `command` 必须引用已声明的 command id
- `menus` 每项的 `location` 只能是 `commandPalette` / `appToolbar` / `appSidebar`

### 铁律 12：manifest 的每个对象都是 `additionalProperties: false`

**多写一个字段，宿主就在安装阶段直接拒绝整个包**，报错形如：

```
Failed to parse plugin manifest: unknown field `label`,
expected `id` or `items` at line 68 column 3
```

这类错误在打包阶段完全看不出来（zip 打得出来、checksums 也算得对），只有安装时才炸，而且**报错只指出第一个问题** —— 修一个再装一次，逐个试错非常费时。

各贡献点允许的字段（抄自官方 `manifest.schema.json` 的 `$defs`）：

| type | 允许的字段 |
|---|---|
| `workbench` | `type` `id` `label` `description` `icon` `ai` |
| `command` | `type` `id` `label` `description` `icon` `action` `enablement` |
| **`menus`** | **只有 `type` `id` `items`** —— 没有 `label`！ |
| `context-menu` | `type` `id` `label` `description` `icon` `menu` `dynamic` `action` |
| `result-view` | `type` `id` `label` `description` `icon` |
| `connection-provider` | `type` `id` `label` `icon` `database_type` `description` `fields` `workbench` `filesystem_provider` `capabilities` `proxy_route` `actions` |
| `filesystem-provider` | `type` `id` `label` `description` `icon` `schemes` `root_uri` `capabilities` |
| `mcp` | `type` `id` `description` `ai_tools` `external_tools` |

`menuItem` 的必填字段是 `location` `command` `group` `order`（**四个都必填**），可选 `default_visible` `when`。

**对策**：`_buildpkg.js` 内置了完整的 schema 字段白名单闸门，打包前一次性校验全部字段、必填项、格式、权限枚举与 locale 键，把所有问题一次报完。白名单来源：

```
https://raw.githubusercontent.com/t8y2/dbx/main/plugins/manifest.schema.json
```

升级 DBX 后应重新拉取比对一次。

### 铁律 12b：`localizations` 的键必须对上真实贡献点 id，且不含死条目

宿主会把对不上的键判为**无效清单**。每加/删一个贡献点，必须同步更新 `localizations`。

反向也要检查：**不该为没有可本地化文案的贡献点写本地化条目**。`menus` 贡献点没有 `label`/`description`，为它写 `localizations.en.contributions.<menus-id>.label` 是死配置（本项目踩过）。

### 铁律 13：`.dbx-store.json` 的 `name` / `permissions` 必须与 manifest 一致

商店会比对，不一致直接拒绝。打包脚本做一致性闸门。

### 铁律 14：版本号三处必须一致

改版本时同步这三处，打包脚本会校验：

1. `manifest.json` 的 `version`
2. `backend/main.go` 的 `pluginVersion`
3. `ui/storage.js` 的 `UI_VERSION`

不一致会导致「装的是新版、跑的是旧代码」这种极难排查的问题。

### 铁律 15：`.dbx-dev/` 绝不提交

官方文档明确警告：开发目录 `.dbx-dev/` **可能包含明文凭据**，必须加入 `.gitignore`，不要提交或共享。

本项目涉及 API Key，这条尤其致命。

---

## 5. 打包规范（宿主安装校验的硬规则）

`.dbxp` 本质是 zip。宿主安装器（`crates/dbx-plugin-runtime/src/plugins/installer.rs` 与 `manifest.rs`）有三条硬规则，**任何一条不满足，安装直接失败，表现为「装完之后什么也存不了」**。

### 规则 1：`executable` 必须指向包内真实存在的文件

官方打包器会把 `entrypoints.backend.executable` 重写成：

```
bin/<target>/<binary>[.exe]
```

其中 `target` 形如 `windows-x64` / `darwin-arm64` / `linux-x64`。

**关键：只有 windows 目标才带 `.exe`**（官方 CLI 的 `executable_name()` 就是这么定的）。

宿主解析时**不会自动补 `.exe`，也不会做 target 目录替换**——名字对不上就是：

```
Plugin backend executable does not exist
```

整个包判为 incompatible。

```js
// ✅ 正确
const exeRel = `bin/${target}/${binary}${target.startsWith("windows") ? ".exe" : ""}`;
mani.entrypoints.backend.executable = exeRel;   // 打包时重写 manifest
```

`target` 还必须匹配 `^[a-z0-9-]{1,64}$`（对齐官方 `validate_artifact_target`）。

### 规则 2：`checksums.json` 必须精确覆盖

- `algorithm` 必须是字符串 `"sha256"`
- `files` 必须**精确等于**包内除 `checksums.json` / `signature.json` 之外的**每一个**文件

少一个多一个都是：

```
Plugin checksums do not cover the package exactly
```

```js
const checksums = { algorithm: "sha256", files: {} };
for (const [name, data] of files) {
  checksums.files[name] = crypto.createHash("sha256").update(data).digest("hex");
}
files.push(["checksums.json", Buffer.from(JSON.stringify(checksums, null, 2) + "\n", "utf8")]);
```

### 规则 3：`signature.json` 可省，但需开启「允许安装未签名开发包」

本地测试未签名包时，必须在 **DBX 插件中心**显式开启该选项。该选项不会放宽官方商店安装校验。

官方商店插件由 `t8y2/dbx-store` 维护者审核后统一加 Ed25519 签名，**开发者不接触私钥，也不要自行填写 `signingKeyId`**（那是商店 artifact metadata 字段，不是 manifest 字段）。

### 规则 4（极易漏）：zip 必须写 Unix 权限位

宿主安装器在 Unix 上执行：

```rust
if let Some(mode) = entry.unix_mode() { set_permissions(from_mode(mode & 0o777)) }
```

而 `zip` crate 的 `unix_mode()` 在 `external_attributes == 0` 时**直接返回 `None`**
→ 宿主跳过设权限
→ 解出来的文件是默认 `0o644`
→ **侧车没有可执行位，macOS/Linux 上起不来**

必须手工写 central directory 的外部属性：

```js
// version made by：高字节 3 = Unix
ch.writeUInt16LE((3 << 8) | 20, 4);

const isExe = name === exeRel;
const unixMode = isExe ? 0o100755 : 0o100644;   // S_IFREG | 权限

// ⚠️ JS 的 << 是 32 位【有符号】运算，0o100644 << 16 会溢出成负数 —— 必须用乘法
ch.writeUInt32LE((unixMode * 0x10000) >>> 0, 38);
```

官方打包器给 `bin/<target>/` 下的文件 `0o755`、其余 `0o644`，照做。

### 5.1 打包时的文件收集规则

```js
for (const dir of ["assets", "ui"]) {
  // 跳过 _ 前缀（构建脚本）与 . 前缀（隐藏文件）
  if (e.name.startsWith("_") || e.name.startsWith(".")) continue;
}
```

`dbx-plugin.toml` 的 `[package].include` 声明哪些目录参与打包：

```toml
schema_version = 1

[backend]
language = "go"
directory = "backend"
binary = "dbx-plugin-translation"

[package]
include = ["assets", "ui"]
```

CLI 会拒绝符号链接、越界路径、`.dbx-dev`、过大的文件和不安全的输出位置。**不要把 `dist/` 或开发数据再次作为输入目录。**

### 5.2 产物命名

```
dist/<plugin-id>-<version>-<target>.dbxp
dist/<plugin-id>-<version>-<target>.artifact.json
dist/release-candidates.json
dist/old/          # 历史版本归档
```

`artifact.json` 结构：

```json
{ "target": "windows-x64", "url": "<文件名>", "sha256": "...", "size": 12345 }
```

`release-candidates.json` 结构（对齐 DBX Store 官方发布流程）：

```json
{
  "plugin": { "id": "...", "name": "...", "description": "...",
              "publisher": "...", "version": "...", "permissions": [] },
  "artifacts": [ { "target": "...", "url": "...", "sha256": "...", "size": 0 } ]
}
```

**已发布资产不可覆盖**，任何字节变化都必须递增版本号并重新审核。旧包只挪进 `dist/old/`，不删除（保留可追溯）。

---

## 6. 踩坑清单（按「症状 → 根因 → 对策」组织）

### 6.1 侧车与协议

| 症状 | 根因 | 对策 |
|---|---|---|
| 装上了但数据永远存不上，无任何报错 | stdout 混入了非协议输出 | 铁律 1：日志一律走 stderr 或 `sidecarTrace()` 文件 |
| `Sidecar identity or protocol does not match manifest` | 侧车自报 id/version 与 manifest 不符 | 铁律 3：`resolveMetadata()` 运行时反读包内 manifest |
| `Plugin backend executable does not exist` | `executable` 路径/扩展名与包内实际不符 | 规则 1：只有 windows 带 `.exe` |
| macOS/Linux 上侧车起不来，Windows 正常 | zip 缺 Unix 权限位，解出来是 0644 | 规则 4：手工写 `external_attributes` |
| 侧车能启动但宿主判定不兼容 | `checksums.json` 未精确覆盖 | 规则 2 |
| `go get` 官方 SDK 失败：`unknown revision v0.1.0` | SDK **未发布到 Go module proxy** | vendor 进项目（见 6.2） |
| 老工具链拒绝构建 | 上游 SDK 声明 `go 1.22`，Go 1.20 无自动工具链切换 | vendor 成本项目内的普通 package，`go.mod` 声明 `go 1.21` |

### 6.2 SDK vendor 规范

官方 Go SDK 位于 `@dbx-app/plugin-cli` npm 包内：

```
<npm-global>/node_modules/@dbx-app/plugin-cli/sdk-root/plugins/sdk/go/dbx-plugin-sdk/sdk.go
```

本机实际路径：

```
C:\Users\Lenovo\AppData\Roaming\npm\node_modules\@dbx-app\plugin-cli\sdk-root\plugins\sdk\go\dbx-plugin-sdk\sdk.go
```

**已实测：该文件与 `dbx-todo` vendor 的版本内容完全一致（411 行，diff 为 0；字节数差异仅来自 CRLF/LF）。**

vendor 规范：

1. 复制到 `backend/dbxsdk/sdk.go`，**字节级复制，禁止任何修改**——任何改动都可能悄悄偏离宿主的协议预期
2. 同目录放 `VENDOR.md` 说明来源、为何 vendor、传输方式
3. import 路径用本模块内的：`dbxpluginsdk "github.com/vyrnax/translation/dbxsdk"`
4. 升级 CLI 后应重新 diff 一次，确认是否需要同步

**为什么用官方 SDK 而不是手写 JSON-RPC 循环**——SDK 白送三件容易写错的事：

- `plugin/initialize` 由 SDK 自己应答，返回恰好 `{protocolVersion, capabilities, plugin:{id,version}}`；宿主通告的 `protocolVersions` 不含 1 时返回 `-32001`
- 每个请求一个 goroutine（并发关联请求 ID）
- 8 MB 行缓冲；notice（无 `id` 的请求）不产生响应

这三件事手写循环很容易搞错，而**搞错任何一件都足以让宿主丢弃侧车**。

### 6.3 前端沙箱

| 症状 | 根因 | 对策 |
|---|---|---|
| `SecurityError` | 在 opaque origin 里访问 `localStorage` | 铁律 4：落盘只走 `invoke` |
| 界面全白，所有按钮失效 | `$("id").xxx = ...` 对 null 赋值，位于 boot 开头连坐 | 铁律 7：`click()` / `on()` / `val()` / `setVal()` |
| 某个按钮点了报 `xxx.join is not a function` | 同名函数被后声明者静默覆盖 | 铁律 8：打包脚本扫重复声明 |
| 导出文件没反应，也不报错 | blob 下载被宿主无声取消 | 走 `host.saveFile`（原生保存对话框） |
| `navigator.clipboard` 不可用 | 沙箱无剪贴板权限 | 写用 `dbxPlugin.copy()`；读需 `host.clipboard:read` |
| `alert`/`confirm` 不弹 | 沙箱禁用原生对话框 | 自己写对话框组件 |
| 代码分割的 chunk 404 | 构建产物用了绝对路径 `/assets/...`，携带不了插件 id | 设 `base: "./"`；本项目不用构建工具，天然规避 |
| `requestFullscreen()` 被拒 | 旧宿主未委派 Permissions-Policy | 用 `document.fullscreenEnabled` 特性检测，false 时退化为页面内最大化 |
| 自带 `disabled` 样式表失效 | 宿主把 `<link>` 内联成 `<style>` 后，`disabled` 不从内容属性反映到 IDL | 加载后自己重设 `sheet.disabled = true` |

### 6.4 桥接与上下文

| 症状 | 根因 | 对策 |
|---|---|---|
| `xxx.invoke is not a function` | 把 `ready` 的结果当 API 对象 | 铁律 6 |
| 调侧车方法无响应 | 用了 `request` 而非 `invoke` | 铁律 5 |
| 侧车拿不到用户配置的目录/地址 | 只依赖 `connection/connect` 传参，前端从工作台入口打开时没有该参数 | **三层保险**（见下） |
| 重启后数据「消失」 | 侧车只写 `config.json` 从不读，跨进程重启后目录回到兜底位置（空库） | 启动时 `loadConfigDir()` 还原 |
| 先全局打开、后建连接，之前数据找不到了 | 数据写进了兜底目录 | `migrateDir(from, to)`：仅当目标无数据时整体搬迁，**绝不覆盖** |

**存储目录三层保险**（参考插件验证过的范式，本项目用于「引擎配置目录」同理）：

```
a) connection/connect 把配置交给侧车
b) 侧车持久化到 <DBX_PLUGIN_DATA_DIR>/config.json，启动时 loadConfigDir() 还原
c) 前端若从工作台上下文读到配置，额外 invoke(".../setDir") 兜底
```

**吸收扫描**：每个 RPC 进 `Handle()` 前过一遍 `absorbParams()`，递归扫描所有可能的键名变体（`storage_dir` / `storageDir` / `storage_path` / `storagePath` / ...），自动喂给 `setDir()`。这样即使前端把路径藏在嵌套结构里，侧车也能拿到。

> 本项目对应的是引擎配置键：`endpoint` / `api_key` / `engine_type` / `model` 等，同样要做多变体吸收。

**上下文提取要递归**：宿主可能把配置放在 `ctx.external_config`、`ctx.connection.external_config`，或更深的嵌套里。参考实现用 `extractPath(obj, keys, depth)` 递归最多 4 层查找。

**`onContext` 要容错**：老宿主没有该方法，`try/catch` 包住，缺失时忽略。

### 6.5 负载与超时

| 限制 | 值 | 说明 |
|---|---|---|
| Workbench context UTF-8 上限 | **2 MiB** | 超限直接报错 |
| `request` 参数 JSON 上限 | **2 MiB** | 传 base64 时守住 `MAX_UPSTREAM_BYTES = 1.5 MB` |
| SDK 单条 JSON 消息上限 | **8 MB** | `maxJSONBytes` |
| SDK 二进制消息上限 | **64 MB** | 需 framed 传输（Go 不可用） |
| `host.storage` 单值 | **256 KiB** | 序列化后 |
| `host.storage` 整库 | **1 MiB** | 这是 UI 状态存储，不是数据落盘通道 |
| 文件系统内联读写 | **4 MiB** | 大文件要分块 + 进度/取消 |
| `queryData` 行数 | 默认 500，最多 5000 | 序列化后行数据上限 8 MiB |
| `queryData` 超时 | 受连接自身超时和 **60 秒**上限约束 | |
| MCP 工具单次调用 | **120 秒** | 长任务应后台启动并返回句柄 |
| `connection/action` 的 `timeout_ms` | **1–120000** | |

**前端必须自己设超时**，不能裸等：

```js
var RPC_TIMEOUT = 30000, READY_TIMEOUT = 8000, CTX_TIMEOUT = 2500,
    PING_TIMEOUT = 6000, BRIDGE_TIMEOUT = 8000;
```

`waitBridge()` 用 25ms 轮询等 `window.dbxPlugin` 注入（宿主注入时机不确定），超时后 `failHard()` 而不是无限等待。

**大批量数据、缓存、Sidecar 产生的任何文件，放进侧车自己的数据目录**，不要塞 `host.storage`。

### 6.6 并发与数据一致性

**乐观锁 `_rev`**：单会话正常流程不会冲突，但「被其它进程改过」时前端手里的 `rev` 会落后于磁盘。

```
1. 读当前磁盘 rev（事务内，被 SQLite 串行化）
2. 若前端回声的 incomingRev != 当前 rev → 返回 {conflict:true, newRev}，绝不覆盖
3. 否则整体替换写入，rev 递增后回传
4. 前端收到新 rev 更新 currentRev；遇 conflict 则 refreshRev() 后重试一次
```

重试**只做一次**——既避免单会话下「二次保存必失败」，也保证多会话时不静默覆盖对方数据。

**整体替换要在一个事务里** `DELETE` 再 `INSERT`，保证不会出现「半份快照」。

**瞬态锁缓解**：`busy_timeout(5000)` + migrate 失败重试一次（间隔 150ms），错误消息带语句序号与库路径，便于远程定位。

**`SetMaxOpenConns(1)`**：SQLite 单写者，避免并发写冲突。

**幂等加列**：旧库升级用 `ALTER TABLE ... ADD COLUMN`，忽略 "duplicate column" 错误。

**原子写文件**：临时文件 + rename，不要直接覆盖。

### 6.7 本机沙箱环境（构建脚本相关）

| 症状 | 根因 | 对策 |
|---|---|---|
| `spawnSync` 对任何 exe 立刻 `EBUSY` | 本机沙箱接管 | **只能用异步 `spawn`**（`_testutil.mjs` 的 `runProcess`） |
| `fs.rmSync`/`unlinkSync` 目录一多就 `ETIMEDOUT` | 被接管成「移到回收站」 | 删除一律走异步 `fs.promises.rm` |
| Go 编译进程被杀 | `GOCACHE`/`GOTMPDIR` 在沙箱外 | 指到项目内：`_gocache/`、`_gotmp/`（并 `.gitignore`） |
| 刚写出的二进制启动失败 | 杀软短暂锁住 | 带重试：`/EBUSY\|EPERM\|ETXTBSY\|EACCES/` 匹配后 sleep 500ms 重试，最多 4 次 |
| 排查时只看到 `exit=null` 以为是「侧车不回话」 | 没打印 `error.message` | **务必把 `error.message` 打出来**——进程启动失败与侧车不回话是两回事 |

### 6.8 本轮实施新踩的坑（阶段 0–1）

以下都是实施过程中真实遇到并已修复的问题，按「症状 → 根因 → 对策」记录。

| 症状 | 根因 | 对策 |
|---|---|---|
| `go get golang.org/x/crypto/hkdf` 后 `go.mod` 的 `go` 指令被顶到 1.26.0 | x/crypto 与 x/sys 最新版都声明 `go 1.26.0`，会绑架整个项目的工具链下限 | **不引入 x/crypto**：HKDF 只有 extract+expand 两步约 30 行，用标准库 `crypto/hmac`+`crypto/sha256` 手写（`backend/secret/hkdf.go`）；x/sys 固定到 v0.22.0（兼容 go 1.21） |
| `regexp.MustCompile` panic：`invalid character class range: \\-+` | Go 原始字符串里写了 4 个反斜杠，RE2 解析成「两个字面反斜杠 + `-+` 范围」 | 把 `-` 放到字符类**末尾**（`[A-Za-z0-9_+-]`），无需任何转义，彻底规避范围解析歧义 |
| 日志里正常 URL 被掩码成 `com/****ions` | 脱敏的 `longToken` 字符类含 `/` 和 `.`，把 URL 路径段整体匹配了 | 字符类**移除 `/` `.` `=`**；密钥总是以 `apiKey=xxx` 带键名形态出现，第一步 kv 正则已覆盖，兜底正则不必激进 |
| 搜 `100%` 命中 0 条 | SQLite 的 `LIKE` 默认不认反斜杠转义，`escapeLike` 转义了却没声明 `ESCAPE` 子句 | `LIKE ? ESCAPE '\\'`，转义与声明必须成对出现 |
| 拖拽排序后顺序不变 | `UPDATE ... SET ord=ord+?, updated_at=? WHERE id NOT IN (?)` 的参数顺序与占位符不匹配，`now` 被放到了末尾 | 参数顺序严格对应占位符出现顺序：offset → now → ids... |
| 同一秒创建的多条记录分页时重复/漏项 | `ORDER BY created_at` 在时间戳相同时顺序不确定 | 所有排序补 `rowid` 作 tiebreak（`ORDER BY created_at DESC, rowid DESC`） |
| 新建的引擎立刻「不存在默认引擎」 | `mkEngine` 测试助手没设 `Enabled`，而 `SaveEngine` 忠实持久化调用方给的值（不做隐式默认） | 明确契约：新建引擎的 `Enabled` 默认由 RPC 层负责；测试助手同步补上 |
| `prefs["x"] != 1200` 恒为真 | 偏好经 JSON 往返后数字统一变 `float64`，接口比较时 `float64(1200) != int(1200)` | 测试用 `numEq()` helper 先统一转 float64 再比 |
| `NaN`/`Inf` 写成 Go 常量报 `division by zero` | Go 编译期常量折叠不允许除零 | 用 `math.NaN()` / `math.Inf(1)` 在运行期构造 |
| `_verify.mjs` 报「用了 localStorage」但代码里没有 | 闸门扫的是**原文**，而 storage.js 的注释正在解释「为什么不用 localStorage」 | 扫描前剥离注释（块注释 + 行注释），且 `_buildpkg.js` 与 `_verify.mjs` 的剥离规则必须一致，否则同一个包一个放行一个拒绝 |
| 未知方法探针测不出 `-32601` | 用了中文方法名，SDK 的 `validProtocolName` 只允许 ASCII，请求在 `decodeRequest` 阶段就被拒，到不了 `Handle` | 探针方法名必须 ASCII（`nonexistent/method`）；这条约束本身值得记住：**RPC 方法名不能含非 ASCII 字符** |
| e2e 断言「数据库没写进去」（只有 4096 字节） | 库开了 WAL 模式，侧车运行中新数据都在 `translation.db-wal`，主库还是空壳；进程干净退出时才 checkpoint 回主库 | 检查数据库内容必须把 `.db` + `.db-wal` + `.db-shm` 一起读 |
| Go 原始字符串里写不出反引号 | Prompt 文本需要提到 ``` 围栏，而原始字符串（反引号包裹）内部不能含反引号 | 用「原始字符串 + 解释字符串 + 原始字符串」拼接：`` ` + "```" + ` `` |
| `context.Cause(err)` 编译失败 | `context.Cause` 接受的是 `context` 而不是 `error` | 取消判定用 `errors.Is(err, context.Canceled)` 沿 Unwrap 链查找 |

| `dbx-plugin-env` 监听器在桌面端不触发 | 新版宿主把该事件 target 在 **document** 上且**不冒泡**，注册在 window 冒泡阶段的监听器收不到；而开发宿主是在 iframe 内 `dispatchEvent`（会冒泡），所以本地测不出来 | `window` 与 `document` **都注册且都用 `capture=true`**；另加 `onInit` 兜底（某些宿主只发 init 不发 env） |
| 宿主改了调色板/圆角，插件界面不变 | 本项目 CSS 用 `--bg`/`--panel`/`--text`，宿主投递的是 `--color-background`/`--color-card`/`--color-foreground`，**两套变量名对不上**，token 投递了也没人消费 | 加 `HOST_TOKEN_MAP` 做「宿主名 → 本项目名」映射，且只在宿主确实投递了该 token 时才覆盖（否则会把 CSS 兜底值冲成 undefined） |
| 映射写了但界面仍不变 | 映射目标 `--primary-fg` 在 CSS 里**根本没定义**，按钮仍硬编码 `color: #fff` —— 映射到一个不存在的变量等于没映射 | `_verify.mjs` 解析 `HOST_TOKEN_MAP` 每一对，逐个核对目标变量在 CSS 中「已定义」且「被 `var()` 消费」；同时校验无硬编码 `color: #fff` |
| 英文界面下仍显示中文 | `info-secret` 的「（系统密钥库，真加密）」被硬编码成中文字符串，没走 i18n | 动态生成的文案也必须走 `t()`；这类问题在中文环境测不出来，只有切英文才暴露 |
| `_buildpkg.js` 报 `SyntaxError: Invalid or unexpected token` | 给 JS 文件加注释时误用了 Python 的 `#`（批量改两个闸门脚本时复制粘贴串了） | JS 用 `//`、Python 用 `#`；改完立刻 `node --check` 验语法，别等到打包才发现 |
| 开发宿主里字体跟随测不出来 | 开发宿主只投递 15 个 token（`--color-*` + `--radius-*`），**不含 `--font-*`**（已实测确认） | 字体跟随只能在桌面端验证；CSS 必须用 `var(--font-sans, 兜底栈)` 形式，保证宿主没投递时仍走兜底而不是退化成衬线体 |

| 安装失败：`unknown field \`label\`, expected \`id\` or \`items\`` | 给 `menus` 贡献点加了 `label`，但官方 schema 的 `menusContribution` 只允许 `type`/`id`/`items` 且 `additionalProperties:false` | 移除该字段；`_buildpkg.js` 加完整 schema 字段白名单闸门，打包前一次报完所有问题（宿主只报第一个） |
| `localizations` 里给 menus 本地化 label 是死配置 | menus 没有 label 字段，本地化条目无处生效 | 只为「有 `label`/`description` 的贡献点」要求本地化覆盖；反向加死条目闸门 |
| `_buildpkg.js` 报 `Invalid regular expression: Unterminated group` | 正则**字面量**里出现裸 `/`（如 `https://`、`(?!.*//)`）会提前终止字面量 | 含 `/` 的 pattern 一律用 `new RegExp("字符串")`，字符串里只需按普通规则转义一次 |

| 界面语言永远是英文，切宿主语言没反应（主题却正常） | `detect()` 只在 `boot()` 开头跑一次，那时宿主的 **init 消息还没到达**，`dbxPlugin.locale` 返回的是桥接闭包的**初始值**（en）；init 之后 locale 才更新为真实值，但再没重新探测过。主题之所以正常，是因为 `S.init().then()` 里调了 `applyTheme()` 却漏了对应的 `detect()` + `applyI18N()` | `S.init()` 完成后**必须重新** `detect()` + `applyI18N()`。诊断报告里同时打印「界面语言 / 宿主 locale / `dbxPlugin.locale`」三个值，一眼就能区分是「宿主没投递」还是「插件没读取」 |
| `translate/schema` 加载失败，引擎管理页「新建引擎」表单是空的 | `E.init()` 在 `S.init()` **之前**执行，此时桥接未就绪，`rpcRaw` 直接 reject 且**永不重试** | ① `rpcRaw` 改为桥接未就绪时先 `waitBridge()` 再调用；② `E.init()`/`H.init()` 移到 `S.init().then()` 内；③ `E.init()` 的 Promise 加 catch 并 toast 提示（否则用户只看到空白弹框，不知道是 schema 没加载） |
| 语言/主题跟随「有时生效有时不生效」 | 事件通道有三重不确定性：新版宿主 target=document 且不冒泡、老宿主 target=window、某些宿主只发 init 不发 env。任一情况没覆盖就是静默失效 | 三重保险：window+document 双注册且都 capture、`onInit` 兜底、**1.5 秒低频轮询**（比较「locale+appearance+token 数」指纹，无变化则什么都不做）。轮询与事件处理器**共享指纹**，避免重复重绘 |

| 点取消后要等 29 秒才停（CLI 是 `sleep 30`） | 两个原因叠加：① `for scanner.Scan()` 是阻塞读，CLI 不输出时循环体里的 `ctx.Err()` 检查【根本没机会执行】；② Windows 上 `.bat` 会形成 `cmd.exe → 实际命令` 的进程树，`Process.Kill()` 只杀 cmd.exe，真正的命令变孤儿进程**继续持有 stdout 管道写端**，读取方依旧阻塞 | ① 读取移入 goroutine，主流程用 `select { case <-results / case <-runCtx.Done() }`；② 按平台实现 `killTree`：Windows 用 `taskkill /F /T /PID`，Unix 用 `Setpgid` + `kill(-pid)`。修复后取消耗时 29.4s → 0.65s |
| 注入防护测试「通过」了，但把实现改成 shell 拼接后**依然通过** | 判据是「payload 尝试创建 marker 文件，断言文件不存在」，而 payload 里的 `touch` 在 Windows 上根本不存在 —— 无论有没有经过 shell，marker 都不会被创建，测试永远 PASS（空转） | 改用强判据：假 CLI 把**实际收到的 argv 写成 JSON 文件**，测试读文件断言「与传入的参数数组逐字相等」。经过 shell 会切词/吞运算符，参数个数必变，判据必然失败。已用变异测试验证（改成 `cmd.exe /C` 拼接后测试正确 FAIL） |
| 想按行还原子进程参数，却只得到 1 行 | `runCLI` 把各次 emit 的文本**无分隔符拼接**（这对翻译是正确的 —— 增量片段本该直接相连），stdout 无法还原参数边界 | 需要精确参数边界时走**文件**而非 stdout |

| 阿里云填了 AccessKey Secret，点「测试连接」却报「不能为空」 | `buildEngine` 硬编码了 `Secrets{"apiKey":…, "secretKey":…, "appSecret":…}` 三个键，而阿里云适配器读的是 `accessKeySecret` —— 键名对不上，适配器永远读到空值 | 改为 `secretsFor(kind, plain)`：从 schema 反查标了 `Secret: true` 的字段名，用它作键。**新增引擎只要 schema 写对就自动正确**，不存在漏配。并补 `TestSecretsFor_AllEnginesCanReadSecret` 遍历全部引擎做回归（已用变异测试验证该测试确实能抓到原 bug） |
| 保存引擎反复提示「配置已被其它会话修改，已自动重试」，诊断日志里 `本地 rev=59 服务端 rev=60` 反复出现同一个 59 | 两处叠加：① `withRevRetry` 只在 `body.rev === undefined` 时才填 rev，而 `buildPayload` 已把 engines.js 自己缓存的陈旧 rev 塞进 payload，该分支永不成立，**重试时用的还是陈旧 rev**；② `prefs/set` 与 `engines/*` 共用同一个全局 rev，改一次偏好就让引擎对话框缓存的 rev 失效 | `withRevRetry` 改为**无条件** `body.rev = status.rev`（storage.js 是唯一能从每个 RPC 响应持续刷新 rev 的地方，即 rev 的唯一权威来源）；`buildPayload` 不再自带 rev，上层模块不缓存 rev |

| DeepL 填了正确的 Key，点「测试连接」报 400 `Value for 'source_lang' not supported`；但官方 curl 示例能成功 | DeepL 的 `source_lang` 与 `target_lang` 接受的取值集合**不同**：target 要 `EN-US`/`PT-PT`（带地区后缀），source 只接受 `EN`/`PT`（不带）。`MapSource` 与 `MapTarget` 共用一张表，于是源语言发出了 `EN-US` | lang 包新增 `sourceOverrides` 表，`MapSource` 优先查它。**隐蔽点**：英译中失败、中译英正常（ZH 两个位置都合法），看起来像偶发故障；而连通性探测固定用 en→zh，所以「测试连接」必失败 |
| deepl 的单元测试一直绿，却没发现上面这个 bug | 测试断言写的是 `source_lang == "EN-US"` —— **照着实现的输出写断言**，等于把 bug 固化成规范 | 断言值必须来自官方文档，不能来自「实现现在输出什么」。已改为断言 `EN`，并补 `TestMap_DeepLTargetStillHasRegion` 防止修源语言时把目标语言也改坏（target 传 `EN` 同样会 400） |
| GitHub Actions 上 `[FATAL] 找不到 target=darwin-arm64 的侧车二进制：/home/runner/.../_xbuild\dbx-plugin-translation-darwin-arm64` | `_buildpkg.js` 用 `root + \`_xbuild\\${binary}...\`` 硬编码反斜杠。Windows 上正常，Linux runner 上反斜杠不是分隔符，拼成「文件名里带反斜杠」的怪路径，`fs.existsSync` 必然 false | 全部改用 `path.join`。**症状极具误导性**：日志显示 5 个平台 `[build]` 全部成功，下一步却说找不到文件，看起来像 Go 交叉编译的问题，实际只是路径分隔符 |
| 改完 `path.join` 后本地打包报 `ENOENT: ...dbx-plugin-translationmanifest.json` | 只改了 `resolveExe` 里的 3 处，却把 `const root = __dirname + path.sep` 的尾部分隔符去掉了，而文件里还有 7 处 `root + "manifest.json"` 依赖它 | 改路径拼接方式时必须**全文件扫描**所有同类拼接点，不能只改报错的那一处。已加 `_verify.mjs` 闸门扫描全部构建脚本 |
| 新加的路径闸门一上来就 FAIL，报 `_buildpkg.js` 与 `_verify.mjs` 都有硬编码路径 | 闸门扫的是**原文**，而它自己的注释里正写着这个 bug 的示例代码（`root + \`_xbuild\\...\``） | 扫描前用 `stripComments` 剥离注释。这与「storage.js 注释解释为什么不用 localStorage 却被判违规」是同一类教训 —— **凡是 grep 源码的闸门，都必须先剥离注释** |
| 状态栏改了N轮仍随内容上下漂移 | `#app` 用 grid，但**在流子元素有 5 个**（topbar + 两条 banner + main + statusbar），`grid-template-rows` 只声明了 4 条轨道 → statusbar 落进隐式 `auto` 行，「固定状态栏轨道」根本没作用到它 | 多区纵向布局优先 **flex 列**：固定项 `flex: 0 0 auto`，伸缩项 `flex: 1 1 auto; min-height: 0`。`min-height: 0` 不能省——flex/grid 子项默认 `min-height: auto`，内容超高时不收缩而是把容器撑爆。用 grid 就必须数清「在流子元素 = 轨道数」，且注意 `[hidden]` 的元素显示出来时会多占一条 |
| 「明明改了 CSS，界面一点变化没有」，而脚本没报错 | 批量替换的匹配串写成**单行**，文件里实际是**多行块** → 匹配 0 次、替换 0 次、也不报错 | 任何替换型修改都要先断言 `count(old) == 1` 再替换，不满足就退出非零。「改了没生效」的第一嫌疑人永远是**替换根本没发生** |
| 图标放 `assets/` 里前端怎么都读不到 | 宿主 `ui.readAsset` **只允许 UI root 以内**的路径，越界直接抛 `Asset is outside UI root` | UI 要消费的资源放 `ui/icons/`；`assets/` 只留给 manifest 里 `icon` 字段那种由宿主加载的图 |
| `<img>` 用 `readAssetUrl` 拿到的地址加载失败 | 沙箱 CSP 的 `img-src` 只放行 `data:` 与 `blob:`，`readAssetUrl` 可能返回 `http(s)` 形式，被 CSP 拦掉且**不报网络错误** | 优先 `readAsset` → 自己拼 `data:` URI；`readAssetUrl` 只作兜底；两者都失败退回首字母色块，绝不出现破图 |
| 同一个 18px 方槽里，圆形 logo 比方 logo 明显大一圈 | 各引擎 SVG 的**画布**尺寸（24/32/1024）与**内容包围盒占画布比例**都不同，`object-fit: contain` 只按画布等比缩放 | 做**光学归一**：实测每个图标的内容包围盒，用 `<g transform>` 统一成「最长边占画布 88% + 居中」。手写 SVG path 解析器算包围盒两次都算错（贝塞尔极值不是端点），要用真实渲染引擎（无头浏览器 `getBBox()`）测 |
| 深色主题下 `localcli` 图标几乎看不见 | 图标本身是黑色，深色背景没有对比度；`prefers-color-scheme` 对内联 SVG 不生效 | 图标配色必须明暗两种主题各看一遍。不能用宿主主题色（会和内容色撞）就用中性彩色；内联 SVG 想跟随主题要写 `fill="var(--brand-fg, currentColor)"` |
| 翻译页引擎下拉里放不出图标 | 原生 `<select>` 的 `<option>` 由**操作系统绘制**，浏览器不支持在里面放图片/HTML | 需要图标就得**自绘下拉**（`button` + `role="listbox"` 的 `ul`），同时补 `aria-haspopup` / `aria-expanded` / `aria-selected`，别让读屏器失去语义 |
| 引擎列表的拖拽把手「看得见但拖不动」，也没有任何报错 | 原生 HTML5 DnD（`draggable` + `dragstart`）在宿主的 **sandbox iframe** 里不触发 | 改用**指针事件**自己实现：`pointerdown/move/up` + 按 `clientY` 命中卡片实时插入 DOM，松手时读 DOM 顺序提交。凡是「沙箱里点了没反应」的功能，先怀疑浏览器原生能力在 opaque origin iframe 中被削掉 |
| 改了「默认引擎置顶」的排序契约，`TestReorderEngines` 立刻 FAIL | **不是实现错，是旧断言钉住了旧契约**（原断言要求 reorder 后 `e2` 第一） | 契约变更要同步改断言，并且**补一步能区分新旧契约的验证**（这里加「换默认引擎再查一次顺序」）：只测一组的话全 0 的 `ord` 也能凑出同样顺序，测试失去区分力 |
| 点「设为默认」后引擎插队到列表第一位，用户再也控制不了顺序 | 排序写成 `ORDER BY is_default DESC` —— 把「哪台是默认」和「列表怎么排」两件事耦合在一起；再叠上「翻译页下拉必须与列表同序」这条要求，一个动作会连带改掉两处排列 | **顺序只由 `ord` 决定，默认只改标记**（2026-10-29 用户明确要求，推翻旧的「默认永远置顶」）。这条契约在代码里原本有【四处】实现，必须一起改干净：① `ListEngines` 的 `ORDER BY is_default DESC`；② 前端 `orderedEngineList()` 比较器里的 `a.isDefault` 优先；③ 默认引擎的拖拽把手被刻意禁用（`bindGrip` 里 `if (e.isDefault) return` + `drag-disabled` 样式）；④ `commitOrder()` 在写回前把默认引擎 id 硬塞到最前 —— 这一处最隐蔽，表现是「把默认引擎拖到哪都会自己弹回第一位」，用户只会以为拖动功能坏了。凡是「一个概念被多处代码各自实现」的改动，都要先 `grep` 全量定位再动手；断言上用 `SetDefaultEngine` 前后 id 序列必须完全相等来兜住四处中的任何回退 |
| 内置引擎上线后，新加的用户引擎看起来「排在内置后面」 | 新引擎 `ord` 默认 0，多个 0 靠 `created_at` 决胜，内置记录在首次启动时就创建了，自然在最前 | 保留「ord → created_at → rowid」的决胜链（确定性优先于好看），用户可用把手任意拖动；`EnsureBuiltin` 在已有用户引擎时把内置放到 `max(ord)+1` |
| 本机 `go test -race` 起不来：`-race requires cgo` | Windows 的 race detector 依赖 cgo，而本项目**必须无 cgo** 才能交叉编译 6 平台 | 本机不跑 `-race`，交给 CI 的 ubuntu job；**不要**为了跑竞态检测在本机开 cgo——那会掩盖真实的构建约束 |

| 新版 GitHub Release 正文挂着**上一个版本**的说明 | `RELEASE_NOTES.md` 采用「累积历史」写法，每次发版只在末尾追加一节，于是每次 Release 都以一大段上一版的完整功能手册开头，真正的增量埋在最后；而且**没有任何闸门校验这个文件**，忘了改就是静默失败 | 改为**只写当前版本**：Release body = 本版增量，历史归档天然存在于 GitHub Releases 页面与 git tag（每个 Release 的 body 就是当时的快照，不可变）。用户向的完整功能手册、安装、已知限制、排查迁到 `README.md` 第 8 节；能力与数据披露 `README.md` 第 7.3 节早就有，不再重复维护。并在 `_buildpkg.js` 加闸门：`RELEASE_NOTES.md` 首行版本号必须等于 `manifest.version` |

| 用户配好第一个官方引擎后，翻译仍走内置免费引擎 | 「首个引擎自动设默认」的判据是 `COUNT(*)=1`，而内置记录从安装起就永久占着一条 → 判据再也不成立；把置位改成条件后又出现**两个默认引擎**（只置位不清别人），并列时按 `ord/created_at` 决胜，更早创建的内置记录继续顶在最前 | 判据改为「非内置记录只有 1 条」，且提升默认时**必须先 `UPDATE … is_default=0` 再置位**。任何「唯一位」语义的标记都要成对写清除+设置，光设置会留下并列 |
| 内置引擎与用户的 OpenAI 引擎抢同一个并发信号量、语言支持集还拿错 | 内置引擎复用 `openai` 适配器，内层 `Kind()` 返回 `"openai"`；而 kind 同时是**信号量 key** 与**语言映射表索引** | 包装类型必须覆盖 `Kind()`（以及 `SupportedLangs()`）再对外暴露。凡是「复用别人实现」的适配器，都要检查内层实现把 kind/id 用作【路由键】的地方 |
| 内置端点要求无凭据，但 openai 适配器对非 localhost 一律拒绝空 Key | 校验只认「localhost 可以没 Key」，而公益端点既不需要 Key 也不是 localhost | 加 `noAuth` 内部开关（**不出现在表单字段里**，用户在 UI 上填不出来），而不是给内置引擎塞一个假 Key —— 假 Key 会被当 `Authorization` 头发出去 |
| 单段上限想设 400 却被夹回 500 | `openai` 的 `maxChars` 校验下限是 500，而内置引擎的服务端 30 秒网关超时要求单段 ≤400（实测 820 字符必 504） | 下限降到 100。教训：**同一份校验的边界值也是契约**，收紧或放宽都要写清为什么；`TestBuild_ConfigClamping` 同步更新 |
| 「列表数量 = 1」的 e2e 断言在内置引擎上线后失败 | 不是代码错，是**断言钉住了旧形态**（列表原本只会有用户引擎）；而 `list[0]` 也不再必然是用户引擎 | 契约变更后同步改断言，并把新契约本身加进断言（内置记录存在、默认引擎仍置顶、删除内置被拒绝且文案含替代方案）。同时警惕用 `list[0]` 取「我刚建的那条」—— 应按 id 查 |
| Go 测试里 `res["engines"].([]any)` 恒为 nil，看着像「接口返回空列表」 | `Handle()` 返回的是 **Go 原生结构**（`[]map[string]any`），不是 JSON；断言成 `[]any` 直接失败且没有任何报错 | 断言用具体类型 `[]map[string]any`（沿用仓库既有惯例），或先 `mustJSON()` 再 `json.Unmarshal` 到 `[]any` |
| 一次性脚本里改了版本号，输出却是旧值 | 脚本先读盘改版本、又为了改描述**再次 read 同一文件**，把上一轮结果覆盖掉；打印的 "OK" 只证明脚本没崩 | 落盘型脚本改完必须**重新读文件复核**（`_bump` 类操作要断言最终三处相等），不能相信「没报错就是改成了」 |

| 引擎下拉的宽度被「当前选中的那一项」钉死，切到长名字就换行撑高列表 | 自绘下拉的菜单写了 `position:absolute; left:0; right:0`（宽度=容器宽），而容器 `.kind-select{flex:1 1 auto}` 处在随内容收缩的 `.engine-picker` 里，于是容器宽=按钮文案宽=**当前选中项的宽度**。CSS 层面看不出问题，只有在「选中短名字 + 存在长名字」的组合下才暴露 | 菜单改成 `left:0; right:auto; min-width:100%`，实际宽度由 `fitDropdownMenu` 按最长选项量出来后夹在「视口右边界」内写 inline width（无条件 `width:max-content` 会被 `overflow:auto` 的容器裁掉或冒出横向滚动条，所以必须量一次）。**两处下拉共用同一个函数**，别各写一份 |
| 只想改宽度，最后发现真正的痛点是「名称根本没有上限」 | 用户界面里任何「文本长度不可控」的位置，迟早都会以换行/挤压控件的形式爆出来；靠 CSS 截断 + 悬浮看全称只是补救 | 优先定**硬性上限**并前后端同一规则（全角按 2 单位、半角按 1，上限 32 单位 ≈ 16 汉字）。规则必须在两处各自有测试钉住，并用同一张用例表交叉核对（本次用一次性 node 脚本抽出前端实现与 Go 表逐条比对，9 条含混合与边界全一致）—— 否则漂移的表现是「用户没输超长却被保存拒绝」 |

| 「CSS 规则明明写了，界面上一点变化都没有」，而且本地打包、闸门、控制台都不报错 | 按 JS 习惯在 `.css` 里写了 `// 说明`。CSS 没有行注释语法，浏览器做错误恢复时会**连紧随其后的那条规则一起丢弃** —— 我一次丢了两条：省略号那条（长名字继续换行）和选择器宽度区间那条（下拉宽度仍然跟着选中项跑） | 打包闸门新增 CSS 检查：任何以 `//` 开头的行、以及未闭合的 `/*`，一律 `[FATAL]`。这类「只有运行时才暴露」的语法问题必须前置到打包阶段 |
| 判断「样式没生效」不能只读代码，要量 | 我第一反应是「测量方式错了」（把 `fitDropdownMenu` 改成 `width:max-content` 再量），改完看似合理，但用户的现象没变 | 用无头浏览器（Edge `--headless=new --dump-dom`）把真实的 `styles.css` + 真实 DOM + 从 `storage.js` 里**抽出来的**函数跑一遍，打印「容器宽 / 菜单宽 / 每个选项的行高」，修复前后各跑一次。行高 >40 就是换行了 —— 一次测量直接锁定是 CSS 被吞，而不是 JS 没算 |

| 「行变灰了但拖不动」，松手后顺序不变、也没有任何报错 | `pointermove` / `pointerup` 只挂在 15px 宽的把手 `<span>` 上，指望 `setPointerCapture` 把后续事件重定向过来；而那行 capture 调用写在 `try { ... } catch { /* 忽略 */ }` 里，**失败被静默吞掉**，于是光标一离开把手就再没有事件到达 | 拖动类交互一律把 `move`/`up`/`cancel` 挂到 **window** 上，不依赖捕获（捕获只该当优化，不能当正确性前提）；`catch` 里绝不能什么都不做。用合成事件在真实浏览器里量一次即可证伪：挂在把手上的监听收到 0 次、window 上 2 次 |
| 拖动中的行显示成灰白半透明，用户以为「把引擎拖成禁用了」 | `.engine-card.dragging { opacity:.45 }` 与「已禁用」行（`is-disabled` 灰字）视觉上几乎一样，而且这个类文件里还写了【两遍】 | 被拖对象要表达「离开原位、正在移动」而不是「变弱」：描边高亮 + 阴影 + 微上浮，**文字保持正常对比度**；另外拖拽期间把被拖行设 `pointer-events:none`（否则命中判定永远命中它自己）、`body.is-reordering` 锁掉文本选择（原生选区会吃掉 pointermove） |

| 「引擎页已经拖好顺序，翻译页的下拉还是老顺序」，而且**没有任何报错** | 拖动走的是**本地乐观更新**路径（`applyReorderIds`），它只改了 `state.engines` 的数组顺序：① 没把新 `ord` 回写进每条记录，而下拉是按 `ord` 排序的 → 排序结果当然是拖之前的顺序；② 没调用 `notifyChanged()`，翻译页压根不知道列表变过。（`loadEngines()` 那条正常路径两件事都做，所以只有「拖完立刻切页」才复现） | 任何「改了共享状态的本地乐观更新」都必须同时做到：**重算所有派生字段**（这里就是 `ord`）+ **向消费方广播**；失败分支还要一并回滚并再次广播，否则前端显示的顺序与库里的 ord 从此分叉，重启插件后顺序会莫名跳回去。新增消费方时先问「它读数组顺序还是读 ord 字段」，两者不一致就是这类 bug 的温床 |


**从这些坑里提炼的两条通用规则**（已并入铁律）：

- **静态闸门的判据必须在 `_buildpkg.js` 与 `_verify.mjs` 之间保持一致**，且都要先剥离注释再扫描。两份闸门判据不一致会产生「本地打包通过、校验失败」的困惑。
- **凡是「时好时坏」的断言，先怀疑 WAL / 缓存 / 异步时序**。数据库内容检查要覆盖 WAL 文件；异步协议测试要用轮询等待而非固定 sleep。
- **「跟随宿主」类功能必须在桌面端验证，开发宿主测不出来**。开发宿主与桌面端在事件 target、token 投递范围上都有差异（env 事件冒泡行为、`--font-*` 是否投递）。凡是依赖宿主投递内容的功能，都要在 `_verify.mjs` 里加静态校验作为替代防线。
- **映射/别名类代码必须双向校验**：既校验「源存在」，也校验「目标被真实消费」。只写映射不核对目标，会产生「代码看起来对、界面就是不变」的静默失效。
- **安全类测试必须做变异验证**：把实现故意改坏（例如参数数组改成 shell 拼接），确认测试会 FAIL。不做这一步就无法区分「实现安全」和「测试写错了所以永远通过」—— 本项目真的踩过后者，而且是在 Windows 上因为 `touch` 不存在而空转。
- **凡是「等外部进程/网络」的循环，取消检查不能只放在循环体里**。阻塞读（`scanner.Scan()`、`conn.Read()`）在对方不输出时会让循环体永远得不到执行，必须把读取放进 goroutine，主流程用 `select` 监听 `ctx.Done()`。
- **杀子进程要考虑进程树**。Windows 的 `.bat`/`.cmd`、Unix 的 shell 脚本都会产生父子进程，只杀直接子进程会留下孤儿继续持有管道，导致「取消无效」与 goroutine 泄漏。
- **跨模块共享的状态必须有唯一权威来源**。rev 这类「服务端递增、客户端回声」的值，只能由一层持有并从每个响应里刷新；让多个模块各自缓存，必然出现「A 的缓存过期了但 B 不知道」，症状是重试逻辑看似存在却永远用旧值。
- **凡是「按名字取配置」的地方，名字必须来自同一份 schema**。适配器读 `accessKeySecret`、而调用方塞 `apiKey`，编译器不会报错、七个引擎都正常，只有一个引擎静默失效 —— 这类 bug 只能靠「遍历全部引擎」的测试兜住。
- **测试断言的期望值必须来自官方文档，不能来自「实现现在输出什么」**。照着实现写断言等于把 bug 固化成规范：测试永远绿、线上永远错。DeepL 的 `source_lang=EN-US` 就是这么漏过去的。
- **同一服务商的「请求参数」与「响应参数」取值集合可能不同**，源语言与目标语言也可能不同。接第三方 API 时必须逐个位置核对官方文档，不能假设对称。
- **构建脚本里的路径一律用 `path.join`**，绝不手写分隔符。本地是 Windows、CI 是 Linux，硬编码反斜杠在本地永远测不出来。
- **凡是「宿主只在某个阶段才校验」的契约，必须在本地提前校验**。manifest 字段白名单就是典型：打包阶段不报错、安装阶段才炸，而且一次只报一个错。把官方 schema 的约束抄进打包闸门，一次报完全部问题，比逐个试错省几倍时间。
- **JS 正则字面量里不能出现裸 `/`**。含 URL 或 `//` 的 pattern 一律用 `new RegExp("...")`。
- **凡是读「宿主桥接属性」的代码，都必须区分 init 前与 init 后**。`dbxPlugin.locale` / `.theme` / `.context` 在 init 消息到达前返回的是闭包初始值而非真实值。任何在 `boot()` 开头读它们的逻辑，都必须在 `ready` 之后**再读一次**。这类 bug 的症状是「某个功能永远停在默认值」，而且因为界面看起来完全正常，用户只会以为「插件不支持」。
- **静默失效的功能要有主动兜底**。「语言/主题不跟随」不会报错、不会白屏，用户根本不会去报 bug，只会默默卸载。对这类功能，一层廉价的轮询（1.5 秒比较一次指纹）远比「相信事件一定会来」可靠。

- **别把「机器速度」当成规范来断言，否则测试必然 flaky**：`openai_test.go` 曾断言 `ElapsedMs <= 0` 即报错，等于假设「一次翻译至少花 1ms」。该假设在快机器上不成立 —— Linux CI 上对进程内 `httptest` 端点的一次往返常在 0.x ms 内完成；Windows 本机更极端，实测连续两次 `time.Now()` 有 **100%** 概率返回完全相同的值（200 次采样全为 0），200 次进程内 HTTP 往返里 **177 次** `time.Since(start)` 直接等于 0。于是同一份代码在 CI 上随机红绿（前一次 run 还是绿的）。**正确修法是改掉断言（只断言非负），而不是把实现硬掰成正数** —— 0ms 是合法状态。顺带把 14 处重复的 `time.Since(start).Milliseconds()`（向下截断，0.4ms 显示成 0ms）收敛成 `engine.ElapsedMs()`（向上取整 + 钳非负），缺陷只可能存在一处。教训同 DeepL 那条：**断言的值必须来自规范，不能来自「实现现在输出什么」**。

- **「已上架的版本号绝不能复用」**：商店核对过 0.2.2 那批字节的哈希之后，任何改动（哪怕只是换图标、调状态栏）都必须升到 0.2.3 再打包。同号不同字节会让「用户装的到底是哪份」说不清，商店也会判 provenance 不符；Release 资产也不可覆盖，只能新版本重发。
- **改布局的验证单位是「在流子元素 ↔ 轨道/伸缩声明」的对应关系**，不是「我加了 CSS」：轨道数少于子元素数时，多出来的元素落进隐式轨道，看起来声明了固定尺寸却完全不起作用。
- **「替换型脚本修改」必须断言替换发生了几次**：匹配不上的替换是静默成功，代价是一整轮人工复验白跑。

### 6.9 网络与代理（国内环境）

| 问题 | 对策 |
|---|---|
| `winget install GoLang.Go` 失败 `0x80072efd` | 连不上 `go.dev`；改用阿里云镜像 `https://mirrors.aliyun.com/golang/` |
| `go mod download` 卡住 | `go env -w GOPROXY=https://goproxy.cn,direct` + `GOSUMDB=sum.golang.google.cn` |
| MSI 用户级安装后 `go` 命令找不到 | 未写 PATH；手工加 `C:\develop\Go\bin` 到用户 PATH，**并重开终端** |
| `npm install` 慢 | 视情况设 registry 镜像 |

本机已配置完成（Go 1.27.3 @ `C:\develop\Go`，GOPROXY 已设）。

### 6.10 主题、字体与国际化（一律跟随宿主）

**设计决策：主题、字体、界面语言全部跟随宿主 DBX，不提供任何手动覆盖入口。**

理由：用户在 DBX 里切了深色主题或英文界面，期望所有面板一起变；插件若自顾自保持另一套外观会显得割裂。而且这三个值的唯一事实来源就是宿主，存进 prefs 反而会制造「设置里显示 A、界面实际是 B」的不一致。

因此 `prefKeys` 白名单里**刻意没有** `themeOverride` / `uiLocale`，并有 `TestPrefs_AppearanceKeysRejected` 把「外观不入库」固化成契约，防止日后被顺手加回。

#### 6.10.1 env 事件必须用 capture 注册（最易漏的坑）

`dbx-plugin-toolbox` 的 `src/lib/host.js` 注释写得很明确：

> DBX env events target **document** and do not bubble; capture supports both current and older window-based hosts.

也就是说新版宿主把 `dbx-plugin-env` 的 target 放在 **document** 上且**不冒泡**。document 是 window 的祖先，事件不冒泡时，注册在 window **冒泡阶段**的监听器永远收不到。

```js
// ❌ 收不到（新版宿主 target=document 且不冒泡）
window.addEventListener("dbx-plugin-env", onEnv);

// ✅ capture 阶段从 window 向下传播到 document，能截获
window.addEventListener("dbx-plugin-env", onEnv, true);
document.addEventListener("dbx-plugin-env", onEnv, true);   // 双注册兼容新旧宿主
```

**漏掉的症状**：「DBX 切了主题，插件界面纹丝不动」。而开发宿主是在 iframe 内部 `dispatchEvent`（target = iframe window，会冒泡），所以**本地测不出来、装进桌面端才暴露**。`_verify.mjs` 已把这条固化成校验项。

另外要加 `onInit` 兜底：某些宿主在桥接就绪时只发 init 不发 env，首屏会用错外观。

#### 6.10.2 主题负载的完整结构

官方文档「主题负载携带什么」的投递矩阵：

| DBX 设置 | 投递形式 |
|---|---|
| 浅色/深色/跟随系统 | `theme.appearance` |
| 调色板 + 自定义配色 | `--color-*` tokens |
| 圆角风格 | `--radius-*` tokens |
| UI 字体 | `--font-sans` token |
| 编辑器（等宽）字体 | `--font-mono` token + `theme.editor.fontFamily` |
| 编辑器字号 | `theme.editor.fontSize` |
| SQL 编辑器语法主题 | `theme.editor.theme` |
| UI 缩放（宿主窗口级） | **不投递** —— 插件自管缩放 |
| 数据网格字体/类型配色、编辑器背景图 | **不投递** —— DB 域（`--dbx-*` 不在扫取范围） |

`theme` 可能是**属性也可能是函数**，老宿主甚至只返回 `"dark"`/`"light"` 字符串，三种形态都要接（见 `storage.js` 的 `readThemeObject`）。

#### 6.10.3 宿主 token 名与本项目变量名对不上（真实功能缺口）

本项目 `styles.css` 用自己的语义变量名（`--bg` / `--panel` / `--text` / `--primary`），而宿主投递的是 DBX/shadcn 体系的 `--color-background` / `--color-card` / `--color-foreground`。**两套名字对不上，宿主投递的调色板就完全不会生效** —— 只有明暗切换生效（因为 CSS 里有 `body[data-theme="dark"]` 两套兜底值），用户在 DBX 里自定义的主色、边框色、圆角都传不进来。

对策是加一层 `HOST_TOKEN_MAP` 映射（宿主名 → 本项目名），且**只在宿主确实投递了该 token 时才覆盖**，否则会把 CSS 兜底值冲成 undefined。

**映射目标必须在 CSS 里真实定义且被消费**，否则映射写了等于没写。本项目曾出现过映射到 `--primary-fg` 但 CSS 里根本没这个变量、按钮仍硬编码 `color: #fff` 的情况。`_verify.mjs` 现在会解析 `HOST_TOKEN_MAP` 的每一对，逐个核对目标变量在 CSS 中「已定义」且「被 `var()` 消费」。

#### 6.10.4 字体跟随的落地方式

```css
/* --font-sans / --font-mono 由 applyTheme 从宿主注入，这里只提供兜底 */
--sans: var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI",
            "PingFang SC", "Microsoft YaHei", sans-serif);
--mono: var(--font-mono, ui-monospace, "SFMono-Regular", Consolas, Menlo, monospace);

body { font-family: var(--sans); }
```

用 `var(--font-sans, 兜底栈)` 而不是直接写死字体：宿主投递了就覆盖，没投递就用兜底，两种情况都正确。

**开发宿主只投递 15 个 token（`--color-*` + `--radius-*`），不含 `--font-*`**（已实测确认）。所以字体跟随**只能在桌面端验证**，开发宿主里永远走兜底栈。

#### 6.10.5 字号不归宿主管

官方文档明确：「字体&字号类关注点归插件所有：宿主不规定你的字号」。`theme.editor.fontSize` 是**编辑器**字号，给等宽代码区用；UI 正文仍由插件自己定（本项目 14px）。不要拿编辑器字号去覆盖 body 字号。

#### 6.10.6 国际化

`localizations` 只覆盖 manifest 层文案（插件名、说明、贡献点 label/description），**不会翻译插件 UI 自己的文本**。UI 文本必须自建字典（`ui/i18n.js`，zh-CN 与 en 各 137 键，完全对称）。

语言来源：`dbxPlugin.locale`，归一化规则「以 zh 开头 → zh-CN，以 en 开头 → en，无法识别 → 回退 zh-CN」。

**动态生成的文案也要走 i18n**：本项目曾把 `info-secret` 的「（系统密钥库，真加密）」硬编码成中文，在英文界面下仍显示中文。这类问题在中文环境里测不出来，只有切到英文才暴露。

必须测试浅色/深色主题和 `zh-CN`/`en` 两种语言。


---

### 6.11 商店上架审核踩坑（v0.2.1 被打回的 4 个阻断项）

首次提交 `t8y2/dbx-store` 上架时，审核者给出「暂不能上架」，4 个阻断项。逐条记录，**这些都不是打包 bug，`_verify.mjs` 全绿也照样被打回**——因为它们校验的是「包自身是否合法」，而商店校验的是「包能否被第三方复现与核验」。

| 症状 | 根因 | 对策 |
|---|---|---|
| 审核者报「被签名的字节来自一个公开仓库里不存在的 commit」，`git fetch` 取不到 `53d8ebb` | sidecar 是**本地构建**的，Go 把当时的 `HEAD` 编进了二进制（`vcs.revision`），而那个 commit **从未推送到远程**。tag `v0.2.1` 的 CI 产物哈希与 release 资产**全不同** → 一眼看出 release 里的包不是 CI 构建的 | **发布产物一律由 CI 从 tag 构建**：`.github/workflows/release.yml` 复用官方 `t8y2/dbx/.github/workflows/plugin-release-reusable.yml`（pin 到具体 commit），按平台矩阵在各自 runner 上真机构建并 `gh release upload`。本地 `node _release.mjs` 的产物**只用于自测** |
| tag 那次 CI 与 release 资产的 sidecar 分别是 `go1.21.13` 与 `go1.27.3` | CI 写 `go-version: "1.21"`（浮动，解析成当时的补丁号），本地是 1.27.3 —— 两条工具链产出不同字节 | Go 版本 **pin 到精确补丁号 `1.21.13`**，`ci.yml` 与 `release.yml` 用同一个值。浮动写法会随 runner 镜像更新漂移，导致同一 tag 重建出不同哈希，审核者无法复现 |
| 同一个 commit，本地包与 CI 包有 **7 个 SVG** 字节不同 | `.gitattributes` 写了 `eol=lf`，但那只影响**提交时的归一化**；Windows 编辑器把工作区文件改成了 CRLF，git blob 仍是 LF。`_buildpkg.js` 读的是**工作区文件** → 本地打进 CRLF 版，CI（Linux checkout）打进 LF 版 | ① `git add --renormalize assets/` 让工作区回到 LF；② `_buildpkg.js` 增加**CRLF 闸门**：包内任何 `.json/.js/.css/.html/.svg/.txt/.md/.toml` 含 `\r\n` 直接拒绝打包 |
| `validate.mjs` 报 `unknown field(s): plugin, artifacts` | 把 `_release.mjs` 产出的 `release-candidates.json`（`{plugin, artifacts}`，给**自动同步**用的）当成了手工 PR 的 `candidates/<id>.json`（**扁平 schema**） | 两条路径格式不同，别拿错。`_storecandidate.mjs` 专门生成扁平候选（`schemaVersion:1` + `targets[]`，url 必须是 release 资产的**绝对 HTTPS 地址**，icon 必须是绝对 URL 且扩展名 SVG/PNG），并内置 `validate.mjs` 的规则自检 |
| 仓库 `license` 为 null，却声明了 Apache-2.0 | 只有 `.dbx-store.json` 里的字符串，**没有 LICENSE 文件** | 补 `LICENSE`（Apache-2.0 全文）；`_buildpkg.js` 增加闸门：根目录缺 `LICENSE` 直接拒绝打包。LICENSE 不进包（打包只收 `assets/`+`ui/`），所以补它不改变包字节 |
| Release 标着 `prerelease: true`，商店同步不到 | `discover-plugin-releases.mjs` 只认 `!draft && !prerelease` 的 Release | `release.yml` 的 prepare job 建 Release 时用正式发布；已存在则 `gh release edit --draft=false --prerelease=false --latest` 转正式 |

**另外几条审核要求（不阻塞签名，但必须在 PR 里写明）**：

- **`host.network` 白名单不约束原生侧车**。候选只声明 `host.events`，但侧车会直连用户填的任意端点 —— 必须在 PR 模板的「network access / native sidecar behavior」里主动披露，否则审核者会认为在藏能力。
- **翻译历史是明文 SQLite，且卸载后保留**。要在 listing 描述里点明（已写进 `manifest.description` 与 `.dbx-store.json`）。
- **`localcli` 引擎会执行本机命令**（虽是参数数组、不经 shell），属于必须披露的原生行为。
- **darwin 侧车只有 ad-hoc 签名**，未做 Apple notarization，要说明现状与计划。

**已固化的自动闸门**（把「靠人记得」变成「机器强制」）：

- `_buildpkg.js`：缺 `LICENSE` / 包内文本含 CRLF / 资源名含大写 → 拒绝打包。
- `_storecandidate.mjs` 的 `checkProvenance()`：生成扁平候选前核对字节可复现性 ——
  CI 核对 `GITHUB_REF==refs/tags/v<version>` 且 `GITHUB_SHA==HEAD` 且工作树干净；
  本地核对工作树干净 + tag 指向 HEAD + HEAD 已推送远程。不满足即 `[FATAL]`。
  本地 `_release.mjs` 自动带 `--allow-unverified` 跳过（产物不可提交商店）。
  **这道闸门直接对应阻断项 1**：本地构建的 sidecar 内嵌未推送的 `vcs.revision`，
  审核者去公开仓库查不到 → 判定「字节来自不存在的 commit」。

**可复用的经验**：`README.md` 第 7.3 节固化了本插件对这几个审核问题的标准答复口径，下次发版直接照抄，不必重新组织语言。

---

## 7. 编码规范

### 7.1 通用（强制）

1. **语言**：所有自然语言回复、代码注释、文档、错误提示、示例说明，**必须全部使用简体中文**。变量名、方法名等标识符用英文。
2. **编码**：所有源码文件必须 **UTF-8 without BOM**。**严禁在文件起始位置拼接 `\ufeff`**；若生成内容以 `\ufeff` 开头，输出前强制清除。
3. **换行**：Go 源码用 LF；JS/JSON 用 LF；打包时统一 `JSON.stringify(...) + "\n"` 结尾。
4. **临时脚本**：工作过程中产生的临时 `.ps1`/`.py`/`.js`，确认无用后**及时清理**。
5. **格式**：大括号位置、缩进、空格不做硬性规定，生成后用 IDE 自动格式化统一调整。Go 代码提交前跑 `gofmt`。

### 7.2 Go 侧车

1. **文件头注释**：`main.go` 顶部用块注释列出**全部 RPC 方法一览**，一行一个，说明用途。这是最快的接口文档。
2. **注释写「为什么」不写「是什么」**：参考插件的注释价值极高，因为它记录的是事故与约束，例如：

   ```go
   // loadConfigDir 在侧车启动时把上一次连接写入的 storage_dir 还原进 currentDir。
   // setDir 只【写】config.json 从不【读】，导致跨进程重启后 currentDir 回到空，
   // 数据库被解析到插件数据目录的兜底位置（空库），连接数据「消失」。
   ```

   **新踩的坑必须以同样格式补进注释**，这是本项目知识沉淀的主要方式。
3. **错误消息面向用户**：侧车返回的 `message` 会直接显示给用户，必须是可理解的中文，并带上可操作的下一步。技术细节（库路径、语句序号）附在后面便于远程定位。
4. **错误码分区**：

   | Code | 含义 |
   |---|---|
   | `-32601` | 方法不存在（`MethodNotFound`） |
   | `-32602` | 参数非法（`badParams`） |
   | `-32001` | 协议版本不匹配（SDK 自动返回） |
   | `-32002` | 读取/保存存储失败 |
   | `-32004` | 导出失败 |
   | `-32006` | 导入失败 |
   | `-32012` | 写入界面偏好失败 |
   | `-32020` | `rev` 冲突 |
   | `-32030` | **翻译引擎网络失败**（本项目新增） |
   | `-32031` | **翻译引擎鉴权失败**（本项目新增，401/403） |
   | `-32032` | **翻译引擎限流**（本项目新增，429） |
   | `-32033` | **翻译引擎响应格式非法**（本项目新增） |
   | `-32034` | **翻译被取消**（本项目新增） |

5. **`Handle()` 用 `switch method` 平铺分发**，`default` 返回 `MethodNotFound`。不要搞反射注册表——平铺更易读、易静态检查。
6. **业务错误 vs 协议错误**：可预期的业务失败（如「目录不可写」）返回 `{success:false, message:"..."}` 作为**正常 result**，让前端能展示；不可恢复的才用 `PluginError`。
7. **并发安全**：共享状态（连接表、当前目录、DB 句柄）一律加锁。SDK 是**每请求一个 goroutine**，任何全局可变状态都有竞态。
8. **连接/断开必须幂等**。
9. **长任务要设计超时、取消和分块确认**。翻译流式输出属于此类。
10. **不要在事件、context 或错误里回显密钥**。

### 7.3 前端 JS

1. **整体包在 IIFE 里**，`"use strict"`，只暴露一个全局命名空间对象（如 `window.TranslationStore`）。
2. **ES5 风格**（`var` + `function`），最大化宿主 WebView 兼容性。参考插件全程如此。
3. **安全 DOM 助手是强制的**：`$()` / `val()` / `setVal()` / `click()` / `on()` / `h()`。
4. **页面级错误兜底**：

   ```js
   window.addEventListener("error", function (e) { console.error("[翻译] 页面错误:", e.error || e.message); });
   window.addEventListener("unhandledrejection", function (e) { console.error("[翻译] 未捕获 Promise:", e.reason); });
   ```

5. **所有事件回调包 `try/catch`**，异常转 `reportErr(where, err)` → toast，不让单个交互炸掉整个界面。
6. **内置分步诊断**：`note(step, ok, detail)` 记录每一步（最多 150 条），`report()` 输出完整报告（环境快照 + 状态 + 诊断日志）。**安装后白屏/存不上时先看这份报告。**
7. **环境快照**要包含：`origin`、是否 sandbox、`typeof window.dbxPlugin`、`invoke`/`request`/`ready` 的类型、`Object.keys` 前 30 个、`document.readyState`。
8. **看门狗**：启动流程卡住时也要在 UI 留下「卡在哪一步」的证据。
9. **功能开关必须与 HTML 一致**：如 `SHOW_DIAG_BAR` / `ENABLE_IMPORT`，打包脚本会双向校验（开关 true 就得有元素，false 就不能有）。
10. **`[hidden]` 要显式声明 CSS**：`display:flex` 会顶掉默认隐藏。

    ```css
    .more-menu[hidden] { display: none; }
    ```

11. **删除是「从状态移除后整体保存」**，不要依赖 `deletedIds` 推断删除。
12. **保存防抖**：`SAVE_DEBOUNCE = 400`，同时提供 `writeNow()` 立即落盘（关闭前调用）。

### 7.4 manifest.json

1. 引用官方 schema 获得编辑器校验：

   ```json
   "$schema": "https://raw.githubusercontent.com/t8y2/dbx/main/plugins/manifest.schema.json"
   ```

2. **Manifest v1 不接受未声明字段**——多写一个字段就是无效清单。
3. 路径相对于包根目录，**不能以 `/` 开头、不能包含 `..`、反斜杠或重复斜杠**。
4. `ui.entry` 必须位于 `ui.root` 内：root 为 `ui` 时入口写 `ui/index.html`，**不是** `index.html`。
5. `description` 要写清楚数据落在哪里、由谁写入——这是用户最关心的隐私问题。
6. 同一插件内通过 `id` 互相引用：`connection-provider.workbench` 必须指向已声明的 workbench。
7. 图标优先用 Contribution 的 `icon`，缺失时回退插件级 `icon`。

### 7.5 连接表单字段

1. 每个字段必须声明 `key`、`label`、`type`；可选 `description`、`placeholder`、`required`、`default`、`options`、`binding`。
2. 类型只能是 `text`、`password`、`number`、`boolean`、`select`、`radio`、`textarea`。
3. `select`/`radio` **必须提供非空 `options`**，每项含 `label` 和**字符串** `value`。
4. `binding: "port"` 必须用 `number` 类型。
5. 默认值必须符合字段类型。
6. **`binding` 语义**：`config` → 明文 `external_config`；`secret` → Secret Store；`name`/`host`/`port`/`username`/`password`/`database` → 标准连接字段。
7. **条件显隐**：`visible_when` / `required_when` 接受单条子句 `{field, one_of}`，用 `all_of`/`any_of`/`not` 组合。
   - 按**字符串比较**，因此 `[false]` 与 `["false"]` 都能匹配布尔 `false`
   - 条件会**级联**：子句引用的字段自身隐藏时该子句不参与判定
   - 未填写或空值**永不命中**
   - 组合树最多 **8 层、64 个节点**
   - DBX 在对话框与保存/测试/连接校验中使用**同一套求值**，不会出现「表单放行、宿主拒绝」
8. **`picker`**：`text`/`password`/`textarea` 可声明本机文件选择。
   - 桌面端写回**绝对路径**；网页端变成**上传**，内容写入 `content_field` 兄弟字段并清空路径字段
   - 两者互相清除，避免同时存在两份矛盾来源
   - `kind` 支持 `file` / `directory`（仅桌面端）；`accept` 最多 16 条；上传上限 **1 MiB**

---

## 8. 测试与验收

### 8.1 静态闸门（提交前必跑）

```bash
python _domcheck.py       # DOM 悬空引用 / 关键元素 / 重复函数声明
node _verify.mjs          # 包结构 / checksums / executable 路径与扩展名 / 权限位 / 真跑侧车探针
```

`_verify.mjs` 的校验项应至少覆盖：

- `manifest.version` / `id` / `publisher` 与源码一致（防「验的是旧包」这种假绿）
- 文件名里的 target 合法
- `executable` 指向 `bin/<target>/<binary>[.exe]`（windows 才带 `.exe`）且在包内真实存在
- `checksums.algorithm == "sha256"`、精确覆盖、全部 sha256 匹配
- 无 `_` 前缀临时文件混入
- 制作系统声明为 Unix（`(madeBy >> 8) === 3`），否则权限位被忽略
- 可执行文件带 `0755`、普通文件带 `0644`
- `storage.js` 以 `window.dbxPlugin` 为 API、不把 `ready` 的值当 API、用 `invoke(method, payload, {timeoutMs})`、用 `request("host.getContext")` 兜底、含会话内存后端（不伪装持久）、导出诊断报告与看门狗
- `UI_VERSION` 与 manifest 一致
- `app.js` 无重复函数声明、无对缺失元素直接取属性、捕获页面级未处理异常、不用 `dbxPlugin.ready` 卡住启动
- 权限闸门（未声明 `host.ai` / `host.network:*`）
- `command`/`menus` 与 `engines.dbx` 匹配、引用关系合法
- `localizations` 键都能对上真实贡献点 id
- `.dbx-store.json` 的 `permissions`/`name` 与 manifest 一致

### 8.2 真跑侧车探针（唯一可靠的能力证明）

**grep 字符串不可靠**——必须把包内的二进制真的启动一次，用 JSON-RPC 问它：

```js
const input = [
  JSON.stringify({jsonrpc:"2.0", id:"1", method:"plugin/initialize",
                  params:{host:{protocolVersions:[1]}}}),
  JSON.stringify({jsonrpc:"2.0", id:"2", method:"translate/ping", params:{}}),
  JSON.stringify({jsonrpc:"2.0", id:"3", method:"engines/list", params:{}}),
  // ...
].join("\n") + "\n";
```

**只能对本机平台的包执行**：交叉编译出的 darwin/linux 二进制在本机跑不了，标记为 `SKIP` 并注明「需在该平台真机冒烟」。

探针目录用 `DBX_PLUGIN_DATA_DIR` 环境变量指向临时目录，结束后 `removeTree` 清理。

### 8.3 官方本地验收清单

- [ ] 关闭未使用的权限，确认 manifest 权限与实际调用一致
- [ ] 用**空配置、错误凭据、超时、断网、Sidecar 重启**分别测试失败路径
- [ ] 测试 DBX 浅色/深色主题和 `zh-CN`/`en`；不依赖宿主翻译插件 UI
- [ ] 测试重新连接、关闭工作台、重复打开同一工作台、上下文为空的情况
- [ ] 检查包内所有资源路径、可执行文件权限、最终包是否含不应发布的文件

### 8.4 本项目额外验收项

- [ ] **API Key 泄露检查**：`sidecar-trace.log`、前端诊断报告、所有错误消息、RPC 返回值中均不得出现密钥明文
- [ ] **错误 API Key** → 返回 `-32031` 且消息可理解，不重试
- [ ] **端点不可达** → 返回 `-32030`，带超时时长，允许用户重试
- [ ] **429 限流** → 返回 `-32032`，提示稍后再试
- [ ] **流式翻译中途取消** → 侧车立即停止读取上游 SSE，返回 `-32034`，不泄漏 goroutine
- [ ] **上游返回非 JSON / 非预期结构** → 返回 `-32033`，附原始片段（截断）便于定位
- [ ] **多引擎并存**：切换默认引擎后立即生效，不需重启侧车
- [ ] **侧车重启后**：引擎配置、翻译历史、UI 偏好全部还原（`loadConfigDir()` 生效）
- [ ] **大文本**：超长输入分批或明确拒绝，不撑爆 8 MB 行缓冲

### 8.5 开发调试

```bash
# 独立开发宿主（不启动 DBX 桌面端）
dbx-plugin dev --path . --port 5190
# 打开 http://127.0.0.1:5190/

# 开发数据移出项目目录
dbx-plugin dev --path . --data-dir C:/temp/translation-dev

# 从脚本读脱敏日志
curl -sS "http://127.0.0.1:5190/api/diagnostics?after=0&limit=100&level=error"
```

`dbx-plugin.toml` 的 `[dev]` 决定前端构建命令：

```toml
[dev]
ui_build = ["node", "_domcheck.py"]   # 本项目无构建步骤，可留空或跑静态检查
```

配置 `ui_watch` 时，命令只有在**一次完整成功构建后**输出独立一行 `DBX_UI_BUILD_SUCCESS`，开发宿主才会重载 UI。不要在失败路径或无条件退出钩子中输出该标记。

**开发宿主的局限**（官方明确说明）：只模拟受支持的 Host API 子集。**安装、签名、真实 Secret Store、桌面端生命周期和生产权限必须再用 DBX 桌面端验证。**

本项目核心就是 Secret Store 存 API Key，**不在桌面端验一遍等于密钥通道完全没测过**。

调试页可切换语言、主题、自动重载和日志面板。日志只保留内存中最近 500 条，密码/Token/manifest 中的 Secret 字段会脱敏，二进制和过大的值会省略或截断。

本机 DBX 桌面端：`C:\develop\DBX\dbx.exe`

---

## 9. 发布流程

### 9.1 仓库归属（不要提错地方）

| 要修改的内容 | 提交位置 | 是否向官方提 PR |
|---|---|---|
| 本插件前端、Go 后端、测试、发布脚本 | **本仓库** | 否 |
| 新插件上架、版本更新、商店图标和介绍 | `t8y2/dbx-store` | 是，目标分支 `main` |
| DBX 插件 Host、Manifest/Marketplace Schema、SDK、CLI、Packager | `t8y2/dbx` | 是，目标分支 `main` |
| DBX 官方示例或插件开发文档 | `t8y2/dbx` | 是 |
| 某个第三方插件自身的 Bug | 该插件源码仓库 | 不要提到 `dbx-store` |

**插件上架 PR 提交到 `t8y2/dbx-store`，不是 `t8y2/dbx`。**

不要为了发布一个普通插件把整个源码目录复制进 `t8y2/dbx`。

### 9.2 四步流程

**第一步：推 tag，由 CI 构建并发布候选 Release**

```bash
git push origin main
git tag v<version> && git push origin v<version>
```

`.github/workflows/release.yml` 会自动创建 Release 并上传：
- 每个平台的未签名 `.dbxp` 候选包
- 每个候选包对应的 `.artifact.json`
- 汇总后的 `release-candidates.json`（`{plugin, artifacts}` 格式，供商店自动同步）
- `dbx-store-candidate` artifact（扁平 schema 候选文件，供手工提 PR）
- Release Notes（取自 `RELEASE_NOTES.md`）

**🚫 绝不本地构建后手工上传候选包。** Go 会把构建时的 git 戳编进二进制
（`go version -m` 可见 `vcs.revision` / `vcs.modified`）。本地构建时 HEAD 往往还没推到远程，
于是 sidecar 内嵌的 revision 在公开仓库里查不到，商店会判定「被签名的字节来自一个不存在的
commit」——**这是没有裁量空间的 provenance 硬伤**，v0.2.1 正是因此被打回（详见 6.11）。

本地 `node _release.mjs` 的产物只用于开发自测。

候选包放本仓库 GitHub Releases、CDN 或对象存储均可，但**不要把 `.dbxp` 提交进 Git 历史**。

**第二步：让 dbx-store 自动创建候选 PR**

`dbx-store` 的同步 Workflow 定期检查已在 `automation/plugin-sources.json` 注册且 `autoUpdate: true` 的公开仓库，读取最新 Release 的 `release-candidates.json`，创建或更新唯一的：

```
candidates/<plugin-id>.json
```

已启用自动更新的作者**不需要每个版本手工创建 Issue 或 PR**，也不需要在插件仓库配置商店签名 Secret。首次上架或修改商店展示字段时，才按 `dbx-store` 贡献指南提供发布者和候选信息。

**⚠️ 两份候选元数据格式不同，别拿错**（v0.2.1 因此被 `validate.mjs` 拒绝）：

| 文件 | 格式 | 用途 |
|---|---|---|
| `dist/release-candidates.json` | `{plugin, artifacts}` | 上传到 Release，供**自动同步**路径消费 |
| `dist/store-candidate/<id>.json` | 扁平 schema（`schemaVersion`/`targets[]`） | **手工提 PR** 时复制成 `candidates/<id>.json` |

把前者当成手工 PR 的候选文件提交，`validate.mjs` 直接报 `unknown field(s): plugin, artifacts`。
扁平候选由 `_storecandidate.mjs` 生成，它内置了 `validate.mjs` 的规则自检
（键白名单 / semver / HTTPS / icon 扩展名 / sha256 格式 / target 唯一性），
本地就能挡住格式错误，不必等 PR 的 CI 往返。

手工提 PR 时，`candidates/<id>.json` 的 `targets[].url` 必须是 release 资产的**绝对 HTTPS 地址**，
`icon` 也必须是绝对 URL 且扩展名为 SVG/PNG；`source` 应指向**构建这批字节的确切 tag**
（`https://github.com/<owner>/<repo>/tree/v<version>`），审核者据此核对 provenance。

**第三步：DBX Store 审核并签名**

维护者检查源码、Manifest、权限、候选 SHA-256、大小和原生行为。审核通过后受保护的 Workflow 会：按已审核 SHA-256 和大小下载候选包 → 确认没有已有签名并核对插件 ID 和版本 → 用 DBX Store 仓库密钥添加 Ed25519 签名 → 发布最终 `.dbxp`、artifact metadata 和 signing receipt。

**第四步：合并候选 PR**

Workflow 把最终的 `plugins/<plugin-id>.json` 和生成的 `catalog/index.json` 回写到同一候选 PR，发布到 `dbx-store` Release。校验通过后维护者审核并合并。

### 9.3 上架 PR 中不要包含

- `.dbxp` 二进制
- 插件完整源码副本
- Ed25519 私钥、Token 或下载凭据
- 未经 DBX Store 签名的候选包 URL
- 自行伪造的 `verified: true`

生成的 `plugins/<plugin-id>.json` 和 `catalog/index.json` **不要手工编辑**。

### 9.4 更新已有插件

- 每次更新发布**新语义版本**，不允许覆盖旧版本的 Release 资产
- 递增 `manifest.json` 的 `version`（并同步铁律 14 的三处），**不要复用旧版本号**
- 发布新的源码 Tag 和候选 Release
- 等同步 Workflow 创建或更新 `automation/plugin-release/<plugin-id>/<version>` 候选 PR
- 维护者审核、签名并合并

### 9.5 签名

- `publisher` 表示作者归属和商店记录
- `signingKeyId` 表示发布最终安装包的仓库密钥
- **官方商店插件作者不要运行 `keygen`，也不要自行填写 `signingKeyId`**
- 只有运营自己的私有插件仓库时才用 CLI 高级签名工具
- 人工审核决定插件能否展示，Ed25519 仓库签名保证审核后的安装包没被替换
- **签名不能替代源码审查，也不是原生 Sidecar 的操作系统沙箱**

---

## 10. Git 规范

### 10.1 `.gitignore`（强制内容）

```gitignore
# 本地构建缓存（沙箱内 GOCACHE/GOTMPDIR 必须指到项目内，避免被沙箱杀编译进程）
/_gocache/
/_gotmp/

# 编译产物与打包产物
/dist/
/_xbuild/
*.exe
_dbx-plugin-translation

# 校验/探测临时目录
/_verify_tmp/

# 节点依赖（脚本只用 node 内置模块，不需要 node_modules）
/node_modules/

# DBX 开发宿主数据目录（可能含明文凭据，绝不提交）
.dbx-dev/

# 系统/编辑器
.DS_Store
Thumbs.db
.idea
.vscode
.gocache
.gotmp
```

### 10.2 提交信息

用简体中文，动词开头，说明「做了什么 + 为什么」。参考 `dbx-todo` 的风格：

```
初始化第一版翻译插件
重构存储为SQLite
完善导入导出
优化界面功能+增加执行模式
修复子任务回显BUG+执行模式记录看板展开/收缩状态
```

### 10.3 远程

```
origin  https://github.com/vyrnax/dbx-plugin-translation.git
```

当前为空仓库，分支 `main`，尚无提交。

---

## 11. 本机环境（已就绪）

| 组件 | 版本 / 路径 |
|---|---|
| Go | `go1.27.3 windows/amd64` @ `C:\develop\Go` |
| GOPATH | `C:\Users\Lenovo\go` |
| GOPROXY | `https://goproxy.cn,direct` |
| GOSUMDB | `sum.golang.google.cn` |
| Node.js | v24.18.0 |
| npm | 11.16.0 |
| `dbx-plugin` | 0.1.9（`@dbx-app/plugin-cli`） |
| `dbx` CLI | 0.4.127（`@dbx-app/cli`） |
| DBX 桌面端 | `C:\develop\DBX\dbx.exe` |
| Git | 2.31.1.windows.1（`gaoyabin` / `15901055209@163.com`） |
| 官方 Go SDK | `C:\Users\Lenovo\AppData\Roaming\npm\node_modules\@dbx-app\plugin-cli\sdk-root\plugins\sdk\go\dbx-plugin-sdk\sdk.go` |
| 参考插件 | `C:\Project\backend\me\dbx-todo` |
| 项目仓库 | `C:\Project\backend\me\dbx-plugin-translation` |

交叉编译已实测 6 平台全通过（`CGO_ENABLED=0`）。

⚠️ `C:\develop\Go\bin` 是手工加进用户 PATH 的（MSI 用户级安装未自动写入），**新开的终端才生效**。

---

## 12. 给 AI 助手的工作约定

1. **动手前先读本手册**，尤其是第 4 章铁律和第 6 章踩坑清单。
2. **参考 `dbx-todo` 的对应实现**再写自己的代码——它是已上架官方商店、经过真实用户验证的实现。路径 `C:\Project\backend\me\dbx-todo`。
3. **不要凭想象发明 API**。`window.dbxPlugin` 的方法、manifest 字段、RPC 语义一律以官方文档和参考实现为准。manifest v1 不接受未声明字段。
4. **新踩的坑必须写回本手册**第 6 章，格式为「症状 → 根因 → 对策」，并在代码里留下解释「为什么」的注释。这是本项目最重要的知识沉淀机制。
5. **每次改动后跑静态闸门**（`_domcheck.py` + `_verify.mjs`），不要等到打包才发现。
6. **改版本号必须同步三处**（铁律 14）。
7. **生成文件时严禁 BOM**（`\ufeff`）。
8. **临时脚本用完即删**。
9. **不要提交 `dist/`、`.dbx-dev/`、`_gocache/`、`_xbuild/`、`*.exe`**。
10. **涉及密钥的代码要格外谨慎**：任何日志、错误消息、RPC 返回值都不得回显 API Key（铁律 9）。
