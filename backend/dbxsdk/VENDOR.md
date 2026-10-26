# Vendored: DBX Plugin Go SDK

本目录 vendor 了**官方** DBX Plugin SDK for Go，来源是 `@dbx-app/plugin-cli` npm 包内的：

```
sdk-root/plugins/sdk/go/dbx-plugin-sdk/sdk.go
```

本机实际路径（plugin-cli 0.1.9）：

```
C:\Users\Lenovo\AppData\Roaming\npm\node_modules\@dbx-app\plugin-cli\sdk-root\plugins\sdk\go\dbx-plugin-sdk\sdk.go
```

`sdk.go` 是上游文件的**字节级复制**（SHA-256 已核对一致）。**禁止修改**——任何改动都可能
悄悄偏离宿主的协议预期，而这类偏离的表现是「宿主静默丢弃侧车」，极难排查。

## 为什么 vendor 而不是 import 模块

1. `github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk` **未发布到 Go module proxy**，
   `go get` 会失败并报 `unknown revision v0.1.0`。
2. 上游模块声明 `go 1.22`。作为独立模块引入时，低于 1.22 的工具链会拒绝构建
   （Go 1.20 没有自动工具链切换）。
3. 作为本模块内的普通 package vendor 进来后，侧车只需 Go 1.21+，且能**完全离线构建**。
   官方 `dbx-plugin-files` 对 Rust SDK 也是同样处理方式。

本插件内的 import 路径：`github.com/vyrnax/translation/dbxsdk`

## 传输方式

`dbxpluginsdk.NewServer(metadata, handler)` 从 stdin 按行读取 JSON-RPC 2.0 请求
（JSON Lines），并以同样方式写出响应。这正好匹配 DBX manifest v1 —— **manifest v1
已不再接受 `entrypoints.backend.transport` 字段**，写了会被判为无效清单。

> **framed 传输（5 字节头 + payload）是 Rust SDK 的能力。Go 侧车必须留在 JSON Lines。**

## 为什么用官方 SDK 而不是手写 JSON-RPC 循环

SDK 白送三件手写循环极易搞错的事：

- `plugin/initialize` 由 SDK 自己应答，返回恰好
  `{protocolVersion, capabilities, plugin:{id,version}}`；宿主通告的 `protocolVersions`
  不含 1 时返回错误 `-32001`。
- 每个请求一个 goroutine，因此并发请求的 ID 关联是正确的。
- 8 MB 行缓冲；notice（没有 `id` 的请求）不产生响应。

**这三件事搞错任何一件，都足以让宿主丢弃侧车**，而在 UI 上的表现只是「数据存不上」，
没有任何报错。

## 升级方式

升级 `@dbx-app/plugin-cli` 后，重新 diff 一次上游 `sdk.go` 与本文件：

```powershell
fc.exe /b "<npm-global>\node_modules\@dbx-app\plugin-cli\sdk-root\plugins\sdk\go\dbx-plugin-sdk\sdk.go" backend\dbxsdk\sdk.go
```

有差异时整体覆盖复制，并更新本文件记录的来源版本号。**不要手工合并。**
