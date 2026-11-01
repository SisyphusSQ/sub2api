-- 百炼字符计费量与 Token 分开保存；历史记录不推算字符数。
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS audio_input_characters INTEGER NOT NULL DEFAULT 0;
