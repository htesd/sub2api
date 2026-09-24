-- 编号用 239x：上游 v0.2.8 已占用 239/240，按文件名区分不会冲突，合并上游后其迁移仍会补执行。
-- 上游降级标记（只观察）：记录上游 X-Codex-Safety-Buffering-Faster-Model 响应头的值。
-- 可空、无默认值，只改表定义，不重写 usage_logs。刻意不建索引：并发建索引要扫全表，
-- 会拖慢启动；按时间范围筛选已有 created_at 索引可用。
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_faster_model VARCHAR(200);

COMMENT ON COLUMN usage_logs.upstream_faster_model IS
    'Value of the upstream X-Codex-Safety-Buffering-Faster-Model response header (the faster model the upstream reports switching to); NULL when absent. Observation only.';
