# Mesmo Instagram em empresas diferentes

Cada live, post, reel ou story vende por uma loja escolhida no LiveCart.
O perfil do Instagram pode ser o mesmo. Cada loja conserva seu cadastro de
integração, produtos, estoque, clientes, carrinhos, pedidos, ERP, conta Pagar.me
e configuração de frete. Somente a autorização do Instagram é compartilhada.

## Configuração

A conexão já autorizada é a principal. A segunda loja recebe uma integração
social com `instagram_credentials_source_id` apontando para essa conexão.
Ela não guarda cópia do token nem prazo de expiração próprio. A referência é uma
coluna controlada pelo servidor; informar um ID nos metadados de uma requisição
não concede acesso às credenciais de outra loja.

Para uma segunda loja do mesmo lojista, usar a associação controlada descrita
abaixo, sem iniciar um OAuth independente. As conexões usam o mesmo aplicativo
e webhook do LiveCart; não é necessário configurar outro webhook na Meta.

### Token, renovação e desconexão

- Consultas da segunda loja leem as credenciais e a identidade atual da conexão
  principal, preservando o ID da loja que realiza a operação.
- O worker agenda somente a conexão principal. Requisições das duas lojas e
  instâncias diferentes da API usam o mesmo bloqueio no PostgreSQL ao renovar.
  Quem aguardou reutiliza a renovação já salva.
- OAuth na conexão principal e renovação usam esse mesmo bloqueio para gravar
  token e metadados. Reconectar o mesmo perfil recupera as duas lojas; trocar
  por outro perfil enquanto houver vínculos é recusado.
- A segunda loja não pode iniciar uma autorização independente. Sua exclusão
  remove apenas o vínculo local. Excluir a conexão principal é recusado enquanto
  houver vínculos, inclusive inativos, evitando deixá-los sem credenciais.
- Falhas temporárias de rede, limite ou indisponibilidade não marcam o token como
  revogado. Expiração definitiva ou rejeição explícita de autenticação coloca
  a conexão principal em erro; esse estado é refletido nas lojas vinculadas.
- Erros da renovação não registram o token presente na URL nem o corpo bruto
  retornado pela Meta. Falha ao salvar a renovação não é reportada como sucesso.

A [documentação oficial da Meta](https://developers.facebook.com/documentation/instagram-platform/reference/refresh_access_token/)
informa que a renovação usa o próprio token de longa duração, ainda válido e
emitido há pelo menos 24 horas, estendendo sua validade por 60 dias. Não foi
encontrada nessa documentação uma garantia de que **toda** nova autorização
revogue o token anterior; não dependemos dessa suposição.

Na loja escolhida para vender, criar o evento e vincular a publicação à sessão.
Comentários e respostas a stories resolvem a loja pela mídia → sessão → evento.
A conta do comprador e o perfil compartilhado não determinam a empresa da venda.
A mesma pessoa comprando em eventos das duas empresas recebe carrinhos separados.

A restrição existente `uq_lsp_media_in_flight` impede vincular a mesma publicação
a dois eventos ainda não liberados. Uma publicação pode ser reutilizada depois
que seu vínculo anterior for liberado pelo fluxo normal de encerramento.

## Ajuste nas mensagens diretas

Uma DM comum não identifica a empresa quando duas lojas compartilham o perfil.
O código anterior selecionava uma integração com `LIMIT 1`; agora resolve todas
as lojas conectadas e não atribui essa mensagem arbitrariamente a nenhuma delas.
Esse tratamento não interfere nas vendas por comentário ou resposta a story.

O código de **Testar notificação** identifica a loja de destino somente entre
as lojas conectadas ao Instagram que recebeu a DM. O consumo é atômico: código
expirado, repetido ou ambíguo não substitui um destinatário. O identificador do
remetente basta para concluir a configuração, mesmo sem seu nome público.
Falhas de banco são propagadas para a recuperação existente do consumidor.

Log da DM sem empresa definida:
`instagram message ignored: shared account without store context`, com
`account_id`, `message_id` e `connected_stores`, sem conteúdo da conversa.

## Validação e publicação

Testes locais com PostgreSQL descartável cobrem:

- Duas lojas, mesmo Instagram e comprador, código de produto igual, produtos e
  preços diferentes, Tiny/Bling distintos e duas credenciais Pagar.me.
- Carrinhos e seleção de ERP/pagamento separados em live, post, reel e story;
  repetição do comentário sem item duplicado.
- Recusa do vínculo simultâneo da mesma mídia e do uso da integração de
  pagamento de outra loja.
- DM genérica sem atribuição arbitrária; código de teste encaminhado à segunda
  loja; loja desconectada ou não relacionada excluída da captura.
- Código expirado, colisão de códigos, reenvio e captura concorrente.
- Associação com uma única credencial armazenada, repetição idempotente e
  recusa de conta errada, destino já conectado e encadeamento de vínculos.
- Oito chamadas simultâneas de instâncias diferentes realizando uma única
  renovação; atualização visível nas duas lojas, preservando a loja de venda.
- Falha de persistência, erros temporários/permanentes, reconexão, bloqueio de
  troca do perfil e desconexão sem invalidar a loja principal.
- Tentativa de obter credenciais de outra loja via metadados sem efeito.

Os serviços externos nesses testes são simulados: não houve publicação, DM,
cobrança ou pedido real nas empresas. A migration `000156` adiciona a referência
e suas restrições; não associa nem altera as credenciais de contas existentes.

### Sequência de ativação

1. Publicar a migration e o backend correspondente. Confirmar que todas as
   instâncias estão com o código novo antes de criar qualquer vínculo.
2. Após autorização específica de escrita, executar
   [link-shared-instagram.sql](operations/link-shared-instagram.sql). O script
   valida a conta esperada, token com validade restante, loja de destino ativa
   e ausência de integração social conflitante. Cria somente a referência do
   Instagram, em transação, sem copiar ou imprimir tokens. Uma segunda execução
   com os mesmos parâmetros não cria outra integração.
3. Conferir o perfil exibido nas duas lojas, os cadastros próprios de ERP,
   pagamento e frete, e a seleção da mídia na loja que fará a venda.

Parâmetros preparados para o caso solicitado, ainda sem execução em produção:

| Parâmetro | Valor |
| --- | --- |
| `source_integration_id` | `ba0671e9-3236-4ff1-b64a-ade4e838ea98` — Instagram da Canto da Art |
| `target_store_id` | `f0a1bd0c-c1d8-489e-8a95-e286b3e785d2` — Maricelia Decor |
| `expected_account_id` | `17841400705156066` — `@cantodaart_` |

Para desfazer, excluir somente a integração social criada na segunda loja, pelo
fluxo normal de desconexão. Antes de voltar para backend sem suporte a vínculos,
remover todos esses vínculos. A migration de rollback recusa removê-los
silenciosamente.

Em 15/09/2026 a leitura de produção mostrou `@cantodaart_` conectado à Canto da
Art e nenhum cadastro de integração na Maricelia Decor. Esta leitura não ativou
nem modificou qualquer integração.
