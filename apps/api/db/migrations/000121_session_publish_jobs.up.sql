-- 000121 — Publicacao agendada no Instagram (RN-31 / N3). FATIA DESTACAVEL:
-- nada mais no epico depende desta tabela.
--
-- Numeracao: o plano reservava a 000120 para esta tabela, mas a 000116 foi
-- consumida pelo motivo de nao entrega (RN-38) e tudo deslocou em um. O
-- CONTRACT ficou na 000120; o agendamento vem aqui.
--
-- POR QUE O AGENDADOR E NOSSO: a API de Content Publishing do Instagram nao
-- tem agendamento nativo (nao existe scheduled_publish_time) e o container de
-- midia EXPIRA EM 24H. Entao o container so pode ser criado poucos minutos
-- antes da hora — o que esta tabela guarda ate la e a INTENCAO (o asset, a
-- legenda, os produtos, a janela), nao um container ja aberto.
--
-- POR QUE TABELA E NAO COLUNAS EM live_sessions: o job tem ciclo de vida
-- proprio (tentativas, cancelamento, dead-letter) e existe ANTES da sessao.
-- Pendurar estado de execucao no nivel errado e exatamente o erro que este
-- epico esta desfazendo (D17).

CREATE TABLE session_publish_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id        UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,

    -- DIVERGENCIA DELIBERADA DO PLANO: ele previa session_id NOT NULL. Nao
    -- sobrevive ao codigo. A sessao nasce em CreatePostEvent, que exige o
    -- media_id — e o media_id so existe DEPOIS que o Instagram publicou. Um
    -- agendamento para D+3 nao tem sessao nenhuma a que se referir no dia em
    -- que e criado. As duas colunas sao o RESULTADO do job, preenchidas no
    -- disparo; ON DELETE SET NULL porque apagar o evento nao pode apagar o
    -- rastro de que a publicacao aconteceu.
    session_id      UUID REFERENCES live_sessions(id) ON DELETE SET NULL,
    event_id        UUID REFERENCES live_events(id) ON DELETE SET NULL,

    -- Espelha live_sessions.type: o que vai ser publicado define a chamada da
    -- Graph (PublishImagePost / PublishReel / PublishStory).
    media_kind      VARCHAR NOT NULL CHECK (media_kind IN ('post', 'reel', 'story')),

    -- O ASSET RETIDO. Hoje o arquivo morre no storage logo apos o publish
    -- sincrono (integration/service.go, deleteTransientImage); para publicar
    -- em D+3 ele precisa sobreviver ate la. Guardamos a CHAVE, nunca a URL
    -- assinada: a presigned GET dura horas e o agendamento dura dias, entao
    -- ela e re-assinada no disparo. Uma URL guardada aqui publicaria um link
    -- morto e o Graph responderia "erro de fetch" sem motivo obvio.
    asset_path      TEXT NOT NULL,
    -- Necessario no disparo: PublishStory precisa saber se e video ou foto, e
    -- essa informacao chega no upload, nao na hora de publicar.
    asset_content_type TEXT NOT NULL,

    -- Snapshot da intencao: o job publica o que foi agendado, nao o que a loja
    -- passou a ter depois. product_ids e array porque a whitelist do evento e
    -- ordenada por posicao e nao ha a que dar FK antes de o evento existir.
    caption         TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL DEFAULT '',
    product_ids     UUID[] NOT NULL CHECK (cardinality(product_ids) > 0),
    starts_at       TIMESTAMPTZ,
    ends_at         TIMESTAMPTZ,
    cart_expiration_minutes    INT,
    cart_max_quantity_per_item INT,

    scheduled_for   TIMESTAMPTZ NOT NULL,
    status          VARCHAR NOT NULL DEFAULT 'scheduled'
                    CHECK (status IN ('scheduled','publishing','published','failed','cancelled')),

    -- SEM container_id, embora o plano o previsse: o provider cria o
    -- container, espera ele ficar FINISHED e publica dentro de UMA chamada
    -- (PublishImagePost/PublishReel/PublishStory), entao nunca existe um
    -- instante em que a gente segure um container id para gravar. Coluna que
    -- ninguem escreve e a armadilha que este epico esta desfazendo — publish_at
    -- existe desde a 000112 e nunca teve escritor.
    published_media_id TEXT,

    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT,
    last_attempt_at TIMESTAMPTZ,
    published_at    TIMESTAMPTZ,
    cancelled_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- O sweep de backstop varre por aqui: task asynq perdida (Redis limpo, deploy
-- no meio) deixa o job 'scheduled' com a hora vencida e ninguem o dispara.
-- Mesmo desenho do SweepEndedTimedEvents, que existe pelo mesmo motivo.
CREATE INDEX idx_session_publish_jobs_due
    ON session_publish_jobs (scheduled_for) WHERE status = 'scheduled';

-- Job preso em 'publishing' (processo morto entre a reivindicacao e o
-- desfecho) precisa ser recuperavel: sem este indice, a varredura de presos
-- seria um seq scan na tabela inteira a cada 5 minutos.
CREATE INDEX idx_session_publish_jobs_stuck
    ON session_publish_jobs (last_attempt_at) WHERE status = 'publishing';

-- A tela de agendamentos lista por loja, do mais proximo ao mais distante.
CREATE INDEX idx_session_publish_jobs_store
    ON session_publish_jobs (store_id, scheduled_for DESC);

COMMENT ON TABLE session_publish_jobs IS
    'RN-31/N3: agendamento de publicacao no Instagram. O agendador e NOSSO — a API de Content Publishing nao tem scheduled_publish_time e o container expira em 24h, entao ele e criado poucos minutos antes de scheduled_for. asset_path existe porque no caminho sincrono o arquivo e apagado do storage logo apos o publish; aqui ele e retido ate o desfecho do job (publicado, falho ou cancelado).';

COMMENT ON COLUMN session_publish_jobs.session_id IS
    'Preenchida NO DISPARO. A sessao so pode nascer depois que a midia existe (CreatePostEvent exige media_id), entao um agendamento vive sem sessao ate publicar.';

COMMENT ON COLUMN session_publish_jobs.asset_path IS
    'Chave no storage, nunca URL assinada: a presigned GET dura horas e o agendamento dura dias. A URL e gerada no disparo.';
