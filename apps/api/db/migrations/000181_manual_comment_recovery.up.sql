ALTER TABLE live_comment_work
    ADD COLUMN manual_recovery jsonb,
    ADD COLUMN recovery_result text,
    ADD COLUMN recovery_reason text;

COMMENT ON COLUMN live_comment_work.manual_recovery IS
    'Immutable merchant authorization for a no_product comment, including original result, actor and pinned purchase';
