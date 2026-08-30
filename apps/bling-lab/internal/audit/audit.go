// Package audit grava toda interação com o Bling num JSONL append-only. É a
// matéria-prima da rodada de medição: cada afirmação sobre o comportamento da
// API tem que poder ser rastreada até um request/response real gravado aqui.
//
// Diferença deliberada em relação ao tiny-lab: aqui os headers de RESPOSTA são
// gravados INTEIROS (menos os de sessão). O Bling não documenta header de cota
// nenhum, e provar essa ausência contra a conta real do lojista é um dos
// resultados que o plano depende — uma ausência só é dado se a coleta for
// completa.
package audit

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	At           time.Time         `json:"at"`
	Kind         string            `json:"kind"` // "api" | "oauth" | "webhook" | "guard"
	Method       string            `json:"method,omitempty"`
	URL          string            `json:"url,omitempty"`
	Status       int               `json:"status,omitempty"`
	DurationMS   int64             `json:"duration_ms,omitempty"`
	RequestBody  json.RawMessage   `json:"request_body,omitempty"`
	ResponseBody json.RawMessage   `json:"response_body,omitempty"`
	ResponseRaw  string            `json:"response_raw,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Account      string            `json:"account,omitempty"`
	Note         string            `json:"note,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// sensiveis são os headers que NUNCA vão para o log. Set-Cookie carrega o
// PHPSESSID que o token endpoint devolve; Authorization carrega o Basic.
var sensiveis = map[string]bool{
	"set-cookie":    true,
	"authorization": true,
	"cookie":        true,
}

// Headers achata a resposta preservando tudo que não é segredo. Chave em minúsculo
// para o `jq` ficar previsível.
func Headers(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if sensiveis[lk] {
			out[lk] = "<omitido>"
			continue
		}
		out[lk] = strings.Join(v, ", ")
	}
	return out
}

type Log struct {
	mu   sync.Mutex
	path string
}

func New(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return &Log{path: path}, nil
}

// Append nunca derruba o comando que o chamou: falhar em auditar é ruim, mas
// perder a chamada em si é pior.
func (l *Log) Append(e Entry) error {
	if l == nil {
		return nil
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (l *Log) Path() string { return l.path }
