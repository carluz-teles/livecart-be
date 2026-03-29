package simulator

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// SimulatedUser represents a fake Instagram user
type SimulatedUser struct {
	ID       string
	Username string
}

// Generator creates fake data for simulation
type Generator struct {
	users            []SimulatedUser
	commentTemplates []string
	rng              *rand.Rand
}

// NewGenerator creates a new data generator
func NewGenerator() *Generator {
	return &Generator{
		users: []SimulatedUser{
			{ID: "user_001", Username: "maria_silva"},
			{ID: "user_002", Username: "joao_santos"},
			{ID: "user_003", Username: "ana_costa"},
			{ID: "user_004", Username: "pedro_lima"},
			{ID: "user_005", Username: "carla_dias"},
			{ID: "user_006", Username: "lucas_oliveira"},
			{ID: "user_007", Username: "juliana_souza"},
			{ID: "user_008", Username: "rafael_pereira"},
			{ID: "user_009", Username: "fernanda_alves"},
			{ID: "user_010", Username: "bruno_rodrigues"},
		},
		commentTemplates: []string{
			"quero %d",
			"reserva %d pra mim",
			"%d unidades por favor",
			"manda %d",
			"separa %d",
			"quanto custa?",
			"ainda tem?",
			"qual o tamanho?",
			"tem outras cores?",
			"entrega pra onde?",
			"aceita pix?",
			"qual o prazo?",
		},
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// RandomUser returns a random simulated user
func (g *Generator) RandomUser() SimulatedUser {
	return g.users[g.rng.Intn(len(g.users))]
}

// GetUserByUsername finds a user by username (partial match)
func (g *Generator) GetUserByUsername(username string) *SimulatedUser {
	for _, user := range g.users {
		if user.Username == username {
			return &user
		}
	}
	// If not found, create a new user with that username
	return &SimulatedUser{
		ID:       fmt.Sprintf("user_%s", uuid.New().String()[:8]),
		Username: username,
	}
}

// RandomComment returns a random comment text
func (g *Generator) RandomComment() string {
	template := g.commentTemplates[g.rng.Intn(len(g.commentTemplates))]

	// If template has %d, replace with random quantity
	if containsFormatVerb(template) {
		quantity := g.rng.Intn(5) + 1 // 1-5
		return fmt.Sprintf(template, quantity)
	}

	return template
}

// GenerateCommentID generates a unique comment ID
func (g *Generator) GenerateCommentID() string {
	return fmt.Sprintf("comment_%s", uuid.New().String()[:12])
}

// GenerateMessageID generates a unique message ID
func (g *Generator) GenerateMessageID() string {
	return fmt.Sprintf("mid_%s", uuid.New().String()[:16])
}

// ListUsers returns all available simulated users
func (g *Generator) ListUsers() []SimulatedUser {
	return g.users
}

func containsFormatVerb(s string) bool {
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '%' && s[i+1] == 'd' {
			return true
		}
	}
	return false
}
