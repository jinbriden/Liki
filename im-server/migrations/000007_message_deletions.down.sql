DROP TABLE IF EXISTS message_deletions;
ALTER TABLE conversation_members DROP COLUMN IF EXISTS cleared_seq;
