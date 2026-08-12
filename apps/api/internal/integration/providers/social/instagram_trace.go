package social

// =============================================================================
// [IGTRACE] — instrumentação TEMPORÁRIA do fluxo de live do Instagram
//
// Existe para caçar UMA pergunta: por que a Meta para de entregar
// live_comments no meio da transmissão. Respondemos 200 em 100% das entregas,
// o Cloudflare registra a última requisição no mesmo minuto que o nosso log, e
// reinscrever a conta não traz a entrega de volta. Sem enxergar CADA chamada
// que fazemos à Graph, o que sobra é adivinhação.
//
// TODO REMOVER quando a causa for identificada. Todo log desta investigação
// carrega o prefixo "[IGTRACE]" na mensagem — `grep -rn "\[IGTRACE\]"` acha
// todos, aqui e no restante do fluxo.
// =============================================================================

import (
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// TracePrefix é o marcador de toda linha desta investigação.
const TracePrefix = "[IGTRACE] "

// traceTransport registra TODA requisição HTTP feita ao Instagram: método,
// caminho, status e duração.
//
// Um RoundTripper e não 16 chamadas instrumentadas à mão: a captura tem de
// valer para qualquer chamada, inclusive as que alguém acrescentar depois — e
// uma chamada esquecida é exatamente o tipo de ponto cego que já nos custou
// tempo aqui.
type traceTransport struct {
	base   http.RoundTripper
	logger *zap.Logger
}

func (t *traceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := t.base.RoundTrip(req)
	took := time.Since(started)

	// O caminho SEM query string: o access_token viaja na query em boa parte
	// das chamadas da Graph, e log é lugar por onde credencial vaza.
	path := req.URL.Path
	fields := []zap.Field{
		zap.String("method", req.Method),
		zap.String("path", path),
		zap.Duration("took", took),
		zap.Strings("query_keys", queryKeys(req)),
	}

	if err != nil {
		t.logger.Warn(TracePrefix+"graph call failed", append(fields, zap.Error(err))...)
		return resp, err
	}

	fields = append(fields, zap.Int("status", resp.StatusCode))
	if resp.StatusCode >= 400 {
		t.logger.Warn(TracePrefix+"graph call returned error status", fields...)
	} else {
		t.logger.Info(TracePrefix+"graph call", fields...)
	}
	return resp, nil
}

// queryKeys devolve os NOMES dos parâmetros, nunca os valores — o suficiente
// para saber que campos pedimos (`subscribed_fields`, `fields`) sem imprimir
// token.
func queryKeys(req *http.Request) []string {
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		if strings.Contains(strings.ToLower(k), "token") {
			continue
		}
		keys = append(keys, k)
	}
	return keys
}

// tracedClient embrulha o client do provider com o RoundTripper acima.
func tracedClient(logger *zap.Logger) *http.Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &http.Client{
		// 8s, e não 30s. Nenhuma chamada nossa à Graph tem motivo para durar
		// mais: a publicação de mídia, que é a única lenta, tem polling próprio
		// com deadline separado (waitContainerFinished). Com 30s, dois POSTs
		// empilhados (a tentativa com HUMAN_AGENT sempre falha antes do
		// fallback) somavam mais que os 5s que a Meta exige para o 200.
		Timeout:   8 * time.Second,
		Transport: &traceTransport{base: http.DefaultTransport, logger: logger},
	}
}
