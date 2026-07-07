-- Revert WhatsApp via Twilio (PRD 006)

-- 5. Notification settings: remove cart_recovery key and restore previous default
UPDATE stores
SET notification_settings = notification_settings - 'cart_recovery'
WHERE notification_settings ? 'cart_recovery';

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
  }
}'::jsonb;

-- 5. Provider message correlation
DROP INDEX IF EXISTS idx_notification_logs_provider_message;
ALTER TABLE notification_logs DROP COLUMN IF EXISTS provider_message_id;

-- 4. Index
DROP INDEX IF EXISTS idx_carts_whatsapp_recovery;

-- 3. Customer opt-out
ALTER TABLE customers DROP COLUMN IF EXISTS whatsapp_opted_out;

-- 2. Cart consent
ALTER TABLE carts DROP COLUMN IF EXISTS whatsapp_consent_at;
ALTER TABLE carts DROP COLUMN IF EXISTS whatsapp_consent;

-- 1. Constraints back to pre-075 state
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_provider_check
    CHECK (provider IN ('mercado_pago', 'pagarme', 'tiny', 'instagram', 'melhor_envio', 'smartenvios'));

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_type_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_type_check
    CHECK (type IN ('payment', 'erp', 'social', 'shipping'));
