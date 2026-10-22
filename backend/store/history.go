package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// AddHistory 追加一条翻译历史，并按 historyLimit 淘汰超额的最旧记录。
//
// 【不参与 rev 乐观锁】：历史是追加型数据，两个会话各自翻译各自的，
// 不存在「旧快照覆盖新数据」的问题。如果给它加 rev 校验，
// 用户开着两个窗口翻译时会不停收到冲突提示，这是纯粹的干扰。
func (s *Store) AddHistory(item HistoryItem, historyLimit int) error {
	if strings.TrimSpace(item.ID) == "" {
		return errors.New("历史记录 ID 不能为空")
	}
	if item.CreatedAt == "" {
		item.CreatedAt = nowISO()
	}
	return s.WithTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO history (id, engine_id, engine_name, engine_kind,
			                     src_lang, dst_lang, source, target,
			                     chars, elapsed_ms, chunks, starred, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			item.ID, nullStr(item.EngineID), nullStr(item.EngineName), nullStr(item.EngineKind),
			nullStr(item.SrcLang), nullStr(item.DstLang), item.Source, item.Target,
			item.Chars, item.ElapsedMs, item.Chunks, boolToInt(item.Starred), item.CreatedAt); err != nil {
			return fmt.Errorf("写入历史失败: %w", err)
		}
		return pruneHistory(tx, historyLimit)
	})
}

// pruneHistory 淘汰超出上限的最旧记录，但【保留收藏】。
//
// 保留收藏是关键：用户特意标星的内容通常是有长期价值的（例如常用术语的译法），
// 如果自动清理把收藏也删了，收藏功能就失去了意义。
//
// 上限 <= 0 时不淘汰（用户显式关闭了限制）。
func pruneHistory(tx *sql.Tx, limit int) error {
	if limit <= 0 {
		return nil
	}
	var total int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&total); err != nil {
		return fmt.Errorf("统计历史数量失败: %w", err)
	}
	if total <= limit {
		return nil
	}
	// 删除「非收藏」中最旧的 (total-limit) 条。
	// 用子查询定位待删 ID，避免 SQLite 的 DELETE ... LIMIT 兼容性问题。
	excess := total - limit
	if _, err := tx.Exec(`
		DELETE FROM history WHERE id IN (
			SELECT id FROM history
			WHERE starred=0
			ORDER BY created_at ASC
			LIMIT ?
		)`, excess); err != nil {
		return fmt.Errorf("淘汰旧历史失败: %w", err)
	}
	return nil
}

// HistoryQuery 是历史查询条件。
type HistoryQuery struct {
	Limit   int    // 每页条数，<=0 时用默认 50
	Offset  int    // 偏移量
	Q       string // 模糊搜索（源文或译文）
	Starred *bool  // nil=不限，true=只看收藏，false=只看未收藏
}

// ListHistory 分页查询历史，按创建时间倒序。
//
// 返回 items 与 total（满足条件的总条数，供前端算总页数）。
// total 单独查一次是必要的：分页需要知道总量才能显示「第 x/y 页」。
func (s *Store) ListHistory(q HistoryQuery) ([]HistoryItem, int, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500 // 防止前端传入离谱的 limit 拖垮查询
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	where := []string{"1=1"}
	args := []any{}
	if strings.TrimSpace(q.Q) != "" {
		// LIKE 模糊搜索源文与译文。
		// 注意：这里不做全文索引，5000 条历史的 LIKE 扫描在本机 SQLite 上
		// 仍是毫秒级，不值得引入 FTS5 的复杂度。
		//
		// 【必须带 ESCAPE 子句】：escapeLike 把 % 和 _ 转义成 \% 和 \_，
		// 但 SQLite 默认不认反斜杠为转义符 —— 不声明 ESCAPE '\' 的话，
		// 用户搜 "100%" 会匹配不到字面含百分号的记录（\% 被当成「反斜杠 + 通配符」）。
		where = append(where, `(source LIKE ? ESCAPE '\' OR target LIKE ? ESCAPE '\')`)
		like := "%" + escapeLike(q.Q) + "%"
		args = append(args, like, like)
	}
	if q.Starred != nil {
		where = append(where, "starred=?")
		args = append(args, boolToInt(*q.Starred))
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计历史总数失败: %w", err)
	}

	rows, err := s.db.Query(`
		SELECT id, engine_id, engine_name, engine_kind, src_lang, dst_lang,
		       source, target, chars, elapsed_ms, chunks, starred, created_at
		FROM history WHERE `+whereSQL+`
		ORDER BY created_at DESC, rowid DESC
		LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询历史失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]HistoryItem, 0, q.Limit)
	for rows.Next() {
		var (
			it                               HistoryItem
			engineID, engineName, engineKind sql.NullString
			srcLang, dstLang                 sql.NullString
			starred                          int
		)
		if err := rows.Scan(&it.ID, &engineID, &engineName, &engineKind, &srcLang, &dstLang,
			&it.Source, &it.Target, &it.Chars, &it.ElapsedMs, &it.Chunks, &starred, &it.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("读取历史记录失败: %w", err)
		}
		it.EngineID = engineID.String
		it.EngineName = engineName.String
		it.EngineKind = engineKind.String
		it.SrcLang = srcLang.String
		it.DstLang = dstLang.String
		it.Starred = starred != 0
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历历史失败: %w", err)
	}
	return out, total, nil
}

// escapeLike 转义 LIKE 通配符。
//
// 不转义的话，用户搜索 "100%" 会把 % 当成通配符，搜出所有含 "100" 的记录，
// 这与用户意图不符。SQLite 需要配合 ESCAPE 子句使用。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SetHistoryStarred 设置/取消收藏。
func (s *Store) SetHistoryStarred(id string, starred bool) error {
	res, err := s.db.Exec(`UPDATE history SET starred=? WHERE id=?`, boolToInt(starred), id)
	if err != nil {
		return fmt.Errorf("更新收藏状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteHistory 按 ID 批量删除，返回实际删除条数。
func (s *Store) DeleteHistory(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// 过滤空 ID，避免生成 `IN ('')` 这类无意义条件
	clean := make([]any, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return 0, nil
	}
	res, err := s.db.Exec(`DELETE FROM history WHERE id IN (`+placeholders(len(clean))+`)`, clean...)
	if err != nil {
		return 0, fmt.Errorf("删除历史失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ClearHistory 清空历史。keepStarred=true 时保留收藏项。
func (s *Store) ClearHistory(keepStarred bool) (int, error) {
	q := `DELETE FROM history`
	if keepStarred {
		q += ` WHERE starred=0`
	}
	res, err := s.db.Exec(q)
	if err != nil {
		return 0, fmt.Errorf("清空历史失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountHistory 返回历史总数，供诊断与导出统计。
func (s *Store) CountHistory() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计历史数量失败: %w", err)
	}
	return n, nil
}

// nullStr 把空串转成 NULL 存库。
//
// 为什么不用空串：历史列表里 engine_id 为空表示「引擎已被删除」，
// 用 NULL 能与「引擎 ID 恰好是空串」区分开（后者是数据损坏）。
// 前端也能据此决定是否显示引擎名。
func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
