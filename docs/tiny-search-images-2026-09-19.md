# Busca Tiny: demora durante SYNC e foto ausente

## Evidências de produção (somente leitura)

Backend `1af06c2` e frontend `989afe9` estavam publicados com sucesso em
19/09/2026. Portanto, a investigação já considera a primeira correção da busca.

O usuário confirmou que “Maristela” se refere à **Maricelia Decor**. Não tem o
código de barras usado na tentativa. As buscas individuais inicialmente
localizadas pertencem à **Canto da Art**, integração
`ae56635b-b978-4959-bb39-4c7ffed4aabe`; os tempos abaixo não são da Maricelia.

- Às 11h41 de Brasília, a busca demorou 15,175 s: GTIN vazio, SKU encontrado,
  consulta de nome vazia. Mesmo após encontrar o SKU, aguardou a consulta de nome.
- Às 11h44, outra busca equivalente demorou 6,390 s.
- Às 12h23, um GTIN encontrado levou 6,573 s. O registro de cota imediatamente
  anterior indica espera de 6,357 s antes do envio.
- O SYNC completo de 1.538 produtos estava ativo nesses intervalos. Os registros
  confirmam concorrência pelo mesmo orçamento de leitura da conta.
- A cota aprendida era um envio a cada 1.250 ms. Não foi necessário alterar a
  taxa, consultar credenciais Tiny ou escrever em produção para diagnosticar.
- Os detalhes dos produtos Tiny `832975980` e `848750310` foram lidos com sucesso
  em 1,953 s e 4,475 s. Ambos têm imagem no catálogo local. O frontend continuava
  renderizando a prévia sem imagem, e a galeria só aparecia com mais de uma foto.

### Maricelia Decor

Loja `f0a1bd0c-c1d8-489e-8a95-e286b3e785d2`, integração Tiny ativa
`1078e202-4a29-440f-818a-db0f04974554`. Consultas em produção somente leitura:

- O último SYNC completo terminou em 17/09 às 15h37 de Brasília, com 44/44
  produtos e nenhuma falha. O catálogo atualmente tem 66 produtos. Não havia
  um SYNC completo pendente nessa integração.
- Na amostra de 1.500 logs da loja, de 19/09 entre 13h46 e 17h03 de Brasília,
  houve 416 esperas pela cota: mediana de 1,027 s, percentil 95 de 6,450 s e
  máximo de 23,916 s (às 16h57). São chamadas da conta, sem associação comprovada
  à busca relatada; não se deve apresentá-las como duração daquela importação.
- Nesse intervalo, 89 lotes de recuperação automática verificaram 780 saldos.
  O código dessa rotina disputava a mesma cota sem a prioridade já adicionada
  ao SYNC completo. A correção agora cobre também essa leitura periódica.
- Não foram localizados eventos de busca/importação dessa conta nos registros
  consultados. Sem o termo ou a tentativa correspondente, não é possível
  identificar o produto cuja foto faltou nem medir aquela consulta específica.

## Correções

1. SKU exato e único encerra a busca Tiny sem consultar nome. GTIN continua
   prioritário; SKU parcial mantém a busca por nome. Outros provedores conservam
   o fluxo de busca anterior.
2. Busca, seleção e confirmação da importação recebem preferência sobre o SYNC
   completo Tiny e a leitura periódica de recuperação de estoque. A preferência
   é compartilhada no PostgreSQL, por conta e
   categoria, através de um prazo renovável de cinco segundos. Expira sem
   limpeza manual se a requisição for cancelada ou a instância cair.
3. O intervalo de envio e o bloqueio anunciado pelo ERP continuam obrigatórios
   inclusive para a consulta prioritária. Pagamentos, pedidos, escrita, outras
   contas e o limitador Bling não recebem a restrição do SYNC.
   A verificação de estoque disparada por webhook mantém seu fluxo; somente a
   leitura do varredor periódico cede prioridade. O processamento posterior da
   fila de espera conserva o contexto original.
4. A linha do produto usa a imagem recebida nos detalhes. A galeria funciona
   com uma imagem e com `imageUrl` sem `imageUrls`; a imagem padrão já aparece
   selecionada. Ausência de imagem confirmada é informada na tela.
5. Quando existe um único resultado elegível, a leitura dos detalhes começa
   antes do clique. Havendo vários resultados, somente o escolhido é consultado.
   Não voltamos a buscar estoque de todos os produtos para desenhar a lista.
6. Trocar o termo cancela a consulta anterior imediatamente durante o debounce.
   Uma falha de rede/timeout não dispara mais duas tentativas silenciosas;
   o botão permite nova tentativa explícita.

## Publicação e limites

A migration **161** adiciona apenas `api_rate_budgets.interactive_until`.
Não há histórico de requisições nem novas linhas por busca. Publicar a migration
com o backend e depois o frontend. Nenhum ajuste manual de saldo, pedido,
integração ou fila de produção faz parte desta correção.

A preferência reduz a disputa com o catálogo; não remove limites de plano nem
garante tempo fixo quando a própria Tiny estiver lenta ou sem cota. Consultas
de pedidos e outras operações necessárias ainda compartilham o limite. As fotos
de listas com vários resultados chegam ao selecionar o produto; resultados
únicos carregam a foto antecipadamente.

## Regressões

- SKU exato não consulta nome; SKU parcial preserva todos os resultados.
- Preferência entre duas instâncias, isolamento por conta e entre leitura e
  escrita, respeito a cooldown, cancelamento e expiração da preferência.
- Navegador: foto única efetivamente carregada, galeria sem array de imagens,
  ausência de leitura duplicada após clicar, nenhum carregamento de todos os
  resultados e nova tentativa somente após ação do usuário.
- Preservados os testes anteriores de estoque disponível, importação parcial,
  retomada de variantes, limite de tempo do corpo HTTP e isolamento entre lojas.

Validação final local: 40 pacotes do backend passaram na suíte completa com
`-tags=integration`; 48 testes frontend passaram (36 de operações e 12 da
importação). Os cenários de busca/cota também passaram com `-race`.
`go build ./apps/api/...`, `npm run build` e `git diff --check` passaram.
Após confirmar a conta e incluir a varredura periódica, passaram novamente os
testes de estoque Tiny, prioridade compartilhada, limites Tiny/Bling e SYNC
completo, com PostgreSQL local e `-race`, além do build do backend.
As correções foram preparadas a partir de `main` na branch
`fix/tiny-search-latency-and-images` dos dois repositórios. A aplicação em
produção depende do merge e deploy pelo responsável; os acessos de diagnóstico
foram somente leitura.

Documentação consultada:
[listagem de produtos](https://api-docs.erp.olist.com/api-reference/produtos/listar-produtos),
[imagens de produtos](https://api-docs.erp.olist.com/api-reference/produtos/obter-anexos-e-imagens-do-produto).
