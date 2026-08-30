package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// Re-hospedagem de imagem de produto que EXPIRA.
//
// O Bling devolve as imagens internas como links S3 ASSINADOS. Medido em
// 29/08/2026 contra a conta real:
//
//	link          Expires=1788651842 → 05/09/2026 20:44  (~7 DIAS)
//	linkMiniatura Expires=1788048842 → 29/08/2026 21:14  (~30 MINUTOS)
//
// e o JSON ainda traz um campo `validade` confirmando a data.
//
// O LiveCart guarda a imagem do produto como URL (`products.image_url`), não
// como arquivo. Importar um produto do Bling e guardar a URL é, portanto,
// garantir que a foto some da vitrine em uma semana — e a miniatura, em meia
// hora. O Tiny não tem esse problema, então o caminho de import atual nunca
// precisou disto.
//
// A regra é copiar só o que expira. URL do próprio lojista (as `externas` do
// Bling, ou qualquer imagem do Tiny) fica como está: re-hospedar o que já é
// estável só gastaria armazenamento e criaria uma segunda cópia para
// desatualizar.

// URLEfemera diz se a URL tem prazo de validade embutido.
//
// A heurística é o parâmetro de assinatura na query, que é como os provedores
// de objeto assinam URL temporária: `Expires` + `Signature` (S3 clássico, que é
// o que o Bling usa) ou `X-Amz-Expires` (SigV4).
//
// Deliberadamente CONSERVADORA nas duas direções: um falso positivo custa uma
// cópia desnecessária; um falso negativo custa a foto do produto sumir da
// vitrine sem ninguém entender por quê.
func URLEfemera(bruta string) bool {
	u, err := url.Parse(bruta)
	if err != nil || u.Scheme == "" {
		return false
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "" || q.Get("X-Amz-Signature") != "" {
		return true
	}
	// S3 clássico: Expires é um epoch e vem junto de Signature.
	if q.Get("Expires") != "" && q.Get("Signature") != "" {
		return true
	}
	return false
}

// ExpiraEm devolve quando a URL vence, quando dá para saber.
//
// Serve para o log dizer "esta foto morre em 28 minutos" em vez de só "url
// efêmera" — a diferença entre um alerta acionável e um ruído.
func ExpiraEm(bruta string) (time.Time, bool) {
	u, err := url.Parse(bruta)
	if err != nil {
		return time.Time{}, false
	}
	if v := u.Query().Get("Expires"); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(epoch, 0), true
		}
	}
	return time.Time{}, false
}

// tamanhoMaximoDeImagem é o teto do que baixamos por foto.
//
// Existe porque a origem é de terceiro: sem teto, um arquivo grande (ou um
// servidor que nunca fecha o corpo) segura memória e uma goroutine durante um
// import que o lojista está esperando na tela.
const tamanhoMaximoDeImagem = 12 << 20 // 12 MiB

// ReHospedarImagem baixa a imagem e a devolve numa URL nossa, estável.
//
// Devolve a URL ORIGINAL quando não há o que fazer — URL estável, storage não
// configurado, ou falha no download. Falhar aqui NÃO pode derrubar o import: um
// produto com foto que vai expirar ainda é melhor do que produto nenhum, e o
// aviso fica no log com o prazo.
func (s *Service) ReHospedarImagem(ctx context.Context, storeID, produtoID, original string) string {
	if original == "" {
		return original
	}
	log := logger.From(ctx, s.logger).With(
		zap.String("store_id", storeID),
		zap.String("external_product_id", produtoID),
	)

	if !URLEfemera(original) {
		// URL estável: copiar criaria uma segunda cópia para desatualizar.
		return original
	}
	if s.storage == nil {
		if quando, ok := ExpiraEm(original); ok {
			log.Warn("imagem do produto EXPIRA e não há storage configurado para copiá-la",
				zap.Time("expira_em", quando),
				zap.Duration("faltam", time.Until(quando)))
		}
		return original
	}

	// Prazo curto e próprio: o import é síncrono e o lojista está olhando a tela.
	baixarCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	corpo, contentType, err := baixarImagem(baixarCtx, original)
	if err != nil {
		log.Warn("não consegui baixar a imagem do ERP para re-hospedar — usando a URL que expira",
			zap.Error(err))
		return original
	}

	nome := nomeDeArquivoDaImagem(original, produtoID, contentType)
	nova, err := s.storage.UploadFile(ctx, strings.NewReader(string(corpo)), nome, contentType, "products/"+storeID)
	if err != nil {
		log.Warn("não consegui guardar a imagem re-hospedada — usando a URL que expira",
			zap.Error(err))
		return original
	}

	campos := []zap.Field{zap.String("url_nova", nova)}
	if quando, ok := ExpiraEm(original); ok {
		campos = append(campos, zap.Time("a_original_expiraria_em", quando))
	}
	log.Info("imagem do produto re-hospedada (a URL do ERP expira)", campos...)
	return nova
}

// ReHospedarImagens aplica a re-hospedagem a uma lista, preservando a ordem.
//
// A ordem importa: a primeira é a principal do produto, e trocá-la mudaria a
// foto que a compradora vê sem ninguém ter pedido.
func (s *Service) ReHospedarImagens(ctx context.Context, storeID, produtoID string, originais []string) []string {
	if len(originais) == 0 {
		return originais
	}
	out := make([]string, 0, len(originais))
	for _, u := range originais {
		out = append(out, s.ReHospedarImagem(ctx, storeID, produtoID, u))
	}
	return out
}

func baixarImagem(ctx context.Context, endereco string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endereco, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Uma URL assinada que já venceu responde 403. É o caso mais provável de
		// falha aqui, e a mensagem tem de dizer isso — senão vira "erro ao
		// baixar" e ninguém liga com a expiração.
		if resp.StatusCode == http.StatusForbidden {
			return nil, "", fmt.Errorf("a URL assinada foi recusada (HTTP 403) — provavelmente já expirou")
		}
		return nil, "", fmt.Errorf("download devolveu HTTP %d", resp.StatusCode)
	}

	corpo, err := io.ReadAll(io.LimitReader(resp.Body, tamanhoMaximoDeImagem+1))
	if err != nil {
		return nil, "", err
	}
	if len(corpo) > tamanhoMaximoDeImagem {
		return nil, "", fmt.Errorf("imagem maior que o teto de %d MiB", tamanhoMaximoDeImagem>>20)
	}
	if len(corpo) == 0 {
		return nil, "", fmt.Errorf("o ERP devolveu uma imagem vazia")
	}

	tipo := resp.Header.Get("Content-Type")
	if tipo == "" || !strings.HasPrefix(tipo, "image/") {
		// Sem Content-Type confiável, o padrão é jpeg — que é o que os ERPs
		// devolvem na prática. Recusar por causa do header seria perder a foto
		// por um detalhe que o navegador ignoraria.
		tipo = "image/jpeg"
	}
	return corpo, tipo, nil
}

// nomeDeArquivoDaImagem monta um nome estável a partir do caminho da URL.
//
// A query é descartada de propósito: ela carrega a ASSINATURA, que muda a cada
// leitura. Um nome derivado dela faria a mesma foto virar um arquivo novo a
// cada import.
func nomeDeArquivoDaImagem(bruta, produtoID, contentType string) string {
	base := produtoID
	if u, err := url.Parse(bruta); err == nil {
		if p := path.Base(u.Path); p != "" && p != "." && p != "/" {
			base = produtoID + "-" + p
		}
	}
	if path.Ext(base) == "" {
		base += extensaoDe(contentType)
	}
	return base
}

func extensaoDe(contentType string) string {
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	case strings.Contains(contentType, "gif"):
		return ".gif"
	default:
		return ".jpg"
	}
}

// reHospedarImagensDoProduto troca as URLs efêmeras do produto e das suas
// variações por URLs nossas, no lugar.
//
// Percorre as variações também: no Bling cada variação tem foto própria, e
// consertar só o pai deixaria a grade inteira quebrando em uma semana — o caso
// mais confuso de todos, porque o produto continua com foto e só as opções não.
func (s *Service) reHospedarImagensDoProduto(ctx context.Context, storeID string, p *providers.ERPProduct) {
	if p == nil {
		return
	}
	p.ImageURL = s.ReHospedarImagem(ctx, storeID, p.ID, p.ImageURL)
	p.ImageURLs = s.ReHospedarImagens(ctx, storeID, p.ID, p.ImageURLs)

	for i := range p.Variants {
		v := &p.Variants[i]
		v.ImageURL = s.ReHospedarImagem(ctx, storeID, v.ID, v.ImageURL)
		v.ImageURLs = s.ReHospedarImagens(ctx, storeID, v.ID, v.ImageURLs)
	}
}
