package hooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Envelope é a forma que a doc do Bling declara para todo callback.
// Guardamos o corpo CRU ao lado: a forma de `data` não está no openapi.json
// (webhooks não são endpoints), então qualquer struct nosso é palpite até um
// evento real chegar.
type Envelope struct {
	EventID   string          `json:"eventId"`
	Date      string          `json:"date"`
	Version   string          `json:"version"`
	Event     string          `json:"event"` // recurso.acao — ex. product.updated
	CompanyID string          `json:"companyId"`
	Data      json.RawMessage `json:"data"`
}

type Evento struct {
	ID         string      `json:"id"`
	ReceivedAt time.Time   `json:"received_at"`
	Method     string      `json:"method"`
	Path       string      `json:"path"`
	Query      string      `json:"query,omitempty"`
	Headers    http.Header `json:"headers"`
	Body       string      `json:"body"`
	RemoteAddr string      `json:"remote_addr,omitempty"`

	Assinatura Desfecho  `json:"assinatura"`
	Envelope   *Envelope `json:"envelope,omitempty"`
}

// Resumo é a linha que aparece no terminal quando o evento chega.
func (e *Evento) Resumo() string {
	if e.Envelope == nil {
		corpo := strings.TrimSpace(e.Body)
		if len(corpo) > 120 {
			corpo = corpo[:120] + "…"
		}
		return "(corpo não é o envelope esperado) " + corpo
	}
	return fmt.Sprintf("%s  empresa=%s  eventId=%s  data=%s",
		e.Envelope.Event, e.Envelope.CompanyID, e.Envelope.EventID, resumirData(e.Envelope.Data))
}

func resumirData(d json.RawMessage) string {
	s := strings.TrimSpace(string(d))
	if s == "" {
		return "<vazio>"
	}
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

type Store struct{ dir string }

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Salvar(e *Evento) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, e.ID+".json"), append(b, '\n'), 0o600)
}

func (s *Store) Listar() ([]*Evento, error) {
	entradas, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Evento
	for _, en := range entradas {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".json") {
			continue
		}
		e, err := s.Carregar(strings.TrimSuffix(en.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.Before(out[j].ReceivedAt) })
	return out, nil
}

func (s *Store) Carregar(id string) (*Evento, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var e Evento
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}
