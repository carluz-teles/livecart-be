# Walkthrough: Testando o LiveCart com Instagram Emulator

## Pré-requisitos

1. Backend rodando (`docker compose up`)
2. Uma live criada no sistema (via API ou frontend)
3. Pelo menos um produto cadastrado com keyword

---

## 1. Iniciar o Emulador

```bash
cd /home/carluz_teles/livecart-be/apps/instagram-emulator

# Configurar webhook URL (aponta para o backend)
export EMULATOR_WEBHOOK_URL=http://localhost:3001/api/webhooks/instagram

# Rodar
go run .
```

Você verá:
```
  Instagram Webhook Emulator
  ----------------------------------------
  Server:  http://localhost:8080
  Webhook: http://localhost:3001/api/webhooks/instagram
  Account: @loja_livecart (17841405822304914)

  Type 'help' for available commands

>
```

---

## 2. Criar uma Live no Backend

Antes de testar comentários, crie uma live via API:

```bash
# Substitua YOUR_JWT pelo token de autenticação
curl -X POST http://localhost:3001/api/lives \
  -H "Authorization: Bearer YOUR_JWT" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Live de Teste",
    "platform": "instagram",
    "platformLiveId": "90010498-abc123"
  }'
```

**Importante**: Anote o `platformLiveId` - ele deve ser o mesmo `media_id` que o emulador vai usar.

---

## 3. Iniciar uma Live no Emulador

```bash
> start
```

Output:
```
Live started!
  media_id: 90010498-abc123
  Backend can poll: GET /17841405822304914/live_media
```

O prompt muda para `[LIVE] >` indicando que a live está ativa.

---

## 4. Simular Comentários de Compra

### Comentário simples (usuário aleatório):
```bash
[LIVE] > comment quero 2
```

### Comentário com usuário específico:
```bash
[LIVE] > comment --user maria "quero 3 unidades do CAMISETA"
```

### Comentário com keyword de produto:
```bash
[LIVE] > comment quero 1 BONE
```

O emulador envia um webhook `live_comments` para o backend, que:
1. Encontra a live pelo `media_id`
2. Detecta intenção de compra ("quero X")
3. Busca o produto pela keyword
4. Adiciona ao carrinho do usuário

---

## 5. Verificar no Backend

Veja os logs do backend:
```bash
docker compose logs -f api
```

Você verá logs como:
```
INFO  processing instagram comment  {"media_id": "90010498-abc123", "username": "maria", "text": "quero 3 unidades do CAMISETA"}
INFO  purchase intent detected  {"username": "maria", "quantity": 3}
INFO  product matched by keyword  {"keyword": "CAMISETA", "product_id": "..."}
INFO  added product to cart  {"cart_id": "...", "new_cart": true}
```

---

## 6. Comandos Úteis

| Comando | Descrição |
|---------|-----------|
| `start` | Inicia uma live |
| `end` | Encerra a live |
| `comment <texto>` | Envia comentário (usuário aleatório) |
| `comment --user <nome> <texto>` | Comentário como usuário específico |
| `c` | Atalho para `comment` |
| `burst 10` | Envia 10 comentários aleatórios |
| `dm <texto>` | Envia DM |
| `users` | Lista usuários simulados |
| `status` | Mostra status da live |
| `help` | Mostra ajuda |
| `exit` | Sair |

---

## 7. Fluxo Completo de Teste

```bash
# 1. Iniciar live
> start

# 2. Simular compras de diferentes usuários
[LIVE] > comment --user joao "quero 2 CAMISETA"
[LIVE] > comment --user maria "reserva 1 BONE pra mim"
[LIVE] > comment --user pedro "manda 3 CAMISETA"

# 3. Simular burst de comentários
[LIVE] > burst 5

# 4. Encerrar live
[LIVE] > end

# 5. Sair
> exit
```

---

## 8. Verificar Carrinhos Criados

Após os comentários, verifique os carrinhos via API:

```bash
# Listar orders/carrinhos
curl http://localhost:3001/api/orders \
  -H "Authorization: Bearer YOUR_JWT"
```

---

## Troubleshooting

### Comentário não cria carrinho:
- Verifique se a live existe com o `platformLiveId` correto
- Verifique se o produto tem keyword configurada
- Verifique se o texto contém padrão de compra ("quero", "reserva", "manda")

### Webhook não chega:
- Verifique se `EMULATOR_WEBHOOK_URL` está correto
- Verifique se o backend está rodando na porta 3001

### Produto não encontrado:
- Cadastre um produto com keyword (ex: "CAMISETA", "BONE")
- A keyword é case-insensitive
