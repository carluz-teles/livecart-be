# Playbook: Testes de Convites (Invitations)

## Visao Geral do Fluxo

```
┌─────────────────┐      ┌─────────────────┐      ┌─────────────────┐
│  Admin envia    │      │  Clerk envia    │      │  Usuario aceita │
│  convite        │ ───► │  email c/ link  │ ───► │  convite        │
│  (Settings)     │      │  (automatico)   │      │  (accept-invite)│
└─────────────────┘      └─────────────────┘      └─────────────────┘
```

### Endpoints Backend

| Metodo | Endpoint | Descricao |
|--------|----------|-----------|
| POST | `/stores/{storeId}/invitations` | Criar convite |
| GET | `/stores/{storeId}/invitations` | Listar convites da loja |
| DELETE | `/stores/{storeId}/invitations/{id}` | Revogar convite |
| POST | `/stores/{storeId}/invitations/{id}/resend` | Reenviar convite |

### Paginas Frontend

| Pagina | Rota | Descricao |
|--------|------|-----------|
| Usuarios | `/settings/users` | Gerenciar membros e convites |
| Aceitar Convite | `/accept-invite?__clerk_ticket=...` | Aceitar convite via Clerk |

---

## Pre-requisitos

1. **Backend rodando**: `http://localhost:3001`
2. **Frontend rodando**: `http://localhost:3000`
3. **Usuario admin logado** com loja criada
4. **Email de teste**: usar formato `email+clerk_test@domain.com` para testes Clerk
5. **Codigo de verificacao Clerk**: `424242` (para emails de teste)

---

## Cenarios de Teste

### Cenario 1: Criar Convite (Usuario Novo)

**Objetivo**: Enviar convite para email que NAO tem conta Clerk

**Passos**:
1. Logar como admin da loja
2. Ir para `/settings/users`
3. Clicar em "Convidar membro"
4. Preencher:
   - Email: `novousuario+clerk_test@gmail.com`
   - Role: `member` ou `admin`
5. Clicar em "Enviar convite"

**Resultado esperado**:
- Toast de sucesso
- Convite aparece na lista com status `pending`
- Clerk envia email automaticamente

**Verificacao backend**:
```bash
# Listar convites da loja
curl -X GET http://localhost:3001/api/v1/stores/{storeId}/invitations \
  -H "Authorization: Bearer {token}"
```

---

### Cenario 2: Criar Convite (Usuario Existente)

**Objetivo**: Enviar convite para email que JA tem conta Clerk

**Passos**:
1. Logar como admin da loja
2. Ir para `/settings/users`
3. Clicar em "Convidar membro"
4. Preencher com email de usuario existente
5. Clicar em "Enviar convite"

**Resultado esperado**:
- Toast de sucesso
- Usuario existente recebe email com link de accept

---

### Cenario 3: Aceitar Convite (Usuario Novo - Sign Up)

**Objetivo**: Novo usuario aceita convite e cria conta

**Passos**:
1. Abrir link do email de convite
2. URL tera formato: `/accept-invite?__clerk_ticket=xxx&__clerk_status=sign_up`
3. Preencher formulario:
   - Nome
   - Sobrenome
   - Senha (min 8 caracteres)
4. Clicar em "Criar Conta e Entrar"

**Resultado esperado**:
- Conta criada no Clerk
- Usuario adicionado como membro da loja
- Redirect para dashboard `/`
- Usuario ve a loja no seletor de lojas

**Verificacao backend**:
```bash
# Verificar membership criado
curl -X GET http://localhost:3001/api/v1/stores/{storeId}/members \
  -H "Authorization: Bearer {token}"
```

---

### Cenario 4: Aceitar Convite (Usuario Existente - Sign In)

**Objetivo**: Usuario existente aceita convite e entra automaticamente

**Passos**:
1. Abrir link do email de convite
2. URL tera formato: `/accept-invite?__clerk_ticket=xxx&__clerk_status=sign_in`

**Resultado esperado**:
- Login automatico via ticket
- Tela de "Processando Convite..." -> "Convite Aceito!"
- Redirect para dashboard `/`
- Usuario ve a nova loja no seletor

---

### Cenario 5: Revogar Convite

**Objetivo**: Admin revoga convite pendente

**Passos**:
1. Logar como admin
2. Ir para `/settings/users`
3. Na lista de convites pendentes, clicar em "Revogar" no convite desejado
4. Confirmar acao

**Resultado esperado**:
- Convite removido da lista
- Link de convite invalido

**Verificacao**:
- Tentar acessar link de convite revogado -> erro "Convite Invalido"

---

### Cenario 6: Reenviar Convite

**Objetivo**: Reenviar convite pendente (gera novo token)

**Passos**:
1. Logar como admin
2. Ir para `/settings/users`
3. Na lista de convites, clicar em "Reenviar"

**Resultado esperado**:
- Novo email enviado
- Token antigo invalidado

---

### Cenario 7: Convite Expirado

**Objetivo**: Verificar tratamento de convite expirado

**Passos**:
1. Tentar acessar link de convite apos 7 dias

**Resultado esperado**:
- Pagina de erro: "O link do convite e invalido ou expirou"
- Botao para ir ao login

---

### Cenario 8: Convite Duplicado

**Objetivo**: Tentar enviar convite para email ja convidado

**Passos**:
1. Enviar convite para `user@example.com`
2. Tentar enviar outro convite para `user@example.com`

**Resultado esperado**:
- Erro 409 Conflict
- Mensagem: "invitation already exists for this email"

---

### Cenario 9: Email Mismatch

**Objetivo**: Usuario tenta aceitar convite com email diferente

**Passos**:
1. Usuario A recebe convite no email `a@example.com`
2. Usuario B (logado com `b@example.com`) tenta acessar o link

**Resultado esperado**:
- Erro 403 Forbidden
- Mensagem: "invitation email does not match your account"

---

## Testes de Permissao

### Cenario 10: Member tenta criar convite

**Passos**:
1. Logar como member (nao admin)
2. Tentar acessar endpoint de criar convite

**Resultado esperado**:
- Erro 403 Forbidden
- Apenas admin/owner pode convidar

---

## Testes via cURL

### Criar convite
```bash
curl -X POST http://localhost:3001/api/v1/stores/{storeId}/invitations \
  -H "Authorization: Bearer {token}" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "test@example.com",
    "role": "member"
  }'
```

### Listar convites
```bash
curl -X GET http://localhost:3001/api/v1/stores/{storeId}/invitations \
  -H "Authorization: Bearer {token}"
```

### Revogar convite
```bash
curl -X DELETE http://localhost:3001/api/v1/stores/{storeId}/invitations/{invitationId} \
  -H "Authorization: Bearer {token}"
```

### Reenviar convite
```bash
curl -X POST http://localhost:3001/api/v1/stores/{storeId}/invitations/{invitationId}/resend \
  -H "Authorization: Bearer {token}"
```

---

## Checklist de Verificacao

- [ ] Convite criado aparece na lista
- [ ] Email de convite enviado pelo Clerk
- [ ] Usuario novo consegue criar conta via convite
- [ ] Usuario existente entra automaticamente
- [ ] Membro aparece na lista apos aceitar
- [ ] Convite revogado nao funciona mais
- [ ] Reenvio gera novo email
- [ ] Convite expirado mostra erro apropriado
- [ ] Permissoes respeitadas (apenas admin convida)
- [ ] Email mismatch bloqueado

---

## Notas Importantes

1. **Clerk SDK**: Convites sao criados via Clerk quando a loja tem `clerk_org_id`
2. **Emails automaticos**: Clerk envia emails automaticamente, nao precisamos implementar
3. **Ticket strategy**: Accept-invite usa `strategy: 'ticket'` do Clerk
4. **Fallback local**: Se Clerk falhar, sistema usa banco local (legacy)
