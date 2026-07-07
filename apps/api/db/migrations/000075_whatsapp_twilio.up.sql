-- =============================================================================
-- WHATSAPP VIA TWILIO (PRD 006 - Recuperacao de Carrinho)
-- =============================================================================

-- 1. Integrations: new type 'communication' + provider 'twilio_whatsapp'.
--    Keep in sync with CreateIntegrationRequest oneof and providers/factory.go.
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_type_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_type_check
    CHECK (type IN ('payment', 'erp', 'social', 'shipping', 'communication'));

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_provider_check
    CHECK (provider IN ('mercado_pago', 'pagarme', 'tiny', 'instagram', 'melhor_envio', 'smartenvios', 'twilio_whatsapp'));

-- 2. Cart-level WhatsApp consent (LGPD). Captured at checkout together with
--    customer_phone; recovery/reminder messages require consent = true.
ALTER TABLE carts ADD COLUMN IF NOT EXISTS whatsapp_consent BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS whatsapp_consent_at TIMESTAMPTZ;

-- 3. Customer-level opt-out (reply SAIR/PARAR on WhatsApp). Overrides any
--    cart-level consent for future sends.
ALTER TABLE customers ADD COLUMN IF NOT EXISTS whatsapp_opted_out BOOLEAN NOT NULL DEFAULT FALSE;

-- 4. Recovery worker sweep index: unpaid carts whose expiration has passed
--    and that have a phone + consent. The worker re-checks state at send time;
--    attempts are counted from notification_logs (cart_id + type).
CREATE INDEX IF NOT EXISTS idx_carts_whatsapp_recovery
  ON carts (expires_at)
  WHERE whatsapp_consent = TRUE
    AND customer_phone IS NOT NULL
    AND payment_status IS DISTINCT FROM 'paid';

-- 5. Provider message correlation: Twilio status callbacks (sent/delivered/
--    read/failed) arrive keyed by the provider message SID; we update the
--    original log row through this column.
ALTER TABLE notification_logs ADD COLUMN IF NOT EXISTS provider_message_id VARCHAR;
CREATE INDEX IF NOT EXISTS idx_notification_logs_provider_message
  ON notification_logs (provider_message_id)
  WHERE provider_message_id IS NOT NULL;

-- 6. Notification settings: cart_recovery template + trigger config. New
--    stores get it via column default; existing stores are backfilled with a
--    merge that preserves their current settings.
ALTER TABLE stores ALTER COLUMN notification_settings SET DEFAULT '{
  "checkout_immediate": {
    "enabled": true,
    "on_first_item": true,
    "on_new_items": true,
    "cooldown_seconds": 30,
    "template": "Olá {handle}! 🛒\n\nVocê pediu {produto} na live!\n\nTotal: {total}\n\nFinalize aqui: {link}\n\n⏰ Válido por {expira_em}"
  },
  "item_added": {
    "enabled": true,
    "template": "Oi {handle}! ➕\n\nNovo item adicionado: {produto}\n\nSeu carrinho agora tem {total_itens} itens\nTotal: {total}\n\nFinalize: {link}"
  },
  "checkout_reminder": {
    "enabled": true,
    "template": "Oi {handle}! 🛒\n\nSeu carrinho com {total_itens} itens está esperando!\n\nTotal: {total}\n\nFinalize aqui: {link}\n\n⏰ Válido por {expira_em}"
  },
  "cart_recovery": {
    "enabled": false,
    "delay_minutes": 30,
    "max_attempts": 1,
    "quiet_hours_start": 21,
    "quiet_hours_end": 8,
    "recover_ended_events": true,
    "template": "Oi {nome}! Vi que seu carrinho na {loja} ficou pra trás 🛒\n\n{total_itens} itens · {total}\n\nGaranti tudo de novo pra você. Finalize aqui: {link}"
  }
}'::jsonb;

UPDATE stores
SET notification_settings = COALESCE(notification_settings, '{}'::jsonb) || jsonb_build_object(
  'cart_recovery', jsonb_build_object(
    'enabled', false,
    'delay_minutes', 30,
    'max_attempts', 1,
    'quiet_hours_start', 21,
    'quiet_hours_end', 8,
    'recover_ended_events', true,
    'template', 'Oi {nome}! Vi que seu carrinho na {loja} ficou pra trás 🛒' || E'\n\n' || '{total_itens} itens · {total}' || E'\n\n' || 'Garanti tudo de novo pra você. Finalize aqui: {link}'
  )
)
WHERE notification_settings IS NULL OR NOT (notification_settings ? 'cart_recovery');
