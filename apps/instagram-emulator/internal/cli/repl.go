package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/livecart/instagram-emulator/internal/config"
	"github.com/livecart/instagram-emulator/internal/simulator"
	"github.com/livecart/instagram-emulator/internal/webhook"
)

// REPL represents the interactive command-line interface
type REPL struct {
	config   *config.Config
	session  *simulator.Session
	sender   *webhook.Sender
	builder  *webhook.PayloadBuilder
	commands *Commands
	running  bool
}

// NewREPL creates a new REPL instance
func NewREPL(cfg *config.Config, session *simulator.Session, sender *webhook.Sender) *REPL {
	return &REPL{
		config:   cfg,
		session:  session,
		sender:   sender,
		builder:  webhook.NewPayloadBuilder(cfg.AccountID),
		commands: NewCommands(session, sender, webhook.NewPayloadBuilder(cfg.AccountID)),
		running:  true,
	}
}

// Run starts the REPL loop
func (r *REPL) Run() {
	r.printBanner()

	scanner := bufio.NewScanner(os.Stdin)

	for r.running {
		r.printPrompt()

		if !scanner.Scan() {
			break
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		r.processCommand(input)
	}
}

func (r *REPL) printBanner() {
	title := color.New(color.FgCyan, color.Bold)
	info := color.New(color.FgWhite)

	fmt.Println()
	title.Println("  Instagram Webhook Emulator")
	fmt.Println("  " + strings.Repeat("-", 40))
	info.Printf("  Server:  http://localhost:%d\n", r.config.Port)
	info.Printf("  Webhook: %s\n", r.config.WebhookURL)
	info.Printf("  Account: @%s (%s)\n", r.config.Username, r.config.AccountID)
	fmt.Println()
	color.Yellow("  Type 'help' for available commands\n")
	fmt.Println()
}

func (r *REPL) printPrompt() {
	prompt := color.New(color.FgGreen, color.Bold)
	if r.session.IsLiveActive() {
		prompt.Print("[LIVE] ")
	}
	prompt.Print("> ")
}

func (r *REPL) processCommand(input string) {
	parts := parseInput(input)
	if len(parts) == 0 {
		return
	}

	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	switch cmd {
	case "start":
		r.commands.Start()
	case "end":
		r.commands.End()
	case "comment", "c":
		r.commands.Comment(args)
	case "dm", "message":
		r.commands.DM(args)
	case "burst":
		r.commands.Burst(args)
	case "users":
		r.commands.Users()
	case "status":
		r.commands.Status()
	case "help", "h", "?":
		r.commands.Help()
	case "exit", "quit", "q":
		r.running = false
		color.Yellow("Bye!")
	default:
		color.Red("Unknown command: %s", cmd)
		color.Yellow("Type 'help' for available commands")
	}
}

// parseInput splits input respecting quoted strings
func parseInput(input string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false

	for _, char := range input {
		switch char {
		case '"':
			inQuotes = !inQuotes
		case ' ':
			if inQuotes {
				current.WriteRune(char)
			} else if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(char)
		}
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}
