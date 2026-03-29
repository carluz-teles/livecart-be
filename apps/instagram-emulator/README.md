# Instagram Webhook Emulator

Emulador que replica **exatamente** a API de webhooks do Instagram/Meta para lives. Permite desenvolver e testar o backend antes de obter autorização da Meta.

Quando a autorização chegar, basta trocar o emulador pela API real - **mesmo formato, mesmos payloads**.

## Quick Start

```bash
cd apps/instagram-emulator

# Instalar dependências
go mod tidy

# Rodar o emulador
go run .
```

## Configuração

Crie um arquivo `.env` ou exporte as variáveis:

```bash
EMULATOR_PORT=8080                                        # Porta do servidor HTTP
EMULATOR_WEBHOOK_URL=http://localhost:3001/webhook/instagram  # URL do backend
EMULATOR_ACCOUNT_ID=17841405822304914                     # ID da conta Instagram
EMULATOR_USERNAME=loja_livecart                           # Username da conta
```

## Uso

Ao iniciar, o emulador:
1. Levanta um servidor HTTP na porta configurada (para polling do backend)
2. Inicia um CLI interativo para controlar a simulação

### Comandos Disponíveis

| Comando | Descrição |
|---------|-----------|
| `start` | Inicia uma live |
| `end` | Encerra a live |
| `comment <texto>` | Envia comentário (usuário aleatório) |
| `comment --user <username> <texto>` | Envia comentário como usuário específico |
| `dm <texto>` | Envia DM (usuário aleatório) |
| `dm --user <username> <texto>` | Envia DM como usuário específico |
| `burst [n]` | Envia n comentários aleatórios (padrão: 5) |
| `users` | Lista usuários simulados |
| `status` | Mostra status atual |
| `help` | Mostra ajuda |
| `exit` | Sair |

### Exemplo de Sessão

```
  Instagram Webhook Emulator
  ----------------------------------------
  Server:  http://localhost:8080
  Webhook: http://localhost:3001/webhook/instagram
  Account: @loja_livecart (17841405822304914)

  Type 'help' for available commands

> start
Live started!
  media_id: 90010498-abc123
  Backend can poll: GET /17841405822304914/live_media

[LIVE] > comment quero 2
Webhook live_comments sent!
  @maria_silva: "quero 2"

[LIVE] > comment --user joao "reserva 3 pra mim"
Webhook live_comments sent!
  @joao: "reserva 3 pra mim"

[LIVE] > burst 5
Sending 5 comments...
  [1] @pedro_lima: "quero 2"
  [2] @ana_costa: "quanto custa?"
  [3] @maria_silva: "reserva 1 pra mim"
  [4] @lucas_oliveira: "ainda tem?"
  [5] @carla_dias: "3 unidades por favor"
Burst complete! 5 webhooks sent

[LIVE] > dm "oi, ainda tem o produto?"
Webhook messages sent!
  DM from @fernanda_alves: "oi, ainda tem o produto?"

[LIVE] > end
Live ended!
  Backend will see empty data[] in GET /live_media

> exit
Bye!
```

## API Emulada

### Endpoints HTTP (para o backend consultar)

| Endpoint | Descrição |
|----------|-----------|
| `GET /{id}/live_media` | Retorna lives ativas (para polling) |
| `GET /webhook` | Verificação de webhook |
| `GET /health` | Health check |

### Webhooks Enviados (para o backend)

#### live_comments
```json
{
  "object": "instagram",
  "entry": [{
    "id": "17841405822304914",
    "time": 1678886400,
    "changes": [{
      "field": "live_comments",
      "value": {
        "from": { "id": "user_001", "username": "maria_silva" },
        "comment_id": "comment_abc123",
        "text": "quero 2 unidades",
        "media": { "id": "media_123", "media_product_type": "LIVE" }
      }
    }]
  }]
}
```

#### messages (DM)
```json
{
  "object": "instagram",
  "entry": [{
    "id": "17841405822304914",
    "time": 1678886500,
    "messaging": [{
      "sender": { "id": "user_001" },
      "recipient": { "id": "17841405822304914" },
      "timestamp": 1678886500,
      "message": { "mid": "mid_abc123", "text": "Oi, ainda tem?" }
    }]
  }]
}
```

## Migração para API Oficial

Quando obtiver autorização da Meta:

| Emulador | API Oficial Meta |
|----------|------------------|
| `POST webhooks` → backend | Meta envia webhooks → backend |
| `GET /live_media` | `GET graph.facebook.com/{id}/live_media` |
| `GET /webhook` verification | Mesmo formato |

**Única mudança necessária:** Apontar o backend para a API real.
