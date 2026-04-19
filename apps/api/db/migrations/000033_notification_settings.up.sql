-- =============================================================================
-- NOTIFICATION SETTINGS AND LOGS
-- =============================================================================
-- Adds notification settings to stores and creates notification_logs for tracking

-- 1. Add notification_settings JSONB column to stores
ALTER TABLE stores ADD COLUMN IF NOT EXISTS notification_settings JSONB DEFAULT '{
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

COMMENT ON COLUMN stores.notification_settings IS 'JSON settings for automatic notifications: templates, triggers, cooldown';

-- 2. Create notification_logs table for tracking and idempotency
CREATE TABLE notification_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    event_id UUID REFERENCES live_events(id) ON DELETE SET NULL,
    cart_id UUID REFERENCES carts(id) ON DELETE SET NULL,
    platform_user_id VARCHAR(255) NOT NULL,
    platform_handle VARCHAR(255),
    notification_type VARCHAR(50) NOT NULL, -- 'checkout_immediate', 'item_added', 'checkout_reminder'
    channel VARCHAR(50) NOT NULL DEFAULT 'instagram_dm', -- 'instagram_dm', 'whatsapp', 'email'
    status VARCHAR(50) NOT NULL DEFAULT 'pending', -- 'pending', 'sent', 'failed', 'skipped', 'cooldown'
    message_text TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    sent_at TIMESTAMPTZ
);

-- Indexes for querying
CREATE INDEX idx_notification_logs_store ON notification_logs(store_id);
CREATE INDEX idx_notification_logs_event ON notification_logs(event_id);
CREATE INDEX idx_notification_logs_cart ON notification_logs(cart_id);
CREATE INDEX idx_notification_logs_user ON notification_logs(platform_user_id);
CREATE INDEX idx_notification_logs_status ON notification_logs(status);
CREATE INDEX idx_notification_logs_created ON notification_logs(created_at);

-- Composite index for cooldown check (find recent notifications for same user/store)
CREATE INDEX idx_notification_logs_cooldown ON notification_logs(store_id, platform_user_id, created_at DESC);

COMMENT ON TABLE notification_logs IS 'Tracks all notification attempts for analytics and preventing duplicates';
