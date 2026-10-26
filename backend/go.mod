// DBX 翻译助手插件 —— 侧车（Go，纯标准库 + vendor 的官方 SDK + modernc.org/sqlite）
//
// 依赖约束（不可违反，见 AGENTS.md 第 2.2 节）：
//   - 必须【无 cgo】。官方 `dbx-plugin package` 拒绝为非当前宿主平台打包，
//     纯 Go 无 cgo 才能在 Windows 上直接交叉编译出 darwin/linux 的合法包。
//     因此 SQLite 用 modernc.org/sqlite（纯 Go），【禁止】mattn/go-sqlite3（需 cgo）。
//   - golang.org/x/sys 仅用于 Windows DPAPI（CryptProtectData/CryptUnprotectData），
//     它是纯 syscall 实现，不引入 cgo，不破坏交叉编译。
//   - HTTP 一律用标准库 net/http，SSE 手写解析，不引第三方 HTTP 客户端。
//
// 数据存储：
//   - 引擎配置 / 密钥密文 / 翻译历史 / 界面偏好统一落盘在
//     <DBX_PLUGIN_DATA_DIR>/translation.db（SQLite）。
//   - 前端跑在 opaque origin 沙箱里没有任何磁盘权限，localStorage 一碰就抛
//     SecurityError；唯一落盘通道是 dbxPlugin.invoke(...) → 本进程写磁盘。
//   - 并发防护用 meta 表里单调递增的 rev：前端回声 rev，不匹配则拒绝并回传最新
//     rev 让前端重放，避免多实例「旧快照覆盖新数据」。
//
// 密钥安全：
//   - API Key 明文只在「加密前」与「构造 HTTP 请求头时」两个瞬间存在，
//     绝不写入日志、事件、错误消息或任何 RPC 返回值。
//   - 加密优先用系统密钥库（Windows DPAPI / macOS Keychain / Linux secret-tool），
//     不可用时降级为机器绑定 AES-256-GCM，并通过 translate/ping 的 secretSecure
//     字段告知前端，由前端常驻警示条明确披露「仅混淆保护」。

module github.com/vyrnax/translation

go 1.21

require (
	golang.org/x/sys v0.22.0
	modernc.org/sqlite v1.34.4
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)
