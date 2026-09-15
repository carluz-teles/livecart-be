# Mesmo Instagram em empresas diferentes

Cada live, post, reel ou story vende por uma loja escolhida no LiveCart.
O perfil do Instagram pode ser o mesmo, mas cada loja conserva seu cadastro de
integração, suas autorizações, produtos, estoque, clientes, carrinhos, pedidos,
ERP, conta Pagar.me e configuração de frete.

## Configuração

Conectar o mesmo perfil pelo OAuth do Instagram em **Configurações → Integrações**
de cada loja. O callback existente salva a conexão apenas na loja que iniciou a
autorização. Não é necessário copiar ERP, pagamento ou catálogo da outra empresa.
As conexões usam o mesmo aplicativo/webhook do LiveCart; não se configura outro
webhook na Meta para cada empresa.

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

Os serviços externos nesses testes são simulados: não houve publicação, DM,
cobrança ou pedido real nas empresas. Não há migration nem associação automática
de contas de produção neste pacote.

Após publicar o backend, autorizar o perfil na segunda loja e conferir suas
integrações próprias de ERP, pagamento e frete antes de iniciar vendas.
Em 15/09/2026 a leitura de produção mostrou `@cantodaart_` conectado à Canto da
Art e nenhum cadastro de integração na Maricelia Decor. Esta leitura não ativou
nem modificou qualquer integração.
