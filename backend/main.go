// DBX Translation 即时翻译 —— 原生侧车（Go，纯标准库 + vendor 的官方 SDK + modernc.org/sqlite）
//
// # 侧车 RPC 一览
//
//	translate/ping      —— 版本 / 数据目录 / 密钥方案 / 库健康 / 在飞任务数
//	translate/schema    —— 全部引擎的表单描述 + 规范语言表（前端据此动态渲染）
//	engines/list        —— 引擎列表（不含任何密钥明文）
//	engines/get         —— 单个引擎（不含密钥明文）
//	engines/save        —— 新增/更新引擎（密钥在此加密后落盘）
//	engines/delete      —— 删除引擎（连带删除密钥）
//	engines/setDefault  —— 设为默认引擎
//	engines/toggle      —— 启用/禁用
//	engines/reorder     —— 拖拽排序
//	engines/test        —— 连通性探测（真实试译一个短词）
//	translate/run       —— 非流式翻译（一次性返回完整译文）
//	translate/start     —— 流式翻译（立即返回 taskId，随后发事件）
//	translate/cancel    —— 取消在飞任务
//	history/list        —— 分页查询历史
//	history/star        —— 收藏/取消收藏
//	history/delete      —— 批量删除
//	history/clear       —— 清空（可保留收藏）
//	prefs/get           —— 读界面偏好
//	prefs/set           —— 写界面偏好（带 rev 乐观锁）
//	data/export         —— 导出全部数据（默认不含密钥）
//	data/import         —— 导入数据
//
// # 侧车发出的事件（需 manifest 声明 host.events）
//
//	translate/chunk  { taskId, text, index, total }
//	translate/end    { taskId, target, elapsedMs, engine, detectedLang, chunks }
//	translate/error  { taskId, code, message }
//
// 为什么用自定义事件而不用宿主的 stream() 桥接方法：
// stream() 的 closeMethod 默认值是 "filesystem/stream/close"，说明它是为
// 文件流设计的，桌面端对非文件方法的行为没有文档保证。onEvent + 自定义事件
// 只依赖 host.events 权限（官方文档明确说明「转发后端事件需要 host.events」），
// 可移植性更可靠。
//
// # 三条铁律（违反即静默失败，详见 AGENTS.md 第 4 章）
//
//  1. stdout 只能输出协议消息。任何 fmt.Println 都会污染 JSON Lines 流，
//     宿主解析失败后直接丢弃侧车，前端表现为「什么都存不上」且无报错。
//     日志一律走 trace 包（写文件）。
//  2. 密钥明文绝不写入日志、事件、错误消息或任何 RPC 返回值。
//  3. plugin.id / version 必须与 manifest 一致，否则宿主判定身份不匹配并丢弃侧车。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vyrnax/translation/chunk"
	dbxpluginsdk "github.com/vyrnax/translation/dbxsdk"
	"github.com/vyrnax/translation/engine"
	// free 是【具名】导入而不是空白导入：内置引擎除了靠 init() 注册进工厂，
	// main 还需要它的 Kind / NameZh / SeedConfig 来初始化存储层里的内置记录。
	"github.com/vyrnax/translation/engine/free"
	// 引擎注册：每个子包在 init() 里调用 engine.Register，
	// 这里用空白导入触发。新增引擎 = 新增一个子包 + 这里加一行，
	// 前端零改动（表单由 translate/schema 动态渲染）。
	//
	// 导入顺序即 UI 里引擎类型下拉框的展示顺序。
	_ "github.com/vyrnax/translation/engine/aliyun"   // 阿里云翻译（ACS3-HMAC-SHA256）
	_ "github.com/vyrnax/translation/engine/baidu"    // 百度翻译（MD5 签名）
	_ "github.com/vyrnax/translation/engine/deepl"    // DeepL（REST，无流式）
	_ "github.com/vyrnax/translation/engine/google"   // Google 翻译（API Key）
	_ "github.com/vyrnax/translation/engine/localcli" // 本地 CLI 代理（claude / codex，无需 API Key）
	_ "github.com/vyrnax/translation/engine/openai"   // OpenAI 兼容大模型（SSE 流式）
	_ "github.com/vyrnax/translation/engine/tencent"  // 腾讯云翻译（TC3-HMAC-SHA256）
	_ "github.com/vyrnax/translation/engine/youdao"   // 有道翻译（SHA256 签名）
	"github.com/vyrnax/translation/lang"
	"github.com/vyrnax/translation/secret"
	"github.com/vyrnax/translation/store"
	"github.com/vyrnax/translation/trace"
	"github.com/vyrnax/translation/wordsplit"
)

// 必须与 manifest.json 的 id / version 完全一致，否则宿主判定 Sidecar 身份不匹配并丢弃。
//
// pluginVersion 只是【兜底】：resolveMetadata() 会在运行时从包内 manifest.json
// 反读真实版本号。这样做的原因是版本号需要在三处同步
// （manifest.json / 本常量 / ui/storage.js 的 UI_VERSION），
// 人工同步迟早会漏，而漏了的后果是侧车被宿主静默丢弃。
// 运行时反读保证了「包里的 manifest 说什么版本，侧车就自报什么版本」。
const (
	pluginID      = "com.vyrnax.translation"
	pluginVersion = "0.2.5"
)

// 全局在飞翻译任务上限。
//
// 为什么要有上限：每个在飞任务都持有一个 goroutine + 一条到上游的 HTTP 连接。
// 前端的「输入即译」在快速打字时会连续发起任务，虽然每次都会取消上一个，
// 但取消是异步的（要等上游响应），极端情况下可能堆积。
// 4 是一个平衡值：够用户同时翻译几段文本，又不会让上游配额被瞬间打爆。
const maxInflightTasks = 4

// maxEngineNameUnits 是引擎名称的显示宽度上限：全角字符计 2 个单位、半角计 1 个单位，
// 所以 32 单位约等于「16 个汉字」或「32 个字母」，或二者任意混合。
//
// 为什么必须有上限：名称同时出现在三处窄位置 —— 翻译页的引擎下拉、引擎列表行、历史列表。
// 没有上限时用户可以起一个几十字的名称，把下拉撑成多行、把行首图标与右侧按钮挤走，
// 界面变形且他自己也认不出哪台是哪台。
//
// 为什么按【显示宽度】而不是字符数：界面上受限的本质是「占多少地方」。
// 同样 16 个字符，「阿里云翻译（生产备用）」的宽度约是 16 个 ASCII 字母的两倍，
// 按统一字符数计就会让中文名称更容易撑破下拉框。
//
// 有了硬上限，前端就不需要「省略号 + 鼠标悬浮看全称」那套补救 —— 名称必然放得下。
// CSS 里仍保留 nowrap + ellipsis 作为最后一道保险，只在宿主把 UI 字体调得很大时才可能触发；
// 那时宁可截断也绝不换行，因为换行会把整排选项的行高撑开，比截断难看得多。
const maxEngineNameUnits = 32

// isWideRune 判断字符是否占两个显示宽度（CJK / 全角标点 / 假名 / 韩文音节等）。
//
// 区间表与 ui/storage.js 的 isWideChar 一一对应 —— 两处必须同一规则，
// 否则前端放过的名称会被后端拒绝，用户看到的是「明明没超长却保存失败」。
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母 Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK 部首与标点（含（）、，。～）
		r >= 0x3041 && r <= 0x33FF, // 假名、注音、CJK 兼容符号
		r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF, // CJK 基本汉字
		r >= 0xA000 && r <= 0xA4CF, // 彝文
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意文字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式与小写形式
		r >= 0xFF00 && r <= 0xFF60, // 全角 ASCII 与全角标点
		r >= 0xFFE0 && r <= 0xFFE6: // 全角货币与符号
		return true
	}
	return false
}

// nameWidthUnits 返回字符串占用的显示宽度单位数。
func nameWidthUnits(s string) int {
	units := 0
	for _, r := range s {
		if isWideRune(r) {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// translateTestTimeoutSecs 是连通性探测的超时。
//
// 比正常翻译短：探测失败时用户正在等着看结果，60 秒的等待不可接受。
// 15 秒足够覆盖慢速上游的正常握手 + 一次短文本推理。
const translateTestTimeoutSecs = 15

// plugin 是 RPC handler 的实现。
//
// SDK 对每个请求开一个 goroutine，因此所有可变状态都必须加锁。
type plugin struct {
	mu sync.Mutex

	store     *store.Store
	protector secret.Protector
	dataDir   string

	// tasks 是在飞的流式翻译任务表。
	// 完成/失败/取消三条路径都必须从中删除，否则 goroutine 与 ctx 泄漏。
	tasks map[string]*task

	// semaphores 是每引擎的并发信号量，尊重上游 QPS 限制。
	semaphores map[string]chan struct{}

	// builtinTpl 是内置引擎的定义模板，启动时由 EnsureBuiltin 写库，
	// data/import 之后要用它再兜一次底（导入的旧备份里可能没有这条记录）。
	builtinTpl store.Engine
}

// task 是一个在飞的流式翻译任务。
type task struct {
	id     string
	cancel context.CancelFunc
	// engineID 用于诊断（哪个引擎的任务卡住了）
	engineID string
	started  time.Time
}

// taskCount 返回在飞任务数，供 translate/ping 的诊断字段使用。
func (p *plugin) taskCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tasks)
}

// ---------------------------------------------------------------------------
// 错误构造辅助
// ---------------------------------------------------------------------------

// RPCError 是 SDK 的错误类型别名，减少重复书写。
type RPCError = dbxpluginsdk.PluginError

// badParams 构造「参数非法」错误（-32602）。
func badParams(format string, args ...any) *RPCError {
	return dbxpluginsdk.NewError(-32602, fmt.Sprintf(format, args...))
}

// failed 把底层错误包装成 RPC 错误。
//
// 错误消息会直接展示给用户，因此必须是可理解的中文，
// 并尽可能带上可操作的下一步。技术细节进 trace 日志而非消息本身。
func failed(code int, err error) *RPCError {
	return dbxpluginsdk.NewError(code, trace.Redact(err.Error()))
}

// engineErr 把引擎错误转成对用户友好的 RPC 错误。
//
// 这里做两件事：
//  1. 用 engine.Classify 归一化错误类型，拿到正确的错误码
//  2. 把技术细节写进 trace 日志（用户看不到，但排查时是关键线索）
//
// 【绝不】把 err 的完整内容塞进用户消息：上游返回的错误体可能是几 KB 的 HTML。
func engineErr(err error) *RPCError {
	e := engine.Classify(err)
	if e == nil {
		return dbxpluginsdk.NewError(-32030, "翻译失败")
	}
	trace.Log("引擎错误 kind=%s code=%d msg=%s detail=%s",
		e.KindName(), e.Code(), e.Message, e.Detail)
	msg := e.Message
	if e.Detail != "" && len([]rune(msg)) < 120 {
		// 消息够短时附带细节，帮助用户自助排查
		msg = msg + "（" + e.Detail + "）"
	}
	return dbxpluginsdk.NewError(e.Code(), trace.Redact(msg))
}

// ---------------------------------------------------------------------------
// 数据目录与配置持久化
// ---------------------------------------------------------------------------

// dataDir 返回插件数据目录，三级回退。
//
//	DBX_PLUGIN_DATA_DIR → DBX_PLUGIN_SPACE → os.UserConfigDir()/dbx/plugins/<id> → ./data
//
// 为什么需要回退：开发宿主与桌面端注入的环境变量名可能不同，
// 而「拿不到数据目录」不应该让侧车直接崩溃 —— 退到 ./data 至少能跑起来，
// 用户的数据也不会丢（只是位置不理想）。
func dataDir() string {
	if d := strings.TrimSpace(os.Getenv("DBX_PLUGIN_DATA_DIR")); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv("DBX_PLUGIN_SPACE")); d != "" {
		return d
	}
	if base, err := os.UserConfigDir(); err == nil && base != "" {
		return filepath.Join(base, "dbx", "plugins", pluginID)
	}
	return filepath.Join(".", "data")
}

// configPath 是侧车自己的配置文件（记录数据目录，供重启后还原）。
func configPath() string { return filepath.Join(dataDir(), "config.json") }

// loadConfig 在侧车启动时还原上次的状态。
//
// 这是 dbx-todo 踩过的坑：如果只在运行时写配置而从不在启动时读，
// 跨进程重启后状态会回到空值，数据库被解析到兜底位置（空库），
// 用户看到的现象是「数据全没了」。
func loadConfig() {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	if d, ok := m["dataDir"].(string); ok && strings.TrimSpace(d) != "" {
		trace.Log("loadConfig: 还原 dataDir=%s", d)
	}
}

// writeConfig 持久化侧车配置（原子写：临时文件 + rename）。
//
// 原子写是必要的：直接覆盖写文件时如果进程在写到一半被杀，
// 会留下一个截断的 JSON，下次启动解析失败，配置全丢。
func writeConfig(values map[string]any) {
	b, err := json.Marshal(values)
	if err != nil {
		return
	}
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	// rename 在同一文件系统内是原子操作
	if err := os.Rename(tmp, configPath()); err != nil {
		_ = os.Remove(tmp)
	}
}

// ---------------------------------------------------------------------------
// 身份解析
// ---------------------------------------------------------------------------

// resolveMetadata 从包内 manifest.json 反读版本号，构造 SDK 需要的身份信息。
//
// 从 os.Executable() 所在目录向上最多找 6 层：
// 打包后侧车位于 <安装目录>/bin/<target>/<binary>，manifest.json 在安装目录根部，
// 向上 2 层就能找到；开发期侧车在 backend/ 下，向上 1 层是项目根。
// 6 层留了足够余量，又不会因为一直找到磁盘根目录而变慢。
//
// 只有 manifest 的 id 与常量一致时才采用它的 version ——
// 否则可能读到别的项目的 manifest，自报一个错误的版本号。
func resolveMetadata() dbxpluginsdk.Metadata {
	// capabilities 声明后端能力，宿主据此判断侧车支持哪些特性。
	// 这里的 "translation" 是本插件的业务能力标识。
	caps := []string{"translation", "engines", "history"}
	fallback := dbxpluginsdk.Metadata{ID: pluginID, Version: pluginVersion, Capabilities: caps}

	exe, err := os.Executable()
	if err != nil {
		return fallback
	}
	dir := filepath.Dir(exe)
	for i := 0; i < 6; i++ {
		data, readErr := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if readErr == nil {
			var m struct {
				ID      string `json:"id"`
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &m) == nil && m.ID == pluginID && strings.TrimSpace(m.Version) != "" {
				fallback.Version = strings.TrimSpace(m.Version)
				trace.Log("resolveMetadata: 从 %s 反读到版本 %s", filepath.Join(dir, "manifest.json"), fallback.Version)
			}
			return fallback
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // 已到文件系统根
		}
		dir = parent
	}
	return fallback
}

// ---------------------------------------------------------------------------
// RPC 分发
// ---------------------------------------------------------------------------

// Handle 是 SDK 回调的入口，用 switch 平铺分发全部方法。
//
// 为什么用平铺 switch 而不是反射注册表：
// 平铺写法一眼能看全所有对外方法，静态检查工具能覆盖，
// 而且不存在「注册名拼错导致方法静默不存在」这类问题。
// 反射注册表省的那点代码，远不值得换来这些排查成本。
func (p *plugin) Handle(
	_ dbxpluginsdk.RequestContext,
	method string,
	params json.RawMessage,
	emitter *dbxpluginsdk.Emitter,
) (any, *RPCError) {
	values := map[string]any{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &values); err != nil {
			return nil, badParams("请求参数不是合法的 JSON 对象")
		}
	}

	trace.Log("RPC %s", method)

	switch method {
	// ---------------- 诊断与元信息 ----------------
	case "translate/ping":
		return p.handlePing()

	case "translate/schema":
		return p.handleSchema()

	// ---------------- 引擎管理 ----------------
	case "engines/list":
		return p.handleEnginesList()

	case "engines/get":
		return p.handleEnginesGet(values)

	case "engines/save":
		return p.handleEnginesSave(values)

	case "engines/delete":
		return p.handleEnginesDelete(values)

	case "engines/setDefault":
		return p.handleEnginesSetDefault(values)

	case "engines/toggle":
		return p.handleEnginesToggle(values)

	case "engines/reorder":
		return p.handleEnginesReorder(values)

	case "engines/test":
		return p.handleEnginesTest(values)

	// ---------------- 翻译 ----------------
	case "translate/run":
		return p.handleTranslateRun(values)

	case "translate/start":
		return p.handleTranslateStart(values, emitter)

	case "translate/cancel":
		return p.handleTranslateCancel(values)

	// ---------------- 历史 ----------------
	case "history/list":
		return p.handleHistoryList(values)

	case "history/star":
		return p.handleHistoryStar(values)

	case "history/delete":
		return p.handleHistoryDelete(values)

	case "history/clear":
		return p.handleHistoryClear(values)

	// ---------------- 偏好 ----------------
	case "prefs/get":
		return p.handlePrefsGet()

	case "prefs/set":
		return p.handlePrefsSet(values)

	// ---------------- 数据导入导出 ----------------
	case "data/export":
		return p.handleDataExport(values)

	case "data/import":
		return p.handleDataImport(values)

	default:
		return nil, dbxpluginsdk.MethodNotFound(method)
	}
}

// ---------------------------------------------------------------------------
// 参数解析辅助
// ---------------------------------------------------------------------------

// strArg 取字符串参数，缺失或类型不符时返回 def。
func strArg(values map[string]any, key, def string) string {
	if v, ok := values[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// intArg 取整数参数并夹到 [lo, hi]。
//
// JSON 数字统一反序列化为 float64，必须处理这个类型。
func intArg(values map[string]any, key string, def, lo, hi int) int {
	v, ok := values[key]
	if !ok || v == nil {
		return def
	}
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case int:
		n = float64(t)
	case int64:
		n = float64(t)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(t, "%d", &parsed); err != nil {
			return def
		}
		n = float64(parsed)
	default:
		return def
	}
	if n != n || n > 1e18 || n < -1e18 { // NaN / Inf
		return def
	}
	i := int(n)
	if i < lo {
		return lo
	}
	if i > hi {
		return hi
	}
	return i
}

// boolArg 取布尔参数。
func boolArg(values map[string]any, key string, def bool) bool {
	if v, ok := values[key]; ok && v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// revArg 取前端回声的 rev。
//
// 返回 -1 表示「前端没传 rev」，此时跳过乐观锁校验。
// 这个语义很重要：首次加载时前端还没有 rev，如果强制要求就会死锁
// （拿不到 rev → 不能写 → 永远拿不到 rev）。
func revArg(values map[string]any) int {
	v, ok := values["rev"]
	if !ok || v == nil {
		return -1
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	}
	return -1
}

// requireStore 确认存储层可用。
//
// 存储打开失败时侧车仍会启动（否则用户连「看到错误提示」的机会都没有），
// 但所有需要读写的方法都必须先过这道检查，给出明确的错误而不是 panic。
func (p *plugin) requireStore() *RPCError {
	if p.store == nil {
		return dbxpluginsdk.NewError(-32002,
			"数据存储不可用：无法打开 translation.db。请检查插件数据目录的读写权限，或查看 sidecar-trace.log 了解详情。")
	}
	return nil
}

// ---------------------------------------------------------------------------
// translate/ping 与 translate/schema
// ---------------------------------------------------------------------------

// handlePing 返回侧车健康状态。
//
// 这是前端启动时的握手方法，也是「插件到底活没活着」的唯一可靠判据。
// 返回的每个字段都对应前端的一个决策：
//   - dbOk=false      → 前端进入降级模式（会话内存后端 + 明确标注未持久化）
//   - secretSecure=false → 前端显示黄色警示条（密钥仅混淆保护）
//   - inflight       → 诊断报告里展示，用于排查 goroutine 泄漏
func (p *plugin) handlePing() (any, *RPCError) {
	meta := resolveMetadata()
	dbOk := p.store != nil
	scheme, secure := "", false
	if p.protector != nil {
		scheme = p.protector.Scheme()
		secure = p.protector.Secure()
	}
	result := map[string]any{
		"ok":           true,
		"plugin":       pluginID,
		"version":      meta.Version,
		"dataDir":      p.dataDir,
		"dbOk":         dbOk,
		"secretScheme": scheme,
		"secretSecure": secure,
		"inflight":     p.taskCount(),
		"engineKinds":  engine.Kinds(),
	}
	if dbOk {
		result["dbPath"] = p.store.Path()
		if rev, err := p.store.Rev(); err == nil {
			result["rev"] = rev
		}
	}
	return result, nil
}

// handleSchema 返回全部引擎的表单描述与规范语言表。
//
// 这是「新增引擎前端零改动」的关键：前端不硬编码任何引擎的字段，
// 完全按这份 schema 动态渲染表单。
func (p *plugin) handleSchema() (any, *RPCError) {
	schemas := engine.Schemas()
	kinds := make([]map[string]any, 0, len(schemas))
	for _, s := range schemas {
		kinds = append(kinds, map[string]any{
			"kind":    s.Kind,
			"label":   s.Label,
			"labelEn": s.LabelEn,
			// icon 是图标文件名（相对前端 ui/icons/ 目录），由各引擎在自己的 schema 里声明。
			// 为什么这里要显式列出：handleSchema 是手工挑字段组装 map 的（不是直接序列化
			// engine.Schema 结构体），漏一行前端就拿不到该字段 —— 新增 Schema 字段时必须
			// 同步这里。前端拿到空值会退化为「首字母色块」，不会报错。
			"icon": s.Icon,
			// defaultName / defaultNameEn 供前端「选类型自动补全名称」使用。
			// 同一类型可以添加多个实例（例如两个不同的 OpenAI 兼容端点），
			// 用户必须靠名称区分，所以默认名要能一眼看出是哪家服务。
			"defaultName":   s.DefaultName,
			"defaultNameEn": s.DefaultNameEn,
			"description":   s.Description,
			"descriptionEn": s.DescriptionEn,
			"streaming":     s.Streaming,
			"maxChars":      s.MaxChars,
			"needsSecret":   s.NeedsSecret,
			"secretLabel":   s.SecretLabel,
			// builtin=true 的类型：前端要从「添加引擎」的类型下拉里排除它，
			// 并对它的列表行隐藏「编辑 / 删除」。
			"builtin":    s.Builtin,
			"noThrottle": s.NoThrottle,
			"fields":     s.Fields,
			// 该引擎支持的语言：前端据此禁用下拉框里不支持的选项，
			// 而不是等用户选了之后翻译失败。
			"languages":       lang.SupportedLangs(s.Kind),
			"targetLanguages": lang.SupportedTargetLangs(s.Kind),
			"supportsAuto":    lang.SupportsAutoSource(s.Kind),
		})
	}
	return map[string]any{
		"kinds":     kinds,
		"languages": lang.All(),
	}, nil
}

// ---------------------------------------------------------------------------
// 引擎管理
// ---------------------------------------------------------------------------

// engineToJSON 把存储层的 Engine 转成 RPC 返回结构。
//
// 【关键安全点】：这里显式列出要返回的字段，而不是直接序列化整个结构体。
// 这样即使将来有人给 store.Engine 加了敏感字段，也不会自动泄露到前端。
func engineToJSON(e store.Engine) map[string]any {
	return map[string]any{
		"id":          e.ID,
		"name":        e.Name,
		"kind":        e.Kind,
		"config":      e.Config,
		"hasSecret":   e.HasSecret,
		"fingerprint": e.Fingerprint,
		"isDefault":   e.IsDefault,
		"enabled":     e.Enabled,
		"ord":         e.Ord,
		"builtin":     e.Builtin,
		"createdAt":   e.CreatedAt,
		"updatedAt":   e.UpdatedAt,
	}
}

func (p *plugin) handleEnginesList() (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	list, err := p.store.ListEngines()
	if err != nil {
		return nil, failed(-32002, err)
	}
	rev, err := p.store.Rev()
	if err != nil {
		return nil, failed(-32002, err)
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, engineToJSON(e))
	}
	return map[string]any{"engines": out, "rev": rev}, nil
}

func (p *plugin) handleEnginesGet(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(strArg(values, "id", ""))
	if id == "" {
		return nil, badParams("缺少引擎 id")
	}
	e, err := p.store.GetEngine(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, dbxpluginsdk.NewError(-32602, "引擎不存在或已被删除")
		}
		return nil, failed(-32002, err)
	}
	rev, _ := p.store.Rev()
	return map[string]any{"engine": engineToJSON(e), "rev": rev}, nil
}

// handleEnginesSave 新增或更新引擎。
//
// 密钥处理流程（对应 AGENTS.md 铁律 9）：
//  1. 从参数里取出明文 secrets（只在这一刻存在）
//  2. 立即用 protector 加密
//  3. 密文与方案标识写入 secrets 表
//  4. 返回值只含 fingerprint，【绝不】回传明文
//  5. 明文变量随函数返回被 GC，不写入任何日志
//
// 特殊约定：secrets 里传空字符串表示「清除已保存的密钥」，
// 传 null 或不传该字段表示「保持原密钥不变」。
// 这个区分很重要：用户改引擎名称时不应该被迫重新输入 API Key。
func (p *plugin) handleEnginesSave(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	if p.protector == nil {
		return nil, dbxpluginsdk.NewError(-32035, "密钥保护器未初始化，无法保存 API Key")
	}

	kind := strings.TrimSpace(strArg(values, "kind", ""))
	if kind == "" {
		return nil, badParams("缺少引擎类型 kind")
	}
	if _, _, ok := engine.Lookup(kind); !ok {
		return nil, badParams("未知的引擎类型：%s", kind)
	}

	id := strings.TrimSpace(strArg(values, "id", ""))
	isNew := id == ""
	if isNew {
		id = newID("eng")
	}

	name := strings.TrimSpace(strArg(values, "name", ""))
	if name == "" {
		name = defaultEngineName(kind)
	}
	// 长度校验必须落在后端：输入框的 maxlength 与前端实时截断都拦不住手工构造的 RPC，
	// 也拦不住 data/import 恢复来的旧备份。
	// 内置引擎由存储层直接播种、不经过本函数，因此不受这条校验影响。
	if u := nameWidthUnits(name); u > maxEngineNameUnits {
		return nil, badParams(
			"引擎名称过长（占 %d 个宽度单位，最多 %d 个：约 16 个汉字或 32 个字母）。请缩短名称。",
			u, maxEngineNameUnits)
	}

	config := map[string]any{}
	if raw, ok := values["config"].(map[string]any); ok {
		config = raw
	}

	// 端点类引擎：保存前校验地址，并把警告回传给 UI
	warning := ""
	if ep, ok := config["endpoint"].(string); ok && strings.TrimSpace(ep) != "" {
		_, w, err := engine.ValidateEndpoint(ep)
		if err != nil {
			return nil, engineErr(err)
		}
		warning = w
	}
	// skipTlsVerify 开启时追加警告：这个选项会让连接可被中间人攻击
	if engine.ConfigBool(engine.Config(config), "skipTlsVerify", false) {
		w := "已开启「跳过 TLS 证书校验」，连接可被中间人攻击。仅建议在自签名证书的内网环境下使用。"
		if warning != "" {
			warning += " " + w
		} else {
			warning = w
		}
		trace.Log("engines/save: 引擎 %s 开启了 skipTlsVerify", id)
	}

	rev := revArg(values)
	enabled := boolArg(values, "enabled", true)
	isDefault := boolArg(values, "isDefault", false)
	ord := intArg(values, "ord", 0, 0, 10000)

	// 先读旧记录，用于判断密钥是「保持」「更新」还是「清除」
	var existing store.Engine
	if !isNew {
		old, err := p.store.GetEngine(id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, dbxpluginsdk.NewError(-32602, "引擎不存在或已被删除")
			}
			return nil, failed(-32002, err)
		}
		existing = old
		// 未显式指定时沿用原有的默认/启用状态与排序
		if _, ok := values["isDefault"]; !ok {
			isDefault = existing.IsDefault
		}
		if _, ok := values["enabled"]; !ok {
			enabled = existing.Enabled
		}
		if _, ok := values["ord"]; !ok {
			ord = existing.Ord
		}
	}

	rec := store.Engine{
		ID:          id,
		Name:        name,
		Kind:        kind,
		Config:      config,
		HasSecret:   existing.HasSecret,
		Fingerprint: existing.Fingerprint,
		IsDefault:   isDefault,
		Enabled:     enabled,
		Ord:         ord,
	}

	newRev, err := p.store.SaveEngine(rec, rev)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return conflictResult(p.store), nil
		}
		return nil, failed(-32002, err)
	}

	// 处理密钥：区分「不传=保持」「空串=清除」「有值=更新」
	fingerprint := rec.Fingerprint
	secretAction := "unchanged"
	if rawSec, ok := values["secrets"]; ok && rawSec != nil {
		secMap, _ := rawSec.(map[string]any)
		plaintext := extractSecrets(secMap)
		if plaintext == "" {
			// 显式传空 → 清除密钥
			if err := p.store.DeleteSecret(id); err != nil {
				trace.Log("engines/save: 清除密钥失败 id=%s err=%v", id, err)
			}
			fingerprint = ""
			secretAction = "cleared"
		} else {
			cipher, err := p.protector.Protect([]byte(plaintext))
			if err != nil {
				return nil, failed(-32035, fmt.Errorf("加密 API Key 失败: %w", err))
			}
			fingerprint = secret.Fingerprint(plaintext)
			if err := p.store.SaveSecret(id, cipher, p.protector.Scheme(), fingerprint); err != nil {
				return nil, failed(-32002, err)
			}
			secretAction = "updated"
			// 明文到此为止：不写日志、不进返回值
		}
	}

	trace.Log("engines/save ok id=%s kind=%s new=%v secret=%s rev=%d",
		id, kind, isNew, secretAction, newRev)

	result := map[string]any{
		"ok":           true,
		"id":           id,
		"rev":          newRev,
		"fingerprint":  fingerprint,
		"hasSecret":    fingerprint != "",
		"secretAction": secretAction,
	}
	if warning != "" {
		result["warning"] = warning
	}
	return result, nil
}

// extractSecrets 从前端传来的 secrets 对象里取出密钥明文。
//
// 兼容多种字段名：不同引擎的密钥叫法不同（apiKey / secretKey / appSecret /
// accessKeySecret），前端按 schema 的 Secret 字段名传，这里统一收集。
//
// 只取第一个非空值：一个引擎只有一把密钥，多个非空值说明前端有 bug，
// 取第一个比报错更宽容（用户至少能保存成功）。
func extractSecrets(m map[string]any) string {
	if m == nil {
		return ""
	}
	// 优先按常见字段名顺序取，保证行为可预测
	for _, key := range []string{"apiKey", "api_key", "secretKey", "secret_key",
		"appSecret", "app_secret", "accessKeySecret", "access_key_secret",
		"key", "token", "password"} {
		if v, ok := m[key]; ok && v != nil {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	// 兜底：取任意一个非空字符串值
	for _, v := range m {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// defaultEngineName 为未命名的引擎生成一个可读的默认名。
// defaultEngineName 返回引擎类型的默认名称。
//
// 【从 schema 读取，不再维护第二份名称表】：
// 之前这里硬编码了一个 kind → 名称的 map，与各引擎 schema.go 里的
// DefaultName 字段重复。两份定义必然逐渐漂移 —— 新增引擎时改了 schema
// 却忘了改这里，用户就会看到「aliyun」这种原始 kind 而不是「阿里云翻译」。
//
// 现在单一事实来源是 schema.DefaultName，前端自动补全与后端兜底
// 用的是同一个值，不可能不一致。
func defaultEngineName(kind string) string {
	if _, sch, ok := engine.Lookup(kind); ok {
		if sch.DefaultName != "" {
			return sch.DefaultName
		}
		if sch.Label != "" {
			return sch.Label
		}
	}
	return kind
}

func (p *plugin) handleEnginesDelete(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(strArg(values, "id", ""))
	if id == "" {
		return nil, badParams("缺少引擎 id")
	}
	newRev, err := p.store.DeleteEngine(id, revArg(values))
	if err != nil {
		return p.mapStoreErr(err)
	}
	trace.Log("engines/delete ok id=%s rev=%d", id, newRev)
	return map[string]any{"ok": true, "rev": newRev}, nil
}

func (p *plugin) handleEnginesSetDefault(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(strArg(values, "id", ""))
	if id == "" {
		return nil, badParams("缺少引擎 id")
	}
	newRev, err := p.store.SetDefaultEngine(id, revArg(values))
	if err != nil {
		return p.mapStoreErr(err)
	}
	return map[string]any{"ok": true, "rev": newRev}, nil
}

func (p *plugin) handleEnginesToggle(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(strArg(values, "id", ""))
	if id == "" {
		return nil, badParams("缺少引擎 id")
	}
	enabled := boolArg(values, "enabled", true)
	newRev, err := p.store.SetEngineEnabled(id, enabled, revArg(values))
	if err != nil {
		return p.mapStoreErr(err)
	}
	return map[string]any{"ok": true, "rev": newRev, "enabled": enabled}, nil
}

func (p *plugin) handleEnginesReorder(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	raw, ok := values["ids"].([]any)
	if !ok || len(raw) == 0 {
		return nil, badParams("缺少 ids 数组")
	}
	ids := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			ids = append(ids, strings.TrimSpace(s))
		}
	}
	if len(ids) == 0 {
		return nil, badParams("ids 数组为空")
	}
	newRev, err := p.store.ReorderEngines(ids, revArg(values))
	if err != nil {
		return p.mapStoreErr(err)
	}
	return map[string]any{"ok": true, "rev": newRev}, nil
}

// mapStoreErr 把存储层错误统一映射为 RPC 错误。
//
// 集中处理的原因：rev 冲突需要返回特殊的 {conflict, newRev} 结构
// 让前端能自动刷新重试，这个逻辑如果在每个 handler 里重复写，
// 迟早会有一处漏掉，表现为「某个操作遇到冲突时前端卡住不动」。
// conflictResult 构造 rev 冲突的返回值。
//
// 【只返回 result，不返回 PluginError】—— 这是被 SDK 行为逼出来的设计：
//
//	dbxpluginsdk 的 Emitter.respond() 在 pluginError != nil 时
//	只写 {"jsonrpc","id","error"}，把 result 整个丢掉。
//	因此若同时返回 {conflict,newRev} 与 -32020 错误，前端只能看到错误，
//	拿不到 newRev，也就无法自动刷新重试 —— 而前端的 withRevRetry
//	正是靠 res.conflict === true 触发的。
//
// 所以冲突被设计成「业务结果」而非「协议错误」：ok=false + conflict=true + newRev。
// 错误码 -32020 仍写在 code 字段里，便于日志与诊断对齐错误码表。
func conflictResult(st *store.Store) map[string]any {
	cur := 0
	if st != nil {
		cur, _ = st.Rev()
	}
	return map[string]any{
		"ok":       false,
		"conflict": true,
		"code":     -32020,
		"newRev":   cur,
		"message":  "配置已被其它会话修改，请刷新后重试",
	}
}

func (p *plugin) mapStoreErr(err error) (any, *RPCError) {
	if errors.Is(err, store.ErrConflict) {
		return conflictResult(p.store), nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, dbxpluginsdk.NewError(-32602, "记录不存在或已被删除")
	}
	if errors.Is(err, store.ErrBuiltin) {
		// 不说「不可删除」三个字就完事：必须给出下一步能做什么，
		// 否则用户会以为插件坏了。禁用是内置引擎唯一支持的「不用它」的方式。
		return nil, dbxpluginsdk.NewError(-32602,
			"「免费翻译服务」是内置引擎，不可删除。如果不想用它，可以在引擎列表里把它禁用。")
	}
	return nil, failed(-32002, err)
}

// autoDirection 在真正翻译前决定要不要「换向」。
//
// 解决的问题：目标是中文、用户却贴进来一段中文 —— 引擎（尤其传统机器翻译）
// 会把原文原样吐回，用户以为插件坏了。IDEA 插件用「含非拉丁字符就译成英文」处理，
// 这里收紧成「只在语系冲突时换向」，规则见 lang.AutoDirection。
//
// 换向后必须再过两道能力校验，否则会撞上下一步的语言支持检查：
//  1. 新目标语言要该引擎支持；
//  2. 源语言改成 auto 时，引擎要支持自动检测（不支持就保留用户原来的源语言，只换目标）。
//
// 任一不满足 → 放弃换向：宁可不换，也不能塞给引擎一个它不支持的语言组合。
func (p *plugin) autoDirection(e engine.Engine, sourceLang, targetLang, text string) (string, string, map[string]any) {
	prefs, _ := p.store.GetPrefs()
	if !boolArg(prefs, "autoDirectionEnabled", true) {
		return sourceLang, targetLang, nil
	}
	src, dst, detected, changed := lang.AutoDirection(sourceLang, targetLang, text)
	if !changed {
		return sourceLang, targetLang, nil
	}
	if !lang.Supports(e.Kind(), dst) {
		trace.Log("方向自适应取消：引擎 %s 不支持目标语言 %s", e.Kind(), dst)
		return sourceLang, targetLang, nil
	}
	if src == lang.Auto && !lang.SupportsAutoSource(e.Kind()) {
		src = sourceLang // 该引擎不支持自动检测源语言，维持用户原选择
	}
	if src != lang.Auto && !lang.Supports(e.Kind(), src) {
		trace.Log("方向自适应取消：引擎 %s 不支持源语言 %s", e.Kind(), src)
		return sourceLang, targetLang, nil
	}
	// 日志只记语言码，不记原文内容（本插件的披露口径之一）
	trace.Log("方向自适应: %s→%s 检测=%s ⇒ 改为 %s→%s",
		sourceLang, targetLang, detected, src, dst)
	return src, dst, map[string]any{
		"detected":   detected,
		"fromSource": sourceLang,
		"fromTarget": targetLang,
		"toSource":   src,
		"toTarget":   dst,
	}
}

// prepareSource 按用户偏好决定是否把「标识符形态」的原文拆成空格短语。
//
// 返回两个值：送给引擎的文本 + 给前端展示的信息（未处理时为 nil）。
// 关键约定：
//   - 界面上、历史里存的都还是【原文】，只有发往引擎的那一份被插入空格；
//   - 日志【不记录原文与译文内容】（本插件对审核方的披露口径之一），只记形态与长度；
//   - 默认开启：不拆的话机器翻译引擎多半把 linearGradient 原样吐回来，
//     用户看到的是「译文和原文一样」，比拆错更糟。
func (p *plugin) prepareSource(text string, prefs map[string]any) (string, map[string]any) {
	if !boolArg(prefs, "identifierSplitEnabled", true) {
		return text, nil
	}
	r := wordsplit.Prepare(text)
	if !r.Applied {
		return text, nil
	}
	trace.Log("标识符拆词 style=%s words=%d 原文=%d字符 送引擎=%d字符",
		r.Style, len(r.Words), len([]rune(r.Source)), len([]rune(r.Text)))
	// 界面【刻意不展示】这些内容：分词对用户完全无感知 —— 原文、译文、历史记录都保持原样。
	// 仍回传 text/words/style，只为诊断与排障时能看清「实际送给引擎的是什么」；
	// 不回传原文（前端本来就有，复制进事件只会白占负载）。
	return r.Text, map[string]any{
		"text":  r.Text,
		"words": r.Words,
		"style": r.Style,
	}
}

// effectiveChunkDelay 决定分段之间是否等待。
//
// 全局偏好 chunkDelayMs（默认 1100ms）的存在理由只有一个：百度/有道免费档是 1 QPS。
// 把它套到实测无限流的引擎上（内置免费服务：并发 100 全成功、30 秒内 206 次请求零失败），
// 后果是长文分段翻译白白多等 N×1.1 秒 —— 而该引擎单段上限只有 400 字符，
// 分段数天然比别人多，被拖累得也最重。
//
// 判据来自引擎自己声明的 Schema.NoThrottle，不在这里硬编码 kind 名：
// 以后再加无限流的引擎，改它的 schema 就够了（见 AGENTS.md「schema 单一事实来源在侧车」）。
func (p *plugin) effectiveChunkDelay(kind string, prefsDelay int) int {
	if _, sch, ok := engine.Lookup(kind); ok && sch.NoThrottle {
		return 0
	}
	return prefsDelay
}

// ---------------------------------------------------------------------------
// 引擎实例构造
// ---------------------------------------------------------------------------

// buildEngine 按引擎 ID（或临时配置）构造一个 Engine 实例。
//
// 每次翻译都新建实例而不是复用单例：这样用户改了配置立即生效，
// 不需要处理「旧实例还在用旧端点」的问题。构造成本极低（只填结构体，不发网络请求）。
//
// 密钥解密在这里发生 —— 这是明文密钥存在的第二个（也是最后一个）瞬间，
// 解密后直接交给适配器构造 HTTP 请求头，不写入任何日志。
func (p *plugin) buildEngine(engineID string, adhocKind string, adhocConfig map[string]any, adhocSecret string) (engine.Engine, store.Engine, error) {
	var (
		rec    store.Engine
		plain  string
		kind   string
		config map[string]any
	)

	if engineID != "" {
		// 用已保存的引擎
		got, err := p.store.GetEngine(engineID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, store.Engine{}, engine.New(engine.KindConfig, "引擎不存在或已被删除")
			}
			return nil, store.Engine{}, engine.Wrap(engine.KindNetwork, "读取引擎配置失败", err)
		}
		if !got.Enabled {
			return nil, store.Engine{}, engine.Newf(engine.KindConfig,
				"引擎「%s」已被禁用，请先在引擎管理中启用它", got.Name)
		}
		rec = got
		kind = got.Kind
		config = got.Config
		if got.HasSecret {
			sec, err := p.store.GetSecret(got.ID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, store.Engine{}, engine.New(engine.KindConfig,
						"引擎已标记为有密钥但密钥记录缺失，请重新保存 API Key")
				}
				return nil, store.Engine{}, engine.Wrap(engine.KindNetwork, "读取密钥失败", err)
			}
			decrypted, err := p.protector.Unprotect(sec.Cipher)
			if err != nil {
				// 解密失败最常见的原因：数据目录被复制到另一台机器，
				// 或系统密钥库不可用。必须明确告知用户重新输入，
				// 【绝不】静默清空配置 —— 那会让用户以为配置还在。
				return nil, store.Engine{}, engine.New(engine.KindConfig,
					"API Key 解密失败：系统密钥库不可用，或数据已迁移到其它机器。请在引擎管理中重新输入 API Key。")
			}
			plain = string(decrypted)
		}
	} else {
		// 用临时配置（engines/test 在保存前试连的场景）
		kind = adhocKind
		config = adhocConfig
		plain = adhocSecret
	}

	if kind == "" {
		return nil, store.Engine{}, engine.New(engine.KindConfig, "缺少引擎类型")
	}

	prefs, _ := p.store.GetPrefs()
	opts := engine.Options{
		Name:          rec.Name,
		TimeoutSecs:   engine.ConfigInt(engine.Config(config), "timeoutSecs", 0, 0, 600),
		SkipTLSVerify: engine.ConfigBool(engine.Config(config), "skipTlsVerify", false),
		Concurrency:   engine.ConfigInt(engine.Config(config), "concurrency", 1, 1, 4),
	}
	_ = prefs

	e, err := engine.Build(kind, engine.Config(config), secretsFor(kind, plain), opts)
	if err != nil {
		return nil, store.Engine{}, err
	}
	return e, rec, nil
}

// secretsFor 按引擎 schema 声明的 Secret 字段名，把明文密钥放进对应的键。
//
// 为什么不能硬编码几个键名：
//
//	各引擎的密钥字段名本来就不同 —— openai/deepl/google 是 apiKey，
//	baidu/tencent 是 secretKey，youdao 是 appSecret，aliyun 是 accessKeySecret。
//	之前这里硬编码了 `Secrets{"apiKey":…, "secretKey":…, "appSecret":…}` 三个键，
//	漏了 accessKeySecret，导致【阿里云引擎永远拿不到密钥】，
//	用户明明填了却报「AccessKey Secret 不能为空」。
//
//	这类 bug 的隐蔽之处在于：它只影响某一个引擎，其它七个都正常，
//	测试若没覆盖到那个引擎就完全发现不了。
//
// 现在改为从 schema 反查：schema 里标了 `Secret: true` 的字段就是该引擎的密钥键。
// 这样【新增引擎时只要 schema 写对，这里自动就对了】，不存在漏配的可能。
//
// 兜底：万一 schema 没声明 Secret 字段（例如 localcli 本就不需要密钥），
// 仍按常见键名各放一份，保证向后兼容。
func secretsFor(kind, plain string) engine.Secrets {
	out := engine.Secrets{}
	if plain == "" {
		return out
	}
	if _, sch, ok := engine.Lookup(kind); ok {
		for _, f := range sch.Fields {
			if f.Secret {
				out[f.Key] = plain
			}
		}
	}
	if len(out) == 0 {
		// schema 未声明 Secret 字段时的兜底：覆盖各引擎的常见命名
		for _, k := range []string{"apiKey", "secretKey", "appSecret", "accessKeySecret"} {
			out[k] = plain
		}
	}
	return out
}

// semaphore 返回指定引擎的并发信号量（懒创建）。
//
// 为什么按引擎而不是全局限流：不同服务商的配额是独立的，
// 用 DeepL 的配额去限制百度没有意义。按引擎限流才能让
// 「百度免费档 1 QPS」这类约束被正确遵守。
func (p *plugin) semaphore(kind string, size int) chan struct{} {
	if size < 1 {
		size = 1
	}
	if size > 4 {
		size = 4
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.semaphores == nil {
		p.semaphores = map[string]chan struct{}{}
	}
	// 已存在但容量不同：沿用旧的，避免正在排队的请求被丢弃。
	// 容量变更在侧车重启后生效，这是可接受的（并发数不是频繁调整的配置）。
	if sem, ok := p.semaphores[kind]; ok {
		return sem
	}
	sem := make(chan struct{}, size)
	p.semaphores[kind] = sem
	return sem
}

// acquire 获取信号量，尊重 ctx 取消。
func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release(sem chan struct{}) {
	select {
	case <-sem:
	default:
	}
}

// ---------------------------------------------------------------------------
// engines/test —— 连通性探测
// ---------------------------------------------------------------------------

// handleEnginesTest 用最短的文本试译一次，验证「端点可达 + Key 有效 + 模型存在」。
//
// 支持两种模式：
//   - 传 id：测试已保存的引擎
//   - 传 kind + config + secrets：测试尚未保存的配置（用户在表单里点「测试连接」）
//
// 第二种模式很重要：用户填完表单应该能先验证再保存，
// 否则「保存了一个连不上的引擎」会让人困惑。
func (p *plugin) handleEnginesTest(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	start := time.Now()

	id := strings.TrimSpace(strArg(values, "id", ""))
	adhocKind := strings.TrimSpace(strArg(values, "kind", ""))
	adhocConfig := map[string]any{}
	if raw, ok := values["config"].(map[string]any); ok {
		adhocConfig = raw
	}
	adhocSecret := ""
	if raw, ok := values["secrets"].(map[string]any); ok {
		adhocSecret = extractSecrets(raw)
	}
	if id == "" && adhocKind == "" {
		return nil, badParams("需要提供 id，或提供 kind + config")
	}

	e, _, err := p.buildEngine(id, adhocKind, adhocConfig, adhocSecret)
	if err != nil {
		ee := engine.Classify(err)
		return map[string]any{
			"ok":        false,
			"message":   ee.Message,
			"detail":    ee.Detail,
			"kind":      ee.KindName(),
			"code":      ee.Code(),
			"elapsedMs": engine.ElapsedMs(start),
		}, nil
	}

	// 探测用较短的超时：用户正在等着看结果，60 秒的等待不可接受
	ctx, cancel := context.WithTimeout(context.Background(), translateTestTimeoutSecs*time.Second)
	defer cancel()

	sem := p.semaphore(e.Kind(), 1)
	if err := acquire(ctx, sem); err != nil {
		return map[string]any{
			"ok":      false,
			"message": "等待并发槽位超时，请稍后重试",
			"kind":    "rate-limit",
			"code":    -32032,
		}, nil
	}
	defer release(sem)

	if err := e.Test(ctx); err != nil {
		ee := engine.Classify(err)
		trace.Log("engines/test 失败 kind=%s errKind=%s msg=%s", e.Kind(), ee.KindName(), ee.Message)
		return map[string]any{
			"ok":        false,
			"message":   ee.Message,
			"detail":    ee.Detail,
			"kind":      ee.KindName(),
			"code":      ee.Code(),
			"elapsedMs": engine.ElapsedMs(start),
		}, nil
	}

	elapsed := engine.ElapsedMs(start)
	trace.Log("engines/test ok kind=%s elapsed=%dms", e.Kind(), elapsed)
	return map[string]any{
		"ok":        true,
		"message":   fmt.Sprintf("连接成功（%s，耗时 %d 毫秒）", e.Kind(), elapsed),
		"elapsedMs": elapsed,
		"streaming": e.SupportsStreaming(),
		"maxChars":  e.MaxChars(),
	}, nil
}

// ---------------------------------------------------------------------------
// translate/run —— 非流式翻译
// ---------------------------------------------------------------------------

// handleTranslateRun 执行一次性翻译并返回完整结果。
//
// 这是流式不可用时的降级路径（prefs.streamEnabled=false，或宿主不转发事件），
// 也是「翻译并写入历史」的主路径。
func (p *plugin) handleTranslateRun(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	text := strArg(values, "text", "")
	if strings.TrimSpace(text) == "" {
		return nil, badParams("待翻译文本不能为空")
	}
	// 桥接层单次 JSON 上限 2 MiB，SDK 单条消息上限 8 MB。
	// 这里用 1.5 MB 做前置拦截，留出响应体的余量，
	// 否则请求能进来但响应发不出去，表现为「翻译卡住」。
	if len(text) > maxRequestTextBytes {
		return nil, badParams("文本过大（%d 字节），超过单次请求上限 %d 字节。请分段翻译。",
			len(text), maxRequestTextBytes)
	}

	sourceLang := strings.TrimSpace(strArg(values, "sourceLang", lang.Auto))
	targetLang := strings.TrimSpace(strArg(values, "targetLang", ""))
	if targetLang == "" {
		return nil, badParams("缺少目标语言 targetLang")
	}
	if !lang.IsValid(sourceLang) {
		return nil, badParams("未知的源语言码：%s", sourceLang)
	}
	if !lang.IsValid(targetLang) || targetLang == lang.Auto {
		return nil, badParams("目标语言非法：%s（不能为空或 auto）", targetLang)
	}

	engineID := strings.TrimSpace(strArg(values, "engineId", ""))
	if engineID == "" {
		def, err := p.store.DefaultEngine()
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, dbxpluginsdk.NewError(-32037,
					"还没有配置任何翻译引擎。请先在「引擎管理」里添加一个引擎。")
			}
			return nil, failed(-32002, err)
		}
		engineID = def.ID
	}

	e, rec, err := p.buildEngine(engineID, "", nil, "")
	if err != nil {
		return nil, engineErr(err)
	}

	// 方向自适应必须在语言支持校验【之前】做：校验要针对实际使用的语言对，
	// 否则换向后的组合没人检查，出错时给用户的提示也对不上。
	sourceLang, targetLang, dirInfo := p.autoDirection(e, sourceLang, targetLang, text)

	// 语言支持校验：提前拦截比让上游报「不支持的语言」更友好
	if !lang.Supports(e.Kind(), targetLang) {
		return nil, engineErr(engine.Newf(engine.KindUnsupported,
			"引擎「%s」不支持目标语言 %s", rec.Name, lang.ZhName(targetLang)))
	}
	if sourceLang != lang.Auto && !lang.Supports(e.Kind(), sourceLang) {
		return nil, engineErr(engine.Newf(engine.KindUnsupported,
			"引擎「%s」不支持源语言 %s", rec.Name, lang.ZhName(sourceLang)))
	}
	if sourceLang == lang.Auto && !lang.SupportsAutoSource(e.Kind()) {
		return nil, engineErr(engine.Newf(engine.KindUnsupported,
			"引擎「%s」不支持自动检测源语言，请显式指定源语言", rec.Name))
	}

	prefs, _ := p.store.GetPrefs()
	timeoutSecs := engine.ConfigInt(engine.Config(rec.Config), "timeoutSecs", defaultTranslateTimeoutSecs, 5, 600)
	chunkDelay := intArg(prefs, "chunkDelayMs", 1100, 0, 10000)
	// 内置免费服务这类上游无限流的引擎不沿用段间延时（见 Schema.NoThrottle）。
	chunkDelay = p.effectiveChunkDelay(e.Kind(), chunkDelay)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	// 送引擎的文本可能被拆过词；下面的 recordHistory 用的仍然是原文 text，
	// 保证「原文不被拆分」这条要求成立。
	sent, splitInfo := p.prepareSource(text, prefs)

	result, err := p.runChunked(ctx, e, rec, sent, sourceLang, targetLang, chunkDelay, nil)
	if err != nil {
		return nil, engineErr(err)
	}

	// 写入历史（失败不影响翻译结果返回：历史只是附加功能）
	if herr := p.recordHistory(rec, e.Kind(), sourceLang, targetLang, text, result); herr != nil {
		trace.Log("写入历史失败（不影响翻译结果）: %v", herr)
	}

	out := map[string]any{
		"ok":           true,
		"target":       result.Target,
		"elapsedMs":    result.ElapsedMs,
		"engine":       result.Engine,
		"engineKind":   e.Kind(),
		"model":        result.Model,
		"detectedLang": result.DetectedLang,
		"chunks":       result.Chunks,
	}
	if splitInfo != nil {
		// 只在真的拆过时才带这个字段，省得前端每次判空
		out["split"] = splitInfo
	}
	if dirInfo != nil {
		// 换向是【用户可见】的状态变化（目标语言会跟着变），
		// 所以必须回传，前端据此同步下拉框并解释原因
		out["direction"] = dirInfo
	}
	return out, nil
}

// maxRequestTextBytes 是单次请求文本的字节上限。
const maxRequestTextBytes = 1500 * 1000

// defaultTranslateTimeoutSecs 是整体翻译（含全部分段）的默认超时。
//
// 比单请求超时大：长文分成 10 段、每段 30 秒，整体需要 300 秒。
// 用单请求超时去限制整体，会让长文翻译必然失败。
const defaultTranslateTimeoutSecs = 300

// runChunked 执行分段翻译。
//
// 分段策略（见 chunk 包）：按「段落 → 句子 → 子句 → 硬切」逐级降级，
// 并保护代码块、URL、占位符不被切断。
//
// 分段【顺序执行】而非并发，原因有二：
//  1. 保序：并发返回的顺序不确定，拼接后译文会错乱
//  2. 限流：百度/有道免费档是 1 QPS，并发必然触发限流
//
// emit 非 nil 时逐段推送进度（流式场景）；为 nil 时只在内部累积。
func (p *plugin) runChunked(
	ctx context.Context,
	e engine.Engine,
	rec store.Engine,
	text, sourceLang, targetLang string,
	chunkDelayMs int,
	emit func(engine.Chunk) error,
) (engine.Result, error) {
	maxChars := e.MaxChars()
	if maxChars <= 0 {
		maxChars = 20000
	}

	split, err := chunkSplit(text, maxChars)
	if err != nil {
		return engine.Result{}, err
	}

	sem := p.semaphore(e.Kind(), engine.ConfigInt(engine.Config(rec.Config), "concurrency", 1, 1, 4))
	start := time.Now()

	var (
		builder strings.Builder
		chunks  int
		emitted int
		// detectedLang / model 需要从【分段结果】里保留下来。
		//
		// 之前重建 Result 时只填了 Target/ElapsedMs/Chunks/Engine，
		// 把适配器返回的 DetectedLang 与 Model 丢掉了 —— 症状是
		// 前端 translate/run 响应里 detectedLang 与 model 永远是空串，
		// 「自动检测到的源语言」这个信息完全展示不出来。
		//
		// 取第一个非空值：源语言检测对整篇文本是一致的，
		// 而分段场景下只有第一段（或前几段）会带回检测结果。
		detectedLang string
		model        string
	)
	for i, part := range split {
		// 每段之前都检查取消：长文分段翻译可能持续几分钟，
		// 用户点取消后必须尽快停止，不能等所有段跑完。
		if err := ctx.Err(); err != nil {
			return engine.Result{}, engine.Wrap(engine.KindCancelled, "翻译已取消", err)
		}

		if err := acquire(ctx, sem); err != nil {
			return engine.Result{}, engine.Wrap(engine.KindCancelled, "翻译已取消", err)
		}

		req := engine.Request{
			Source:     part,
			SourceLang: sourceLang,
			TargetLang: targetLang,
			MaxChars:   maxChars,
		}

		var partResult engine.Result
		if emit != nil && e.SupportsStreaming() {
			// 流式：把上游的增量直接转发给前端，同时内部累积完整译文
			partResult, err = e.Stream(ctx, req, func(c engine.Chunk) error {
				c.Index = emitted
				c.Total = len(split)
				emitted++
				builder.WriteString(c.Text)
				return emit(c)
			})
		} else {
			// 非流式（或引擎不支持流式）：一次性拿到结果
			partResult, err = e.Translate(ctx, req)
			if err == nil && emit != nil {
				// 降级为单 chunk 推送：前端代码路径与真流式完全一致，
				// 不需要为「不支持流式的引擎」写分支。
				if emitErr := emit(engine.Chunk{Text: partResult.Target, Index: emitted, Total: len(split)}); emitErr != nil {
					release(sem)
					return engine.Result{}, engine.Wrap(engine.KindCancelled, "翻译已取消", emitErr)
				}
				emitted++
				builder.WriteString(partResult.Target)
			} else if err == nil {
				builder.WriteString(partResult.Target)
			}
		}
		release(sem)

		if err != nil {
			return engine.Result{}, err
		}
		chunks++
		// 保留适配器返回的元信息（只取第一个非空值）
		if detectedLang == "" && partResult.DetectedLang != "" {
			detectedLang = partResult.DetectedLang
		}
		if model == "" && partResult.Model != "" {
			model = partResult.Model
		}

		// 段间延时：尊重免费档 1 QPS 限制。
		// 只在还有后续段时才等，最后一段不用白等。
		if chunkDelayMs > 0 && i < len(split)-1 {
			select {
			case <-ctx.Done():
				return engine.Result{}, engine.Wrap(engine.KindCancelled, "翻译已取消", ctx.Err())
			case <-time.After(time.Duration(chunkDelayMs) * time.Millisecond):
			}
		}
	}

	target := builder.String()
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol, "翻译结果为空")
	}
	name := rec.Name
	if name == "" {
		name = e.Kind()
	}
	return engine.Result{
		Target:       target,
		DetectedLang: detectedLang,
		Model:        model,
		ElapsedMs:    engine.ElapsedMs(start),
		Chunks:       chunks,
		Engine:       name,
	}, nil
}

// chunkSplit 调用 chunk 包分段，并把错误归类为引擎错误。
//
// 分段结果里若发生过硬切（HardCut），译文质量会下降，
// 这里记进 trace 供排查；不阻断翻译，因为「质量略降的结果」
// 仍然远好于「直接失败」。
func chunkSplit(text string, maxChars int) ([]string, error) {
	res, err := chunk.Split(text, maxChars)
	if err != nil {
		if errors.Is(err, chunk.ErrTooLong) {
			return nil, engine.New(engine.KindConfig,
				"文本包含无法切分的超长片段（例如一个极长的 URL 或代码块），请缩短后重试")
		}
		return nil, engine.Wrap(engine.KindConfig, "文本分段失败", err)
	}
	if len(res.Parts) == 0 {
		return nil, engine.New(engine.KindConfig, "文本分段后为空")
	}
	if res.HardCut {
		trace.Log("chunkSplit: 发生硬切（maxChars=%d 段数=%d），译文质量可能下降", maxChars, len(res.Parts))
	}
	return res.Parts, nil
}

// recordHistory 把一次翻译写入历史。
func (p *plugin) recordHistory(rec store.Engine, kind, sourceLang, targetLang, source string, res engine.Result) error {
	prefs, _ := p.store.GetPrefs()
	limit := intArg(prefs, "historyLimit", 500, 100, 5000)
	return p.store.AddHistory(store.HistoryItem{
		ID:         newID("h"),
		EngineID:   rec.ID,
		EngineName: res.Engine,
		EngineKind: kind,
		SrcLang:    sourceLang,
		DstLang:    targetLang,
		Source:     source,
		Target:     res.Target,
		Chars:      len([]rune(source)),
		ElapsedMs:  res.ElapsedMs,
		Chunks:     res.Chunks,
	}, limit)
}

// ---------------------------------------------------------------------------
// translate/start 与 translate/cancel —— 流式翻译任务表
// ---------------------------------------------------------------------------

// handleTranslateStart 启动一个流式翻译任务，立即返回 taskId。
//
// 为什么不直接在 RPC 里同步返回译文：
// 宿主对单次 invoke 有超时上限（开发宿主夹到 120 秒），而长文分段翻译
// 可能持续几分钟。异步任务 + 事件推送才能突破这个限制，
// 并且让用户看到逐字出现的效果。
//
// 任务生命周期（对应 PLAN.md 5.4）：
//  1. 生成单调递增 taskId，注册到 tasks 表，创建可取消的 ctx
//  2. 立即返回 {ok, taskId}
//  3. goroutine 异步执行：分段 → 逐段翻译 → 每个增量发 translate/chunk
//  4. 完成发 translate/end，失败发 translate/error
//  5. 【三条路径都必须从 tasks 表删除】，否则 goroutine 与 ctx 泄漏
func (p *plugin) handleTranslateStart(values map[string]any, emitter *dbxpluginsdk.Emitter) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	text := strArg(values, "text", "")
	if strings.TrimSpace(text) == "" {
		return nil, badParams("待翻译文本不能为空")
	}
	if len(text) > maxRequestTextBytes {
		return nil, badParams("文本过大（%d 字节），超过单次请求上限。请分段翻译。", len(text))
	}

	sourceLang := strings.TrimSpace(strArg(values, "sourceLang", lang.Auto))
	targetLang := strings.TrimSpace(strArg(values, "targetLang", ""))
	if !lang.IsValid(sourceLang) {
		return nil, badParams("未知的源语言码：%s", sourceLang)
	}
	if !lang.IsValid(targetLang) || targetLang == lang.Auto {
		return nil, badParams("目标语言非法：%s", targetLang)
	}

	engineID := strings.TrimSpace(strArg(values, "engineId", ""))
	if engineID == "" {
		def, err := p.store.DefaultEngine()
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, dbxpluginsdk.NewError(-32037,
					"还没有配置任何翻译引擎。请先在「引擎管理」里添加一个引擎。")
			}
			return nil, failed(-32002, err)
		}
		engineID = def.ID
	}

	// 在飞任务上限检查：超出说明有任务卡住了或用户在疯狂触发，
	// 直接拒绝比无限堆积 goroutine 更安全。
	if p.taskCount() >= maxInflightTasks {
		return map[string]any{
			"ok":      false,
			"code":    -32032,
			"message": fmt.Sprintf("同时进行的翻译任务已达上限（%d 个），请等待当前任务完成或取消后再试", maxInflightTasks),
		}, nil
	}

	// emitter 是流式输出的必要条件：没有它就无法推送 chunk 事件。
	//
	// 这个检查放在参数校验与在飞上限之后、构造引擎之前：
	//   - 放在参数校验之后 → 参数错误能给出更精确的提示
	//   - 放在在飞上限之后 → 限流是资源保护，应优先于能力检查拒绝
	//   - 放在构造引擎之前 → 避免为注定失败的请求白白解密一次密钥
	if emitter == nil {
		return nil, dbxpluginsdk.NewError(-32603, "当前宿主不支持事件推送，无法使用流式翻译")
	}

	// 提前构造引擎：配置错误（Key 解不开、引擎被禁用）应该【同步】报错，
	// 而不是返回 taskId 后再通过事件报错 —— 那样前端已经切换到"翻译中"状态，
	// 用户体验是"点了翻译，转了一圈，什么都没发生"。
	e, rec, err := p.buildEngine(engineID, "", nil, "")
	if err != nil {
		return nil, engineErr(err)
	}

	// 方向自适应必须在语言支持校验【之前】做：校验要针对实际使用的语言对，
	// 否则换向后的组合没人检查，出错时给用户的提示也对不上。
	sourceLang, targetLang, dirInfo := p.autoDirection(e, sourceLang, targetLang, text)
	if !lang.Supports(e.Kind(), targetLang) {
		return nil, engineErr(engine.Newf(engine.KindUnsupported,
			"引擎「%s」不支持目标语言 %s", rec.Name, lang.ZhName(targetLang)))
	}
	if sourceLang == lang.Auto && !lang.SupportsAutoSource(e.Kind()) {
		return nil, engineErr(engine.Newf(engine.KindUnsupported,
			"引擎「%s」不支持自动检测源语言，请显式指定源语言", rec.Name))
	}

	prefs, _ := p.store.GetPrefs()
	timeoutSecs := engine.ConfigInt(engine.Config(rec.Config), "timeoutSecs", defaultTranslateTimeoutSecs, 5, 600)
	chunkDelay := intArg(prefs, "chunkDelayMs", 1100, 0, 10000)
	// 内置免费服务这类上游无限流的引擎不沿用段间延时（见 Schema.NoThrottle）。
	chunkDelay = p.effectiveChunkDelay(e.Kind(), chunkDelay)
	// 用户可关闭流式（例如宿主事件转发有问题时的降级开关）
	wantStream := boolArg(values, "stream", true) && boolArg(prefs, "streamEnabled", true)

	taskID := p.newTaskID()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)

	p.mu.Lock()
	if p.tasks == nil {
		p.tasks = map[string]*task{}
	}
	p.tasks[taskID] = &task{id: taskID, cancel: cancel, engineID: engineID, started: time.Now()}
	p.mu.Unlock()

	trace.Log("translate/start taskId=%s engine=%s kind=%s chars=%d stream=%v",
		taskID, rec.Name, e.Kind(), len([]rune(text)), wantStream)

	// 送引擎的文本可能被拆过词；recordHistory 仍写原文（见 runTask 参数命名）
	sent, splitInfo := p.prepareSource(text, prefs)

	go p.runTask(ctx, taskID, e, rec, text, sent, sourceLang, targetLang, chunkDelay, wantStream, splitInfo, dirInfo, emitter)

	return map[string]any{"ok": true, "taskId": taskID}, nil
}

// runTask 是流式翻译任务的执行体，跑在独立 goroutine 里。
//
// 【必须】用 defer 保证任务从 tasks 表删除 —— 完成、失败、取消、panic
// 四条路径都要清理，否则 tasks 表会无限增长，最终触发 maxInflightTasks 拒绝服务。
func (p *plugin) runTask(
	ctx context.Context,
	taskID string,
	e engine.Engine,
	rec store.Engine,
	// sourceText 是用户输入的原文（写历史用它）；
	// sentText 是实际送引擎的文本（标识符预处理后可能不同）。
	// 两个都传进来，是为了让「原文不被拆分」这件事在签名上就是显式的，
	// 不依赖调用方记得用哪个变量。
	sourceText, sentText string,
	sourceLang, targetLang string,
	chunkDelayMs int,
	wantStream bool,
	splitInfo map[string]any,
	dirInfo map[string]any,
	emitter *dbxpluginsdk.Emitter,
) {
	defer func() {
		p.removeTask(taskID)
		if r := recover(); r != nil {
			trace.Log("translate 任务 panic taskId=%s: %v", taskID, r)
			p.emitError(emitter, taskID, -32030, fmt.Sprintf("翻译过程发生内部错误：%v", r))
		}
	}()

	emit := func(c engine.Chunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 事件发送失败通常是宿主已关闭前端（用户关了工作台）。
		// 此时应停止翻译，避免白白消耗上游配额。
		if perr := emitter.Event("translate/chunk", map[string]any{
			"taskId": taskID,
			"text":   c.Text,
			"index":  c.Index,
			"total":  c.Total,
		}); perr != nil {
			trace.Log("translate/chunk 事件发送失败 taskId=%s: %s", taskID, perr.Message)
			return errors.New(perr.Message)
		}
		return nil
	}

	var (
		result engine.Result
		err    error
	)
	if wantStream {
		result, err = p.runChunked(ctx, e, rec, sentText, sourceLang, targetLang, chunkDelayMs, emit)
	} else {
		// 非流式：一次性翻译，然后把完整结果作为单个 chunk 推送。
		// 前端代码路径与真流式一致，不需要分支。
		result, err = p.runChunked(ctx, e, rec, sentText, sourceLang, targetLang, chunkDelayMs, nil)
		if err == nil {
			_ = emit(engine.Chunk{Text: result.Target, Index: 0, Total: 1})
		}
	}

	if err != nil {
		ee := engine.Classify(err)
		trace.Log("translate 任务失败 taskId=%s kind=%s msg=%s", taskID, ee.KindName(), ee.Message)
		p.emitError(emitter, taskID, ee.Code(), ee.Message)
		return
	}

	// 历史存【原文】：拆词只是送给引擎前的一次性变换，不能污染用户的记录
	if herr := p.recordHistory(rec, e.Kind(), sourceLang, targetLang, sourceText, result); herr != nil {
		trace.Log("写入历史失败 taskId=%s: %v", taskID, herr)
	}

	trace.Log("translate 任务完成 taskId=%s elapsed=%dms chunks=%d chars=%d",
		taskID, result.ElapsedMs, result.Chunks, len([]rune(result.Target)))

	endData := map[string]any{
		"taskId":       taskID,
		"target":       result.Target,
		"elapsedMs":    result.ElapsedMs,
		"engine":       result.Engine,
		"engineKind":   e.Kind(),
		"model":        result.Model,
		"detectedLang": result.DetectedLang,
		"chunks":       result.Chunks,
	}
	if splitInfo != nil {
		// 拆词信息只用于诊断：界面对拆词无感知（见 prepareSource 注释）
		endData["split"] = splitInfo
	}
	if dirInfo != nil {
		// 换向会改变目标语言，前端要同步下拉选择并解释原因，故必须回传
		endData["direction"] = dirInfo
	}
	if perr := emitter.Event("translate/end", endData); perr != nil {
		trace.Log("translate/end 事件发送失败 taskId=%s: %s", taskID, perr.Message)
	}
}

// emitError 发送 translate/error 事件。
func (p *plugin) emitError(emitter *dbxpluginsdk.Emitter, taskID string, code int, message string) {
	if emitter == nil {
		return
	}
	if perr := emitter.Event("translate/error", map[string]any{
		"taskId":  taskID,
		"code":    code,
		"message": trace.Redact(message),
	}); perr != nil {
		trace.Log("translate/error 事件发送失败 taskId=%s: %s", taskID, perr.Message)
	}
}

// newTaskID 生成单调递增的任务 ID。
//
// 用 atomic 而非随机数：前端要靠 taskId 做竞态过滤
// （丢弃不属于当前任务的事件），单调递增让「新任务 ID 一定大于旧任务」，
// 调试时也能一眼看出任务的先后顺序。
func (p *plugin) newTaskID() string {
	n := atomic.AddUint64(&taskSeq, 1)
	return fmt.Sprintf("t%d-%d", time.Now().UnixMilli(), n)
}

// taskSeq 是任务序号计数器（进程内单调递增）。
var taskSeq uint64

// removeTask 从任务表删除并释放 ctx。
//
// 必须调用 cancel()：context.WithTimeout 创建的 ctx 会启动一个定时器，
// 不调用 cancel 会让定时器一直挂着直到超时，这是标准的 goroutine 泄漏源。
func (p *plugin) removeTask(id string) {
	p.mu.Lock()
	t, ok := p.tasks[id]
	if ok {
		delete(p.tasks, id)
	}
	p.mu.Unlock()
	if ok && t.cancel != nil {
		t.cancel()
	}
}

// handleTranslateCancel 取消一个在飞任务。
//
// 取消不存在的任务返回 ok=true 而非错误：这是幂等设计。
// 前端的「输入即译」会在每次新输入前取消上一个任务，
// 而上一个任务可能已经自然完成 —— 这种情况不是错误，不该弹提示。
func (p *plugin) handleTranslateCancel(values map[string]any) (any, *RPCError) {
	taskID := strings.TrimSpace(strArg(values, "taskId", ""))
	if taskID == "" {
		return nil, badParams("缺少 taskId")
	}
	p.mu.Lock()
	t, ok := p.tasks[taskID]
	p.mu.Unlock()

	if !ok {
		trace.Log("translate/cancel: 任务不存在或已结束 taskId=%s", taskID)
		return map[string]any{"ok": true, "taskId": taskID, "alreadyDone": true}, nil
	}
	if t.cancel != nil {
		t.cancel()
	}
	trace.Log("translate/cancel ok taskId=%s engine=%s 已运行=%s",
		taskID, t.engineID, time.Since(t.started))
	// 不立即从 tasks 表删除：让 runTask 的 defer 去做，
	// 保证「发 translate/error 事件」与「删除任务」的顺序确定。
	return map[string]any{"ok": true, "taskId": taskID}, nil
}

// ---------------------------------------------------------------------------
// 历史
// ---------------------------------------------------------------------------

func (p *plugin) handleHistoryList(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	q := store.HistoryQuery{
		Limit:  intArg(values, "limit", 50, 1, 500),
		Offset: intArg(values, "offset", 0, 0, 1000000),
		Q:      strings.TrimSpace(strArg(values, "q", "")),
	}
	// starred 是三态：不传=nil（不限），传 true/false 精确筛选
	if v, ok := values["starred"]; ok && v != nil {
		if b, ok := v.(bool); ok {
			q.Starred = &b
		}
	}
	items, total, err := p.store.ListHistory(q)
	if err != nil {
		return nil, failed(-32002, err)
	}
	return map[string]any{"items": items, "total": total}, nil
}

func (p *plugin) handleHistoryStar(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(strArg(values, "id", ""))
	if id == "" {
		return nil, badParams("缺少历史记录 id")
	}
	starred := boolArg(values, "starred", true)
	if err := p.store.SetHistoryStarred(id, starred); err != nil {
		return p.mapStoreErr(err)
	}
	return map[string]any{"ok": true, "starred": starred}, nil
}

func (p *plugin) handleHistoryDelete(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	raw, ok := values["ids"].([]any)
	if !ok {
		return nil, badParams("缺少 ids 数组")
	}
	ids := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			ids = append(ids, strings.TrimSpace(s))
		}
	}
	n, err := p.store.DeleteHistory(ids)
	if err != nil {
		return nil, failed(-32002, err)
	}
	return map[string]any{"ok": true, "deleted": n}, nil
}

func (p *plugin) handleHistoryClear(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	keepStarred := boolArg(values, "keepStarred", true)
	n, err := p.store.ClearHistory(keepStarred)
	if err != nil {
		return nil, failed(-32002, err)
	}
	trace.Log("history/clear ok deleted=%d keepStarred=%v", n, keepStarred)
	return map[string]any{"ok": true, "deleted": n}, nil
}

// ---------------------------------------------------------------------------
// 偏好
// ---------------------------------------------------------------------------

func (p *plugin) handlePrefsGet() (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	prefs, err := p.store.GetPrefs()
	if err != nil {
		return nil, failed(-32012, err)
	}
	rev, _ := p.store.Rev()
	return map[string]any{"prefs": prefs, "rev": rev}, nil
}

func (p *plugin) handlePrefsSet(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	incoming, ok := values["prefs"].(map[string]any)
	if !ok {
		return nil, badParams("缺少 prefs 对象")
	}
	prefs, newRev, err := p.store.SetPrefs(incoming, revArg(values))
	if err != nil {
		return p.mapStoreErr(err)
	}
	return map[string]any{"ok": true, "prefs": prefs, "rev": newRev}, nil
}

// ---------------------------------------------------------------------------
// 数据导出 / 导入
// ---------------------------------------------------------------------------

// exportPayload 是导出文件的结构。
//
// 【安全】Secrets 默认不填充。只有用户显式勾选「包含 API Key」时才写入，
// 且导出文件顶部会有一段醒目的明文警告（见 warning 字段）。
type exportPayload struct {
	Format     string              `json:"format"`
	Version    int                 `json:"version"`
	Plugin     string              `json:"plugin"`
	ExportedAt string              `json:"exportedAt"`
	Warning    string              `json:"warning,omitempty"`
	Engines    []store.Engine      `json:"engines"`
	Secrets    map[string]string   `json:"secrets,omitempty"`
	History    []store.HistoryItem `json:"history"`
	Prefs      map[string]any      `json:"prefs"`
	Counts     map[string]int      `json:"counts"`
}

func (p *plugin) handleDataExport(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	includeSecrets := boolArg(values, "includeSecrets", false)

	engines, err := p.store.ListEngines()
	if err != nil {
		return nil, failed(-32004, err)
	}
	prefs, err := p.store.GetPrefs()
	if err != nil {
		return nil, failed(-32004, err)
	}
	// 导出全部历史（不分页）。上限受 maxRequestTextBytes 约束，
	// 超限时截断并在 counts 里标明。
	history, _, err := p.store.ListHistory(store.HistoryQuery{Limit: 500, Offset: 0})
	if err != nil {
		return nil, failed(-32004, err)
	}

	payload := exportPayload{
		Format:     "dbx-translation-export",
		Version:    1,
		Plugin:     pluginID,
		ExportedAt: time.Now().Format(time.RFC3339),
		Engines:    engines,
		History:    history,
		Prefs:      prefs,
		Counts: map[string]int{
			"engines": len(engines),
			"history": len(history),
			"prefs":   len(prefs),
		},
	}

	if includeSecrets {
		// 解密后以【明文】写入导出文件。这是用户显式要求的迁移场景，
		// 但必须在文件里留下无法忽略的警告。
		payload.Warning = "警告：本文件包含 API Key 明文。请通过安全渠道传输，导入后立即删除本文件。"
		secrets := map[string]string{}
		for _, e := range engines {
			if !e.HasSecret {
				continue
			}
			rec, err := p.store.GetSecret(e.ID)
			if err != nil {
				continue
			}
			plain, err := p.protector.Unprotect(rec.Cipher)
			if err != nil {
				trace.Log("data/export: 引擎 %s 的密钥解密失败，已跳过", e.ID)
				continue
			}
			secrets[e.ID] = string(plain)
		}
		if len(secrets) > 0 {
			payload.Secrets = secrets
			payload.Counts["secrets"] = len(secrets)
		}
		trace.Log("data/export: 用户选择包含密钥，共 %d 条", len(payload.Secrets))
	}

	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, failed(-32004, err)
	}
	if len(b) > maxRequestTextBytes {
		return nil, dbxpluginsdk.NewError(-32004,
			fmt.Sprintf("导出数据过大（%d 字节），超过桥接上限。请先清理部分历史后重试。", len(b)))
	}

	meta := resolveMetadata()
	fileName := fmt.Sprintf("dbx-translation-%s-%s.json", meta.Version, time.Now().Format("20060102-150405"))
	return map[string]any{
		"ok":         true,
		"fileName":   fileName,
		"dataBase64": base64.StdEncoding.EncodeToString(b),
		"bytes":      len(b),
		"counts":     payload.Counts,
		"hasSecrets": payload.Secrets != nil,
	}, nil
}

func (p *plugin) handleDataImport(values map[string]any) (any, *RPCError) {
	if err := p.requireStore(); err != nil {
		return nil, err
	}
	b64 := strings.TrimSpace(strArg(values, "dataBase64", ""))
	if b64 == "" {
		return nil, badParams("缺少 dataBase64")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, badParams("数据不是合法的 base64：%v", err)
	}
	if len(raw) > maxRequestTextBytes {
		return nil, badParams("导入数据过大（%d 字节）", len(raw))
	}

	var payload exportPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, badParams("数据不是合法的 JSON：%v", err)
	}
	if payload.Format != "dbx-translation-export" {
		return nil, badParams("文件格式不匹配：期望 dbx-translation-export，得到 %q", payload.Format)
	}
	if payload.Version != 1 {
		return nil, badParams("不支持的文件版本：%d（当前支持 1）", payload.Version)
	}

	// mode: merge（默认，保留现有数据）| replace（先清空再导入）
	mode := strings.TrimSpace(strArg(values, "mode", "merge"))
	if mode != "merge" && mode != "replace" {
		return nil, badParams("mode 只能是 merge 或 replace，得到 %q", mode)
	}

	counts := map[string]int{}
	var newRev int

	if mode == "replace" {
		// 整体替换：在单个事务里 DELETE 再 INSERT，避免半份快照
		if _, err := p.store.ClearHistory(false); err != nil {
			return nil, failed(-32006, err)
		}
		existing, _ := p.store.ListEngines()
		for _, e := range existing {
			if e.Builtin {
				// 内置引擎不参与「清空」：它不属于用户数据，而是插件自带能力。
				// 放过它还有一个实际好处 —— 否则每次 replace 导入都会撞上一次
				// ErrBuiltin，往日志里写一条看着像故障的「删除旧引擎失败」。
				continue
			}
			if _, err := p.store.DeleteEngine(e.ID, -1); err != nil {
				trace.Log("data/import: 删除旧引擎 %s 失败: %v", e.ID, err)
			}
		}
	}

	for _, e := range payload.Engines {
		if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.Kind) == "" {
			continue
		}
		// 校验引擎类型仍被支持：导入旧备份时可能含已下线的引擎
		if _, _, ok := engine.Lookup(e.Kind); !ok {
			trace.Log("data/import: 跳过未知引擎类型 %s (id=%s)", e.Kind, e.ID)
			counts["skippedEngines"]++
			continue
		}
		if _, err := p.store.SaveEngine(e, -1); err != nil {
			trace.Log("data/import: 保存引擎 %s 失败: %v", e.ID, err)
			continue
		}
		counts["engines"]++

		// 导入密钥：文件里是明文，必须重新用当前机器的保护器加密
		if plain, ok := payload.Secrets[e.ID]; ok && strings.TrimSpace(plain) != "" {
			cipher, err := p.protector.Protect([]byte(plain))
			if err != nil {
				trace.Log("data/import: 引擎 %s 密钥加密失败", e.ID)
				continue
			}
			fp := secret.Fingerprint(plain)
			if err := p.store.SaveSecret(e.ID, cipher, p.protector.Scheme(), fp); err != nil {
				trace.Log("data/import: 引擎 %s 密钥保存失败: %v", e.ID, err)
				continue
			}
			counts["secrets"]++
		}
	}

	// 导入完再兜一次底：旧备份文件里可能根本没有内置引擎这条记录
	//（它是在这个功能之前导出的），必须保证导入后依然开箱可用。
	if p.builtinTpl.ID != "" {
		if _, err := p.store.EnsureBuiltin(p.builtinTpl); err != nil {
			trace.Log("data/import: 内置引擎兜底失败: %v", err)
		}
	}

	for _, h := range payload.History {
		if strings.TrimSpace(h.ID) == "" {
			h.ID = newID("h")
		}
		if h.CreatedAt == "" {
			h.CreatedAt = time.Now().Format(time.RFC3339)
		}
		// limit=0 表示导入时不触发淘汰：用户导入的备份应该完整保留，
		// 淘汰会在下一次正常翻译写入时按 prefs.historyLimit 生效。
		if err := p.store.AddHistory(h, 0); err != nil {
			trace.Log("data/import: 保存历史 %s 失败: %v", h.ID, err)
			continue
		}
		counts["history"]++
	}

	if len(payload.Prefs) > 0 {
		if _, rev, err := p.store.SetPrefs(payload.Prefs, -1); err == nil {
			newRev = rev
			counts["prefs"] = len(payload.Prefs)
		}
	}
	if newRev == 0 {
		newRev, _ = p.store.Rev()
	}

	trace.Log("data/import ok mode=%s counts=%v", mode, counts)
	return map[string]any{"ok": true, "counts": counts, "rev": newRev, "mode": mode}, nil
}

// ---------------------------------------------------------------------------
// 通用辅助
// ---------------------------------------------------------------------------

// idSeq 是 ID 生成的进程内计数器，配合时间戳保证唯一。
var idSeq uint64

// newID 生成带前缀的唯一 ID。
//
// 用「时间戳 + 进程内计数器」而非 UUID：ID 会出现在数据库主键与前端 DOM 里，
// 短一点更易读易调试；同一进程内计数器保证不碰撞，跨进程靠时间戳区分。
func newID(prefix string) string {
	n := atomic.AddUint64(&idSeq, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()/1000, n)
}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

// main 是侧车入口。
//
// 启动顺序有讲究：
//  1. 先建数据目录（trace 要往里写日志）
//  2. 初始化 trace（后续所有步骤都要记日志）
//  3. 还原上次配置（loadConfig）
//  4. 打开数据库（失败不退出：让前端能通过 ping 看到 dbOk=false 并给出提示，
//     而不是「侧车直接消失，用户只看到一片空白」）
//  5. 初始化密钥保护器
//  6. 启动 RPC 服务
func main() {
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// 数据目录都建不了，侧车无法工作。但仍不能写 stdout（会污染协议），
		// 只能写 stderr 然后退出，让宿主报告「后端进程退出」。
		fmt.Fprintf(os.Stderr, "[translation] 无法创建数据目录 %s: %v\n", dir, err)
		os.Exit(1)
	}
	trace.Init(dir)
	loadConfig()

	cwd, _ := os.Getwd()
	trace.Log("=== 侧车启动 pid=%d version=%s dataDir=%s cwd=%s ===",
		os.Getpid(), pluginVersion, dir, cwd)

	st, stErr := store.Open(dir)
	if stErr != nil {
		// 不退出：让前端通过 translate/ping 的 dbOk=false 得知，
		// 并在 UI 上显示可操作的错误（例如「数据目录无写权限」）。
		// 直接退出会让用户看到「插件无响应」，完全不知道该查什么。
		trace.Log("打开数据库失败（侧车仍会启动，前端将进入降级模式）: %v", stErr)
	}

	prot := secret.Default(pluginID)
	trace.Log("密钥保护方案: %s (secure=%v)", prot.Scheme(), prot.Secure())

	p := &plugin{
		store:      st,
		protector:  prot,
		dataDir:    dir,
		tasks:      map[string]*task{},
		semaphores: map[string]chan struct{}{},
	}
	writeConfig(map[string]any{"dataDir": dir, "updatedAt": time.Now().Format(time.RFC3339)})

	// 内置「免费翻译服务」：装好插件就存在、开箱可用，用户不可编辑删除，
	// 只能启用/禁用与设默认。这样新用户第一次打开工作台就能翻译，
	// 不必先跑一遍「申请 Key → 填端点 → 测试连通」。
	//
	// 为什么放在启动流程而不是 store.Open 里：
	// store 是纯持久层，不该知道任何引擎 kind；而 main 已经把引擎包导进来了。
	// 同理，单测里不会被这条记录干扰 —— 只有真实启动才会播种。
	p.builtinTpl = store.Engine{
		ID:      store.BuiltinEngineID,
		Name:    free.NameZh,
		Kind:    free.Kind,
		Config:  free.SeedConfig(),
		Enabled: true,
		Builtin: true,
	}
	if st != nil {
		created, err := st.EnsureBuiltin(p.builtinTpl)
		if err != nil {
			// 播种失败不影响其它引擎：记日志继续，用户仍可手动配置官方引擎。
			trace.Log("内置引擎初始化失败（其它功能不受影响）: %v", err)
		} else if created {
			trace.Log("已初始化内置免费引擎 id=%s model=%s", p.builtinTpl.ID, free.Model)
		}
	}

	meta := resolveMetadata()
	trace.Log("上报身份: id=%s version=%s capabilities=%v", meta.ID, meta.Version, meta.Capabilities)

	server := dbxpluginsdk.NewServer(meta, p)
	if err := server.Serve(); err != nil {
		trace.Log("RPC 服务退出: %v", err)
		fmt.Fprintf(os.Stderr, "[translation] %v\n", err)
		if st != nil {
			_ = st.Close()
		}
		os.Exit(1)
	}
	if st != nil {
		_ = st.Close()
	}
	trace.Log("=== 侧车正常退出 ===")
}
