-- Track per-comment delivery state so the resend flow picks a usable comment.
-- Instagram allows exactly ONE private reply per comment, and rejects replies
-- to hidden/deleted comments — without this state the resend can target a
-- comment that can no longer receive a reply (error 2534066).
ALTER TABLE live_comments ADD COLUMN IF NOT EXISTS private_reply_used BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE live_comments ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false;
