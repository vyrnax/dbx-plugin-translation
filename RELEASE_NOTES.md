# Translation 即时翻译 v0.2.3

DBX 多引擎翻译插件的首个公开版本。在 DBX 工作台内提供即时翻译、流式输出、8 类翻译引擎与完整的引擎管理能力。

> 插件 ID：`com.vyrnax.translation` ｜ 最低 DBX 版本：`0.6.21` ｜ 权限：`host.events`

---

## ✨ 核心特性

### 输入即译，流式输出

在原文框打字，停顿后自动翻译，译文逐字出现。大模型引擎走 SSE 真流式，传统机翻引擎自动降级为整段返回，**前端体验完全一致**。

内置竞态防护：快速连续输入时，旧请求的迟到结果会被自动丢弃，不会覆盖新译文；每次新输入前会取消上一个在飞任务，不浪费配额。

### 8 类翻译引擎

| 引擎 | 类型 | 流式 | 需要密钥 | 说明 |
|---|---|---|---|---|
| **大模型翻译** | OpenAI 兼容 | ✅ | ✅ | 覆盖 DeepSeek、通义千问、智谱、Kimi/Moonshot、OpenAI、Ollama、vLLM、LM Studio 等一切实现 `/v1/chat/completions` 的服务 |
| **本地 CLI 翻译** | 子进程代理 | ✅ | ❌ | 调用本机已登录的 `claude` / `codex`，**复用现有登录态与订阅额度，无需申请 API Key** |
| **DeepL** | REST | — | ✅ | 支持免费档 / 付费档、语气正式度、术语表 |
| **Google 翻译** | REST | — | ✅ | Cloud Translation API v2 |
| **百度翻译** | REST | — | ✅ | 通用文本翻译，MD5 签名 |
| **有道翻译** | REST | — | ✅ | 有道智云，SHA256 签名 |
| **阿里云翻译** | REST | — | ✅ | 机器翻译 TranslateGeneral，ACS3-HMAC-SHA256 签名 |
| **腾讯云翻译** | REST | — | ✅ | TMT TextTranslate，TC3-HMAC-SHA256 签名 |

端点地址均可自定义，方便走企业代理或自建网关。

### 引擎管理

- **选类型自动补全名称**：新建时选好引擎类型，名称栏自动填入「阿里云翻译」「DeepL 翻译」这类可读默认名；手动改过之后再切类型不会覆盖你的命名。同类型可添加多个实例，靠名称区分。
- **连通性测试**：保存前后都能一键试连，返回耗时与可理解的中文错误（区分「Key 无效」「端点不可达」「配额用尽」「限流」）。
- 设为默认、启用/禁用、拖拽排序、删除（连带清除密钥）。

### 31 种语言

`auto` 自动检测 + 30 种语言。语言下拉框会**按当前引擎的能力动态禁用**不支持的选项，而不是等你选完了翻译失败。

### 翻译历史

分页、搜索（原文与译文）、收藏、单条/批量删除、一键用历史记录重新翻译。默认保留 500 条（可调 100–5000），超出时自动淘汰最旧的记录，**收藏的永不淘汰**。

### 长文本自动分段

超过引擎单次上限的文本按「段落 → 句子 → 子句 → 硬切」逐级降级切分，并保护代码块、URL、`{placeholder}` 占位符不被切断。分段顺序执行、段间可配延时（默认 1100ms），避免触发百度/有道免费档的 1 QPS 限制。

### 完全跟随 DBX 外观

主题（明暗）、调色板、圆角、UI 字体、编辑器字体、界面语言**全部实时跟随 DBX 设置**，在 DBX 里改完立即生效，无需重开工作台，也无需在插件里单独配置。界面提供简体中文与 English 双语。

---

## 🔒 安全

- **API Key 由系统密钥库加密**：Windows DPAPI（绑定用户账户）、macOS Keychain、Linux secret-tool。三者都不可用时降级为机器绑定 AES-256-GCM，并在界面上**常驻黄色警示条明确告知「仅混淆保护，安全性降低」**，绝不静默降级。
- **界面永远拿不到明文密钥**：保存后只显示掩码指纹（如 `sk-a****mnop`）。
- 密钥不写入日志、事件、错误消息与任何 RPC 返回值；`sidecar-trace.log` 内置脱敏。
- 导出数据**默认不含密钥**；显式勾选包含时，导出文件内写入明文警告。
- 本地 CLI 引擎用参数数组直接 exec，**不经过 shell**，并拒绝含 shell 元字符的参数。
- 译文一律按纯文本渲染，不使用 `innerHTML`，防止上游返回内容造成 XSS。
- 权限最小化：仅声明 `host.events`（流式输出所必需），不申请网络白名单、不申请剪贴板读取。

---

## 📦 安装

1. 下载与你的平台对应的 `.dbxp` 文件
2. 打开 DBX → **插件中心** → 开启「允许安装未签名开发包」
3. 安装下载的 `.dbxp`
4. 从工具栏图标或命令面板打开「Translation 即时翻译」

| 平台 | 文件 |
|---|---|
| Windows x64 | `com.vyrnax.translation-0.2.3-windows-x64.dbxp` |
| macOS Apple Silicon | `com.vyrnax.translation-0.2.3-darwin-arm64.dbxp` |
| macOS Intel | `com.vyrnax.translation-0.2.3-darwin-x64.dbxp` |
| Linux x64 | `com.vyrnax.translation-0.2.3-linux-x64.dbxp` |
| Linux arm64 | `com.vyrnax.translation-0.2.3-linux-arm64.dbxp` |

> 本版本为未签名候选包。提交官方商店审核并签名后，可直接从 DBX 插件中心安装。

---

## 🚀 快速开始

1. 打开插件 → 切到「引擎」页 → 点「添加引擎」
2. 选择引擎类型，名称会自动填好
3. 填入端点与 API Key（本地 CLI 引擎只需确认命令已在 PATH 中，**无需密钥**）
4. 点「测试连接」确认可用 → 保存
5. 回到「翻译」页，选目标语言，直接在原文框打字

按 `Ctrl/Cmd + Enter` 可手动触发翻译，`Esc` 取消进行中的翻译。

---

## 🔎 能力与数据披露（上架审核所需）

这一节把插件**实际会做什么**讲清楚，供商店审核与用户知情。

### 网络访问

- Manifest 只声明 `host.events`（流式输出所必需），**没有**声明 `host.network:*`。
- 但请注意：`host.network` 白名单**不约束原生侧车**。本插件的全部 HTTP 请求都由内置 Go 侧车发出，
  因此**侧车可以访问用户在引擎配置中填写的任意 HTTP/HTTPS 端点**（大模型网关、DeepL、百度、有道、
  阿里云、腾讯云、Google、Ollama 等，8 类引擎的端点均可自定义，以便走企业代理或自建网关）。
- 侧车**不监听任何端口**，不对外提供服务；只通过 stdin/stdout 与宿主交换 JSON Lines 协议消息。
- 默认严格校验 TLS 证书；`skipTlsVerify` 是按引擎的显式开关，开启时界面红色警示并记入追踪日志。
  保存非 HTTPS 且非 localhost 的端点时会给出警告。

### 数据与隐私

- **翻译原文会发往用户所选的第三方翻译服务**（这是翻译功能的固有行为，由用户自行选择引擎与端点）。
- 引擎配置、翻译历史（**原文与译文**）、界面偏好统一存放在
  `<插件数据目录>/translation.db`（SQLite），**以明文形式落盘**，且**卸载插件后仍会保留**。
  可在「历史」页随时单条删除、批量删除或一键清空；也可手工删除整个数据目录。
- **API Key 不以明文落盘**：经系统密钥库加密（Windows DPAPI / macOS Keychain / Linux secret-tool），
  三者都不可用时降级为机器绑定 AES-256-GCM，并在界面上常驻黄色警示条明确告知「仅混淆保护」。
- 界面永远拿不到明文密钥，保存后只显示掩码指纹（如 `sk-a****mnop`）。
- 密钥不写入日志、事件、错误消息与任何 RPC 返回值；`sidecar-trace.log` 内置脱敏。
- 导出数据**默认不含密钥**；显式勾选包含时，导出文件内写入明文警告。
- 诊断报告不含任何 API Key，可放心公开粘贴。
- 本插件**不读取** DBX 自身的数据库（`dbx.db`），也**不读取**用户在 DBX 中配置的 AI 模型信息。

### 原生侧车行为

- 包含 1 个原生可执行文件：`bin/<target>/dbx-plugin-translation[.exe]`（Go 编译，**无 cgo**）。
- 由宿主按插件生命周期启动/终止，通过 stdio 通信；**不创建子进程监听端口，不写插件数据目录以外的位置**。
- 写入的文件仅限插件数据目录内：`translation.db`（含 `-wal`/`-shm`）、`config.json`、`sidecar-trace.log`。
- **`localcli` 引擎会执行本机命令**：调用用户配置的 `claude` / `codex` / 自定义可执行文件。
  实现上用 `exec.CommandContext` 的**参数数组**直接 exec，**不经过 shell**；
  `cliPath` 留空则从 `PATH` 查找，填写则校验为存在的可执行文件；
  `extraArgs` 按行分割为参数数组并逐条拒绝含 shell 元字符的输入；进程受超时控制（默认 120s）后 kill。
  凭据由该 CLI 自身持有，本插件不接触、不存储。

### 代码签名状态

- 5 个 target 的候选包均为**未签名**（不含 `signature.json`），等待商店审核后统一签名。
- `darwin` 侧车目前只有 Go 链接器自动附加的 **ad-hoc 签名**，**未做 Apple notarization**。
  macOS 用户首次打开可能需要在「系统设置 → 隐私与安全性」中放行。
  notarization 需要 Apple Developer 账号，暂无计划；如后续具备条件会单独说明。
- 全部 target 均由 GitHub Actions 在**对应平台的 runner 上真机构建**（不交叉编译），
  构建自公开 tag，Go 工具链 pin 到精确补丁号 `1.21.13`，
  因此 sidecar 内嵌的 `vcs.revision` 可在本仓库中直接核验。

---

## 📝 v0.2.3 变更（界面与引擎管理体验）

### 引擎图标接入界面

- 8 类引擎各自的品牌 logo 出现在**三个位置**：翻译页的引擎选择下拉、新增/编辑引擎弹框的「引擎类型」下拉、引擎列表每行的名称前面。
- 原生 `<select>` 的 `<option>` 无法渲染图片，因此翻译页的引擎选择改为**自绘下拉**（与引擎管理弹框同一套组件）。
- 所有 logo 做过**光学尺寸归一**：用真实渲染引擎测量每个 SVG 的内容包围盒，统一缩放到「最长边占画布 88% 且居中」。
  否则同样的 18px 方槽里，圆形图标会显得比方形 logo 大一圈。
- `localcli` 图标由黑改蓝（`#3B82F6`）：DBX 支持深色主题，黑色图标在深色背景下几乎不可见。
- 图标经宿主 `readAsset` 读成 data URI（沙箱 CSP 的 `img-src` 允许 `data:`），不可用时退回首字母色块，不出现破图。
- **为什么图标文件从 `assets/` 移到 `ui/icons/`**：宿主的 `ui.readAsset` 明确拒绝 UI root 之外的路径
  （实现里抛 `Asset is outside UI root`），放在 `assets/` 下前端根本读不到。

### 插件图标更换并跟随主题

品牌图标换成新设计。topbar 里的图标是**内联 SVG**，其中「文」字用 `fill="var(--brand-fg, currentColor)"`，
因此随明暗主题实时换色；包内的 `assets/plugin.svg`（插件中心/工具栏用的那份）另配了 `prefers-color-scheme` 双保险。

### 底部状态栏锁定

「就绪」状态与版本号固定在窗口最底部，不再随主区内容高度上下漂移；高度与 DBX 侧边栏对齐。
根因是 `#app` 用的是 grid 而**在流子元素比声明的轨道多一个**（topbar + 两条 banner + main + statusbar），
状态栏落到隐式 `auto` 轨道上，「固定轨道」根本没作用到它 —— 改为 flex 列 + `#main { flex:1 1 auto; min-height:0 }`
+ `flex: 0 0 var(--statusbar-h)` 才根治。

### 引擎列表拖拽排序 + 默认引擎永远置顶

- 排序改用**指针事件**实现：原生 HTML5 拖拽（`draggable` + `dragstart`）在宿主的 sandbox iframe 里不触发，
  表现为「六个小白点能看见但拖不动」。
- **默认引擎永远排在第一位且不可拖动**（侧车 `ListEngines` 改为 `ORDER BY is_default DESC, ord ASC, ...`，
  前端在降级内存模式下再排一次，保证两条路径顺序一致）。
- 只要引擎顺序或默认引擎发生变化，翻译页的引擎下拉立即按新顺序重建。

### 记住你上次选择的引擎

选择持久化到偏好（`currentEngineId`），重开插件后仍是上次用的那个引擎，而不是跳回默认引擎。
恢复优先级：本会话选择 > 跨会话偏好 > 默认引擎；只有「引擎顺序或默认引擎变了」才重置下拉，
避免每次刷新列表都把你正在用的引擎顶掉。

### 移除无意义的密钥指纹标记

引擎列表里那把「钥匙 + 4 个星号」只能证明「存过密钥」，信息量低且容易被误读成密钥本身，
改为图标 + 明确的状态文案。密钥依然照原样加密存储，只是不再显示掩码串。

---

## 📝 v0.2.2 变更（商店上架整改）

本版本**不含功能变更**，全部是上架审核整改：

- **补齐 `LICENSE` 文件**（Apache-2.0 全文）。此前 `.dbx-store.json` 声明了 Apache-2.0 但仓库里没有 LICENSE 文件。
- **发布产物改由 CI 从 tag 构建**：新增 `.github/workflows/release.yml`，复用官方
  `t8y2/dbx/.github/workflows/plugin-release-reusable.yml`（pin 到具体 commit），
  按平台矩阵在真机构建 5 个 target 并上传到同名 Release。
  修复了 v0.2.1 的 provenance 硬伤 —— 当时 release 里的 sidecar 是本地另行构建的，
  内嵌的 `vcs.revision` 在公开仓库中查不到。
- **Go 工具链 pin 到 `1.21.13`**（CI 与发布一致），不再用 `"1.21"` 这类浮动版本，保证同一 tag 可复现同一字节。
- **统一 `assets/` 换行符为 LF**：此前 7 个 SVG 在工作区是 CRLF、在 git blob 里是 LF，
  导致本地打包与 CI 打包的 SVG 字节不同、`checksums.json` 随之不同。
- **候选元数据改为商店扁平 schema**：`_release.mjs` 额外产出
  `dist/store-candidate/com.vyrnax.translation.json`（`schemaVersion:1` + `targets[]`，
  url 为 release 资产的绝对 HTTPS 地址），并在本地先跑一遍 `validate.mjs` 的硬规则自检。
  此前提交的是 `{plugin, artifacts}` 格式，被 `validate.mjs` 判为
  `unknown field(s): plugin, artifacts`。
- **Release 转为正式发布**（`prerelease: false`）：商店的 `discover-plugin-releases.mjs`
  只认非 draft、非 prerelease 的 Release。
- **listing 描述补充披露**：在 manifest 与 `.dbx-store.json` 的描述中写明
  「翻译历史以明文 SQLite 落盘、卸载后保留」与「侧车直连用户配置的任意端点」。

---

## ⚠️ 已知限制

- **不支持编辑器划词翻译**：DBX 插件系统没有编辑器选区 API，`context-menu` 仅支持连接与表节点。
- **不支持译文内联提示**：插件 UI 运行在 iframe 沙箱内，无法覆盖宿主 DOM。
- **不支持自定义快捷键**：DBX 没有提供 keybinding 贡献点。
- **浮动窗口「复制即翻译」尚未实现**：需要 `host.clipboard:read` 权限，计划在后续版本提供。
- **表/字段注释批量翻译尚未实现**：需要 `host.data:read`（Host API 1.4），会把最低宿主版本抬高，计划在后续版本单独评估。
- 5 个 target 均由对应平台的 CI runner **真机构建**（v0.2.2 起不再交叉编译），并各自跑过包结构与侧车协议探针校验；但**尚未在对应真机的 DBX 桌面端完整冒烟**。如遇到问题请反馈。

---

## 🐛 排查

遇到问题时，打开插件右上角「⋯ → 查看诊断报告」，一键复制后提交 Issue。报告包含环境快照、侧车状态、密钥保护方案、主题与语言的实际生效值，以及最近 150 条分步日志。

**报告不含任何 API Key**（自动脱敏），可以放心公开粘贴。

侧车日志位于插件数据目录下的 `sidecar-trace.log`（Windows 通常在 `%APPDATA%\com.dbx.app\plugin-data\com.vyrnax.translation\`）。

---

## 📄 许可

Apache-2.0
