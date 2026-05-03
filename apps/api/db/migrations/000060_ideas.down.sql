DROP TRIGGER IF EXISTS trg_idea_comments_count ON idea_comments;
DROP TRIGGER IF EXISTS trg_idea_votes_count ON idea_votes;
DROP TRIGGER IF EXISTS trg_idea_status_change ON ideas;
DROP FUNCTION IF EXISTS bump_idea_comment_count();
DROP FUNCTION IF EXISTS bump_idea_vote_count();
DROP FUNCTION IF EXISTS notify_idea_status_change();

DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS idea_comments;
DROP TABLE IF EXISTS idea_votes;
DROP TABLE IF EXISTS ideas;
DROP SEQUENCE IF EXISTS idea_number_seq;
