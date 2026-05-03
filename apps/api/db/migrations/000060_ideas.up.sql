-- Ideas channel: global product feedback forum.
-- Authenticated users post ideas, vote (toggle, no self-vote), and discuss in
-- threaded comments. Status is dev-only and changes via direct SQL — the
-- AFTER UPDATE trigger ensures the idea author still gets notified.

CREATE SEQUENCE idea_number_seq START 1;

CREATE TABLE ideas (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    number          BIGINT NOT NULL UNIQUE DEFAULT nextval('idea_number_seq'),
    title           VARCHAR(140) NOT NULL,
    description     TEXT NOT NULL,
    category        VARCHAR(40) NOT NULL CHECK (category IN (
        'eventos_lives','checkout','carrinho','pagamentos','frete_logistica',
        'produtos','pedidos','clientes','integracoes_erp','integracoes_social',
        'notificacoes','dashboard_relatorios','time_permissoes','onboarding',
        'api_webhooks','outros'
    )),
    status          VARCHAR(30) NOT NULL DEFAULT 'aberta' CHECK (status IN (
        'aberta','em_estudo','em_desenvolvimento','concluida','recusada'
    )),
    author_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    vote_count      INTEGER NOT NULL DEFAULT 0,
    comment_count   INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ideas_status     ON ideas(status);
CREATE INDEX idx_ideas_category   ON ideas(category);
CREATE INDEX idx_ideas_author     ON ideas(author_user_id);
CREATE INDEX idx_ideas_vote_count ON ideas(vote_count DESC);
CREATE INDEX idx_ideas_created_at ON ideas(created_at DESC);
CREATE INDEX idx_ideas_search ON ideas
    USING GIN (to_tsvector('portuguese', title || ' ' || description));

COMMENT ON TABLE ideas IS
    'Global ideas channel posts. Status changes are dev-only via SQL; trigger notifies author.';

CREATE TABLE idea_votes (
    idea_id     UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (idea_id, user_id)
);
CREATE INDEX idx_idea_votes_user ON idea_votes(user_id);

CREATE TABLE idea_comments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idea_id           UUID NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
    parent_comment_id UUID REFERENCES idea_comments(id) ON DELETE CASCADE,
    author_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    body              TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_idea_comments_idea_created ON idea_comments(idea_id, created_at);
CREATE INDEX idx_idea_comments_parent       ON idea_comments(parent_comment_id);

-- In-app notifications. Generic shape, but v1 only emits the three idea-related
-- types. The existing internal/notification module is for outbound DM/Email to
-- buyers; this table is the dashboard inbox for authenticated users.
CREATE TABLE notifications (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_id        UUID REFERENCES users(id) ON DELETE SET NULL,
    type            VARCHAR(40) NOT NULL CHECK (type IN (
        'idea_comment','idea_reply','idea_status_change'
    )),
    idea_id         UUID REFERENCES ideas(id) ON DELETE CASCADE,
    comment_id      UUID REFERENCES idea_comments(id) ON DELETE CASCADE,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    read_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_notifications_recipient_unread
    ON notifications(recipient_id, created_at DESC) WHERE read_at IS NULL;
CREATE INDEX idx_notifications_recipient_all
    ON notifications(recipient_id, created_at DESC);

COMMENT ON TABLE notifications IS
    'In-app dashboard notifications. Distinct from notification_logs (outbound to buyers).';

-- Trigger so devs changing status via psql still notify the author.
CREATE OR REPLACE FUNCTION notify_idea_status_change() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        INSERT INTO notifications (recipient_id, actor_id, type, idea_id, payload)
        VALUES (
            NEW.author_user_id,
            NULL,
            'idea_status_change',
            NEW.id,
            jsonb_build_object('old_status', OLD.status, 'new_status', NEW.status)
        );
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_idea_status_change
    AFTER UPDATE OF status ON ideas
    FOR EACH ROW EXECUTE FUNCTION notify_idea_status_change();

-- Denormalized counters maintained by triggers so the Go service stays simple
-- (a plain INSERT/DELETE is enough; no manual UPDATE of counters needed).
CREATE OR REPLACE FUNCTION bump_idea_vote_count() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE ideas SET vote_count = vote_count + 1 WHERE id = NEW.idea_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE ideas SET vote_count = vote_count - 1 WHERE id = OLD.idea_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_idea_votes_count
    AFTER INSERT OR DELETE ON idea_votes
    FOR EACH ROW EXECUTE FUNCTION bump_idea_vote_count();

CREATE OR REPLACE FUNCTION bump_idea_comment_count() RETURNS TRIGGER AS $$
BEGIN
    UPDATE ideas SET comment_count = comment_count + 1 WHERE id = NEW.idea_id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_idea_comments_count
    AFTER INSERT ON idea_comments
    FOR EACH ROW EXECUTE FUNCTION bump_idea_comment_count();
