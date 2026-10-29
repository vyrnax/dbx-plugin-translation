// Package store 是侧车的持久化层（SQLite）。
//
// # 为什么用 SQLite 而不是 JSON 文件
//
// 翻译历史会持续增长（默认保留 500 条，可配到 5000 条），JSON 文件方案
// 每次写入都要序列化整个文档，历史一多就会明显变慢；而且「整体读-改-写」
// 在多实例并发下极易丢数据。SQLite 提供事务、索引与增量写入，
// 分页查询历史也不需要把全部数据读进内存。
//
// # 为什么必须用 modernc.org/sqlite
//
// mattn/go-sqlite3 需要 cgo，而 cgo 会【彻底破坏交叉编译】——
// 官方 `dbx-plugin package` 拒绝为非当前宿主平台打包，我们靠纯 Go
// 才能在 Windows 上直接产出 darwin/linux 的合法包。
// modernc.org/sqlite 是 SQLite 的纯 Go 转译实现，无 cgo。
//
// # 并发模型
//
// SetMaxOpenConns(1)：SQLite 是单写者数据库，多连接并发写会频繁触发
// SQLITE_BUSY。限制为单连接让写操作在 Go 侧串行化，比依赖 busy_timeout
// 重试更可靠。读操作也会被串行化，但本插件的读都很轻量（分页查询），
// 不构成瓶颈。
//
// busy_timeout(5000) 仍然保留：它防的是【跨进程】竞争 ——
// 用户可能同时开了多个 DBX 窗口，每个窗口都有自己的侧车进程，
// 它们可能指向同一个数据目录。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrConflict 表示 rev 乐观锁冲突：前端手里的 rev 落后于磁盘。
//
// 调用方应映射为错误码 -32020，并回传最新 rev 让前端刷新后重试。
// 【绝不】直接覆盖 —— 那会静默丢掉另一个会话的修改。
var ErrConflict = errors.New("数据已被其它会话修改")

// ErrNotFound 表示请求的记录不存在。
var ErrNotFound = errors.New("记录不存在")

// schemaVersion 是数据库结构版本，存在 meta 表里供未来迁移分支使用。
const schemaVersion = "1"

// Store 是持久化层句柄。所有方法都是并发安全的。
type Store struct {
	mu   sync.Mutex // 串行化写事务，配合 SetMaxOpenConns(1)
	db   *sql.DB
	path string
}

// Engine 是翻译引擎的配置记录。
//
// 注意：这里【没有】API Key 明文字段。密钥单独存在 secrets 表里，
// 且只存密文。Engine 只带 HasSecret 与 Fingerprint 两个元信息，
// 供 UI 显示「已配置」状态。
//
// 这是密钥安全的第一道结构保证：即使有人把 Engine 结构体整个序列化
// 到日志或 RPC 返回值里，也不可能泄露密钥，因为字段根本不存在。
type Engine struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Config      map[string]any `json:"config"`
	HasSecret   bool           `json:"hasSecret"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	IsDefault   bool           `json:"isDefault"`
	Enabled     bool           `json:"enabled"`
	Ord         int            `json:"ord"`
	// Builtin 为 true 表示这是【内置引擎】：名字/类型/配置由代码拥有，
	// 不可删除、不可改写，只允许改 Enabled / IsDefault / Ord。
	// 保护逻辑在 SaveEngine / DeleteEngine 内部，不在 RPC 层 ——
	// 只在某个 handler 里判断，迟早有一条写入路径漏掉。
	Builtin   bool   `json:"builtin"`
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// SecretRecord 是密钥密文记录。
type SecretRecord struct {
	EngineID  string `json:"engineId"`
	Cipher    string `json:"cipher"` // 密文或密钥库引用令牌
	Scheme    string `json:"scheme"` // dpapi | keychain | secret-tool | aesgcm-machine
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// HistoryItem 是一条翻译历史。
type HistoryItem struct {
	ID         string `json:"id"`
	EngineID   string `json:"engineId,omitempty"`
	EngineName string `json:"engineName,omitempty"`
	EngineKind string `json:"engineKind,omitempty"`
	SrcLang    string `json:"srcLang,omitempty"`
	DstLang    string `json:"dstLang,omitempty"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Chars      int    `json:"chars"`
	ElapsedMs  int64  `json:"elapsedMs"`
	Chunks     int    `json:"chunks"`
	Starred    bool   `json:"starred"`
	CreatedAt  string `json:"createdAt"`
}

// Open 打开（必要时创建）数据库并完成迁移。
//
// dir 是插件数据目录。数据库文件为 <dir>/translation.db。
//
// 迁移失败会重试一次（间隔 150ms）：SQLite 在数据目录刚被创建、
// 或多个进程同时首次打开时，偶发 SQLITE_BUSY 是正常现象，
// 重试一次能消掉绝大多数瞬态失败。错误消息带语句序号与库路径，
// 便于用户远程反馈时定位。
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("数据目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	path := filepath.Join(dir, "translation.db")

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		s, err := openOnce(path)
		if err == nil {
			return s, nil
		}
		lastErr = err
		if attempt == 0 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("打开数据库失败（库路径：%s）: %w", path, lastErr)
}

func openOnce(path string) (*Store, error) {
	// busy_timeout 让 SQLite 在遇到锁时等待而不是立刻返回 SQLITE_BUSY。
	// 5 秒足够覆盖正常的写事务耗时，又不会让用户感觉「卡死」。
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open 失败: %w", err)
	}
	// 单连接：SQLite 单写者，多连接并发写会频繁 BUSY
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate 建表建索引，全部幂等。
//
// 用 CREATE TABLE IF NOT EXISTS + 忽略错误的 ALTER TABLE ADD COLUMN，
// 这样重复执行安全，未来加列也不需要写版本分支。
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS engines (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			kind        TEXT NOT NULL,
			config_json TEXT NOT NULL,
			has_secret  INTEGER NOT NULL DEFAULT 0,
			fingerprint TEXT,
			is_default  INTEGER NOT NULL DEFAULT 0,
			enabled     INTEGER NOT NULL DEFAULT 1,
			ord         INTEGER NOT NULL DEFAULT 0,
			builtin     INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT,
			updated_at  TEXT
		)`,
		// secrets 与 engines 分开存：
		// 这样「列出引擎」的查询根本不会碰到密文列，
		// 从结构上杜绝了「不小心把密钥查出来返回给前端」。
		`CREATE TABLE IF NOT EXISTS secrets (
			engine_id  TEXT PRIMARY KEY,
			cipher     TEXT NOT NULL,
			scheme     TEXT NOT NULL,
			created_at TEXT,
			updated_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS history (
			id          TEXT PRIMARY KEY,
			engine_id   TEXT,
			engine_name TEXT,
			engine_kind TEXT,
			src_lang    TEXT,
			dst_lang    TEXT,
			source      TEXT NOT NULL,
			target      TEXT NOT NULL,
			chars       INTEGER NOT NULL DEFAULT 0,
			elapsed_ms  INTEGER NOT NULL DEFAULT 0,
			chunks      INTEGER NOT NULL DEFAULT 0,
			starred     INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS prefs (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		// 历史按时间倒序分页是主查询路径，必须有索引，
		// 否则 5000 条历史时每次翻页都是全表扫描。
		`CREATE INDEX IF NOT EXISTS idx_history_created ON history(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_history_starred ON history(starred)`,
		`CREATE INDEX IF NOT EXISTS idx_engines_ord ON engines(ord)`,
	}
	for i, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate[%d/%d]: %w", i+1, len(stmts), err)
		}
	}
	// 幂等加列：老库升级时 builtin 列还不存在，ALTER 会报 duplicate column，
	// 忽略即可；新库由上面的 CREATE TABLE 直接带上（见 AGENTS.md 6.6）。
	if _, err := s.db.Exec(`ALTER TABLE engines ADD COLUMN builtin INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("加 builtin 列失败: %w", err)
	}

	// 初始化 meta：rev 从 0 开始，schema_version 供未来迁移分支
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO meta(key, value) VALUES('rev','0'),('schema_version',?)`,
		schemaVersion,
	); err != nil {
		return fmt.Errorf("初始化 meta 失败: %w", err)
	}
	return nil
}

// Close 关闭数据库。侧车退出前应调用（虽然进程退出时 OS 也会回收）。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Path 返回数据库文件路径，供诊断信息展示。
func (s *Store) Path() string { return s.path }

// ---------------------------------------------------------------------------
// rev 乐观锁
// ---------------------------------------------------------------------------

// Rev 读取当前修订号。
func (s *Store) Rev() (int, error) {
	var rev int
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key='rev'`).Scan(&rev)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取 rev 失败: %w", err)
	}
	return rev, nil
}

// bumpRev 在事务内递增 rev 并返回新值。
//
// 必须在事务里调用 —— rev 的读取与递增如果被拆开，
// 两个并发写会拿到同一个 rev，乐观锁就失效了。
func bumpRev(tx *sql.Tx) (int, error) {
	var cur int
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key='rev'`).Scan(&cur); err != nil {
		return 0, fmt.Errorf("读取 rev 失败: %w", err)
	}
	next := cur + 1
	if _, err := tx.Exec(`UPDATE meta SET value=? WHERE key='rev'`, next); err != nil {
		return 0, fmt.Errorf("递增 rev 失败: %w", err)
	}
	return next, nil
}

// CheckRev 校验前端回声的 rev 是否与磁盘一致。
//
// expectRev < 0 表示「跳过校验」（用于导入这类用户主动的整体覆盖操作）。
// 不一致时返回 ErrConflict 与当前 rev，调用方据此回传 newRev 让前端重放。
func (s *Store) CheckRev(tx *sql.Tx, expectRev int) (int, error) {
	cur, err := revInTx(tx)
	if err != nil {
		return 0, err
	}
	if expectRev >= 0 && expectRev != cur {
		return cur, ErrConflict
	}
	return cur, nil
}

func revInTx(tx *sql.Tx) (int, error) {
	var rev int
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key='rev'`).Scan(&rev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取 rev 失败: %w", err)
	}
	return rev, nil
}

// WithTx 在单个事务里执行 fn，自动提交/回滚。
//
// 所有写操作都必须走这里：SQLite 的事务保证了「整体替换」不会出现半份快照
// （例如删了旧引擎但新引擎没插进去）。
func (s *Store) WithTx(fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errors.New("数据库已关闭")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// nowISO 返回当前时间的 RFC3339 字符串（本地时区）。
//
// 用本地时区而非 UTC：历史记录的创建时间是给用户看的，
// 显示本地时间才符合直觉。排序仍正确，因为 RFC3339 带时区偏移。
func nowISO() string {
	return time.Now().Format(time.RFC3339)
}

// marshalConfig 把配置 map 序列化为 JSON 字符串。
func marshalConfig(cfg map[string]any) (string, error) {
	if cfg == nil {
		cfg = map[string]any{}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("序列化配置失败: %w", err)
	}
	return string(b), nil
}

// unmarshalConfig 反序列化配置 JSON。空串或非法 JSON 都返回空 map 而非错误 ——
// 配置损坏不应该让整个引擎列表加载失败，用户至少还能删掉重建。
func unmarshalConfig(s string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
