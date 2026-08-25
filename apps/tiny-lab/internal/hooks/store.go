// Package hooks é a ponte de webhooks: recebe o que o Tiny manda, grava o
// payload CRU (headers + body + querystring + horário, sem transformação) e
// sabe reenviá-lo quantas vezes for preciso. O replay é a base dos testes de
// idempotência.
package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Event é o registro fiel de uma entrega. Body fica como string para preservar
// byte a byte o que chegou — reserializar um JSON já mudaria a ordem das
// chaves e a formatação, e é justamente o formato real que queremos conhecer.
type Event struct {
	ID         string              `json:"id"`
	ReceivedAt time.Time           `json:"received_at"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      string              `json:"query,omitempty"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	RemoteAddr string              `json:"remote_addr,omitempty"`
}

// Envelope é o shape observado nos webhooks reais do Tiny (bateria 11/07):
// {"versao":"1.0.1","cnpj":"...","tipo":"estoque","dados":{...}}
type Envelope struct {
	Versao string          `json:"versao"`
	CNPJ   string          `json:"cnpj"`
	Tipo   string          `json:"tipo"`
	Dados  json.RawMessage `json:"dados"`
}

// Summary extrai uma linha legível para o terminal. Nunca falha: um payload
// inesperado vira resumo genérico, mas o arquivo cru continua no disco.
func (e *Event) Summary() string {
	var env Envelope
	if err := json.Unmarshal([]byte(e.Body), &env); err != nil || env.Tipo == "" {
		body := strings.TrimSpace(e.Body)
		if len(body) > 120 {
			body = body[:120] + "…"
		}
		return fmt.Sprintf("(fora do envelope conhecido) %s", body)
	}

	// UseNumber evita que ids grandes virem float64 e saiam em notação
	// científica (idProduto=3.57281337e+08 em vez de 357281337).
	var dados map[string]any
	dec := json.NewDecoder(bytes.NewReader(env.Dados))
	dec.UseNumber()
	_ = dec.Decode(&dados)

	// Cada tipo carrega o id em um campo diferente; tentamos os conhecidos.
	var parts []string
	for _, k := range []string{"id", "idProduto", "idPedido", "idNota", "idContato", "numero", "sku", "nome", "saldo", "codigoSituacao"} {
		if v, ok := dados[k]; ok && v != nil && fmt.Sprint(v) != "" {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return fmt.Sprintf("tipo=%s cnpj=%s %s", env.Tipo, env.CNPJ, strings.Join(parts, " "))
}

// Tipo devolve o campo tipo do envelope, ou "desconhecido".
func (e *Event) Tipo() string {
	var env Envelope
	if err := json.Unmarshal([]byte(e.Body), &env); err == nil && env.Tipo != "" {
		return env.Tipo
	}
	return "desconhecido"
}

type Store struct{ dir string }

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Save(e *Event) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, e.ID+".json"), append(b, '\n'), 0o600)
}

// List devolve os eventos em ordem cronológica — o ID começa com o timestamp,
// então a ordem lexicográfica do nome do arquivo já é a ordem de chegada.
func (s *Store) List() ([]*Event, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, en := range entries {
		if !en.IsDir() && strings.HasSuffix(en.Name(), ".json") {
			names = append(names, en.Name())
		}
	}
	sort.Strings(names)

	out := make([]*Event, 0, len(names))
	for _, n := range names {
		e, err := s.load(filepath.Join(s.dir, n))
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Get aceita o ID completo ou um sufixo/prefixo não ambíguo, para o replay ser
// confortável de digitar.
func (s *Store) Get(id string) (*Event, error) {
	if e, err := s.load(filepath.Join(s.dir, id+".json")); err == nil {
		return e, nil
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []*Event
	for _, e := range all {
		if strings.Contains(e.ID, id) {
			hits = append(hits, e)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("nenhum webhook gravado casa com %q", id)
	case 1:
		return hits[0], nil
	default:
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
		}
		return nil, fmt.Errorf("%q é ambíguo — casa com: %s", id, strings.Join(ids, ", "))
	}
}

func (s *Store) load(path string) (*Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Event
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &e, nil
}

func (s *Store) Dir() string { return s.dir }

// headersToHTTP reconstrói os headers originais no replay, menos os que o
// transporte precisa recalcular.
func headersToHTTP(src map[string][]string) http.Header {
	h := http.Header{}
	for k, vals := range src {
		switch strings.ToLower(k) {
		case "content-length", "host", "connection", "transfer-encoding", "accept-encoding":
			continue
		}
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	return h
}
