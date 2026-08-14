-- Aviso no sino quando a releitura em massa do ERP termina.
--
-- A varredura roda em segundo plano e pode levar minutos; sem isto o lojista
-- clica no botao e nao tem como saber quando os saldos ficaram prontos, a nao
-- ser recarregando a lista de tempos em tempos ate parecer certo.
--
-- Sem indice unico de idempotencia, diferente de order_cancellation_reverted:
-- duas varreduras sao dois fatos distintos e ambas merecem aviso. A retentativa
-- do asynq nao duplica porque RunERPResync so devolve erro ANTES de notificar —
-- falha de produto individual nao derruba a varredura nem repete o aviso.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_type_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_type_check
    CHECK (type IN (
        'idea_comment',
        'idea_reply',
        'idea_status_change',
        'order_cancellation_reverted',
        'erp_resync_finished'
    ));
