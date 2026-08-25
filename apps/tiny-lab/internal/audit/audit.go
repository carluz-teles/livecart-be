// Package audit grava toda interação com o ERP num JSONL append-only. É a
// matéria-prima do RECON.md: cada afirmação sobre o comportamento da API tem
// que poder ser rastreada até um request/response real gravado aqui.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry é uma linha do log. Campos vazios são omitidos para o arquivo ficar
// legível a olho nu com `jq`.
type Entry struct {
	At           time.Time         `json:"at"`
	Kind         string            `json:"kind"` // "api" | "oauth" | "webhook" | "guard"
	Method       string            `json:"method,omitempty"`
	URL          string            `json:"url,omitempty"`
	Status       int               `json:"status,omitempty"`
	DurationMS   int64             `json:"duration_ms,omitempty"`
	RequestBody  json.RawMessage   `json:"request_body,omitempty"`
	ResponseBody json.RawMessage   `json:"response_body,omitempty"`
	ResponseRaw  string            `json:"response_raw,omitempty"` // quando não é JSON
	Headers      map[string]string `json:"headers,omitempty"`      // já filtrados
	Account      string            `json:"account,omitempty"`      // cpfCnpj da conta usada
	Note         string            `json:"note,omitempty"`
	Error        string            `json:"error,omitempty"`
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
// perder a chamada em si é pior. O erro é devolvido para quem quiser reportar.
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
