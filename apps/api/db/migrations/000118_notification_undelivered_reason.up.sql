-- RN-38 — nao entrega com MOTIVO. Quando a janela do Instagram ja fechou, o
-- LiveCart nao finge que entregou: registra por que nao deu e mostra a lista
-- para o lojista chamar essas pessoas na mao.
--
-- Por que uma coluna nova e nao error_message: error_message carrega o erro CRU
-- do Graph ("status 400, body: {...}"). Nao da para mostrar isso ao lojista,
-- nao da para agrupar por motivo e nao da para distinguir "o Instagram recusou
-- a tentativa" de "nem tentamos porque a regra da plataforma proibe". O painel
-- da RN-38 precisa exatamente dessa distincao.
--
-- notification_logs.status e VARCHAR(50) SEM CHECK (000033), entao o valor novo
-- 'undelivered' e aditivo puro — nenhuma constraint a alterar. O preco disso e
-- que CountNotificationsByStatus (que conta sent/failed/cooldown) ignora o
-- estado novo em silencio; a leitura da RN-38 nao passa por ela.

ALTER TABLE notification_logs
    ADD COLUMN IF NOT EXISTS undelivered_reason VARCHAR(64);

COMMENT ON COLUMN notification_logs.undelivered_reason IS
    'RN-38: motivo canonico da nao entrega (comment_window_expired, no_eligible_comment, instagram_rejected). Preenchido quando status = undelivered. O texto exibido ao lojista sai de notification.UndeliverableReasonText — a coluna guarda o codigo, nunca a frase.';

-- Indice do painel: "quem nao foi avisado nesta campanha". Parcial de proposito
-- — a esmagadora maioria das linhas e 'sent' e nao tem por que entrar no
-- indice. event_id e NULLABLE na tabela, e linha sem evento simplesmente nao
-- aparece na lista por evento (que e a unica pergunta que o painel faz).
--
-- O predicado e a COLUNA, nao o status: a lista tambem precisa mostrar o que
-- foi tentado e recusado pelo Instagram, que continua com status 'failed'
-- porque foi tentativa real. Um indice sobre status = 'undelivered' deixaria
-- justamente essas linhas fora do plano.
CREATE INDEX IF NOT EXISTS idx_notification_logs_undelivered
    ON notification_logs (event_id, created_at DESC)
    WHERE undelivered_reason IS NOT NULL;

COMMENT ON INDEX idx_notification_logs_undelivered IS
    'RN-38: alimenta a lista de compradores nao avisados de um evento.';
