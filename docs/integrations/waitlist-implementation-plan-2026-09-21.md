# Fila de espera — implementação e implantação

Contrato: [14 decisões confirmadas](waitlist-business-decisions-2026-09-21.md).

## Comportamento implementado

- A promoção vira reserva normal do carrinho. Nenhum GET varre a fila global ou expira uma unidade promovida. Rotinas legadas de expiração individual não fazem alterações.
- O fechamento comercial E é imutável. A elegibilidade ao adicional Y é congelada nesse momento; a data do carrinho usa E+X+Y quando cabível. Aumentos de configuração alcançam carrinhos abertos; reduções não encurtam o prometido nem permitem conceder duas vezes o mesmo aumento. VIP não expira.
- Pagamento confirmado, cancelamento e expiração encerram a espera atomicamente. Pagamento cobra somente as unidades disponíveis; a fila restante não entra no bruto coberto pelo pagamento.
- Cada comentário adicional cria uma solicitação própria. A chave do comentário protege contra reentrega, sem descartar compras adicionais da mesma pessoa.
- Um alocador serializado por produto atende a fila global da loja. Atende parcialmente, conserva a prioridade original, não fura a fila quando a primeira transação está ocupada e não transforma estoque negativo em saldo disponível.
- Estoque, carrinho, saldo da solicitação e eventos duráveis são gravados na mesma transação. Falhas transitórias ou finalização ERP em andamento permanecem retentáveis.
- Lotes de preço preservam os centavos e a sessão de cada adição. Checkout, gateway, descontos, frete, grade ERP, recibo e pedido usam essa projeção. Quantidades de estoque continuam agregadas onde necessário; preços diferentes não viram uma média arredondada.
- O eco de uma grade ERP com preços distintos é comparado por produto/preço/quantidade. Reflexos legítimos do ERP preservam as unidades ainda em espera e seus preços.
- A UI permite Y de 0 a 30 dias em minutos/horas/dias, exibe os preços por lote, o prazo do carrinho e a regra de encerrar a espera ao pagar. Removidas promessas de notificação que o backend não enviava.
- Enquanto uma linha tiver espera pendente, a tela orienta encerrar essa espera antes de editar a quantidade dos itens disponíveis. Isso evita interpretar o contador de disponíveis como quantidade total, removendo solicitações por engano.

## Migrações

162 registra E, elegibilidade e âncoras legadas; 163 cria lotes/preços e projeções; 164 permite solicitações independentes e remove timers individuais; 165 fecha espera junto do carrinho; 166 preserva lotes nas uniões; 167 calcula cobertura financeira sem cobrar espera; 168 encerra a compra dos carrinhos vinculados quando o pedido principal termina, preservando seus vínculos e pagamentos.

O backfill usa o preço armazenado no carrinho, nunca o preço atual do catálogo. Prazos legados sem evidência suficiente são preservados e recebem âncora para aumentos posteriores. Não são recriados automaticamente carrinhos, produtos ou entradas que já expiraram no incidente. O rollback da 164 não reinstala a restrição antiga de uma entrada por comprador/produto, pois isso descartaria solicitações válidas; a reversão operacional deve ser tratada como uma mudança revisada, não como apagamento de lotes.

## Validação

- Testes PostgreSQL reais: fechamento agendado tardio, elegibilidade, monotonicidade, VIP, concorrência, parcial, loja isolada, cabeça ocupada, cancelamento, pagamento, união, rollback de eventos e preços.
- Testes de checkout: total exato, rejeição de linhas duplicadas e de trocas de mesmo valor, compatibilidade de fingerprint legado e compensação de erro ERP sem sobrescrever edições novas.
- Testes ERP: payload completo por preço, reconciliação do eco e preservação da espera, materialização e atribuição às sessões.
- Frontend: testes de operações, navegador com componentes reais em fixture isolada e build de produção.
- A evidência de produção foi coletada somente por SELECT em transação READ ONLY usando a CLI Railway. Não houve reprocessamento, alteração de estoque nem nova cobrança em produção.

A documentação oficial de atualização de itens da Tiny define o envio da grade completa e o recálculo dos totais: https://api-docs.erp.olist.com/api-reference/pedidos/atualizar-itens-do-pedido . Os testes de contrato do adapter são locais; esta entrega não constitui validação de uma nova compra real na conta do cliente.

## Publicação

Branches de produção partem de main; os mesmos commits serão aplicados diretamente em stg atualizado. Publicar primeiro o backend (migrações e contrato priceLots), depois o frontend. O frontend mantém fallback para o preço único legado. O usuário abre e aprova os PRs para main.

## Recuperação do incidente

[Lista de carrinhos e produtos para conferência manual](waitlist-manual-recovery-2026-09-21.md). Reconsultar antes de incluir: o lojista pode ter corrigido uma linha durante a investigação. Carrinho já pago exige preservar o pagamento e combinar como será cobrada a unidade adicional; estoque zero exige conferência física/ERP antes de inclusão.
