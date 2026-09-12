# Identificadores e busca interna de produtos

O catálogo salva SKU em `products.sku` e GTIN/código de barras em
`products.barcode`. A busca interna consulta o PostgreSQL por nome, keyword,
SKU e código de barras; não consulta o ERP. Espaços e quebras de linha nas
extremidades do termo são desconsiderados. Os códigos permanecem como texto,
preservando zeros à esquerda.

## Cadastro e edição

No contrato JSON do catálogo, os identificadores ficam em `shipping.sku` e
`shipping.barcode`, mesmo quando o produto não possui peso ou medidas. Na
resposta do ERP, eles vêm no produto como `sku` e `gtin`; o formulário faz
essa tradução antes de salvar.

Em `PUT /stores/{storeId}/products/{productId}`:

- `shipping` omitido ou `null`: preserva todo o perfil existente.
- `shipping` fornecido: substitui peso, medidas, formato e seguro. As quatro
  medidas podem ser removidas juntas; preenchimento parcial continua inválido.
- `shipping.sku`/`shipping.barcode` omitido ou `null`: preserva o identificador.
- Identificador informado como `""`: remove explicitamente o valor.
- `imageUrl` omitido ou `null`: preserva a imagem; `""` remove.

O formulário omite identificadores inalterados para preservar uma atualização
do SYNC ocorrida depois de sua abertura. A preservação também é feita no SQL,
evitando que uma leitura anterior sobrescreva campos omitidos.

## Recuperação pelo SYNC

O SYNC relê os produtos vinculados ao ERP, inclusive cadastros legados sem
identificadores. Dados de catálogo são salvos independentemente da admissão
do estoque. Se o cálculo de reservas ou a sequência de movimentos impedir
aplicar o estoque, o saldo local é preservado e o SYNC tenta uma nova leitura.
Uma pendência persistente entra na contagem de falhas, sem desfazer os dados
de catálogo recuperados.

Identificadores ausentes na resposta do ERP não apagam valores locais. Quando
também não existem no ERP, o SYNC não consegue preenchê-los.

Logs para acompanhamento:

- `product identifiers synced`: informa produto, loja, origem, quais campos
  foram preenchidos e quais seguem ausentes localmente ou na resposta do ERP.
- `ERP resync identifier coverage`: totais de produtos sem SKU/código de barras
  antes e depois, além dos totais sincronizados e com falha.
- `ERP resync: product failed`: identifica produtos cuja releitura não concluiu.

## Publicação e validação

Publicar o backend antes ou junto do frontend: o novo formulário usa a
preservação de identificadores omitidos. Depois de ambos em produção, executar
o SYNC da integração para recuperar os cadastros antigos. Não há migração de
banco nem recuperação automática disparada pelo deploy.

Regressões cobertas por `service_identifiers_test.go`, `service_catalog_test.go`,
`service_resync_test.go` e, no frontend, `tests/product-catalog.spec.ts`.
Os testes de banco usam PostgreSQL local e um ERP simulado.

A geração global do Swagger está bloqueada por uma referência preexistente
a `InstagramLivesResponse` em `internal/integration/handler.go`. Os arquivos
gerados não foram editados manualmente; este documento registra o contrato
de preservação até a correção do gerador global.
