-- O log é a única fonte da atribuição por transmissão. Dropar a tabela perde
-- essa informação de forma irrecuperável: cart_items só guarda o first-touch.
DROP TABLE IF EXISTS cart_item_events;
