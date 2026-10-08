-- 编号用 239y：紧跟 239x（upstream_faster_model），按文件名排序执行，不与上游编号冲突。
-- 降智观察补充两列（只观察，不参与转发与计费）：
--   upstream_turn_state_len   上游 x-codex-turn-state 响应头的长度（09-23 前 292/332=未降级档，之后恒为 780）；
--                             NULL 表示该次响应没有这个头。
--   upstream_safety_buffering 上游 X-Codex-Safety-Buffering-Enabled 响应头（true/false）；NULL 表示没有该头。
-- 可空、无默认值，只改表定义，不重写 usage_logs；不建索引（按 created_at 范围筛选即可）。
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_turn_state_len INTEGER;

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_safety_buffering BOOLEAN;

COMMENT ON COLUMN usage_logs.upstream_turn_state_len IS
    'Length of the upstream x-codex-turn-state response header; NULL when absent. Observation only.';

COMMENT ON COLUMN usage_logs.upstream_safety_buffering IS
    'Value of the upstream X-Codex-Safety-Buffering-Enabled response header; NULL when absent. Observation only.';
