package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/livecart/instagram-emulator/internal/simulator"
	"github.com/livecart/instagram-emulator/internal/webhook"
)

// Commands handles all CLI commands
type Commands struct {
	session *simulator.Session
	sender  *webhook.Sender
	builder *webhook.PayloadBuilder
}

// NewCommands creates a new commands handler
func NewCommands(session *simulator.Session, sender *webhook.Sender, builder *webhook.PayloadBuilder) *Commands {
	return &Commands{
		session: session,
		sender:  sender,
		builder: builder,
	}
}

// Start starts a new live
func (c *Commands) Start() {
	if c.session.IsLiveActive() {
		color.Yellow("Live already active (media_id: %s)", c.session.GetMediaID())
		return
	}

	mediaID := c.session.StartLive()
	color.Green("Live started!")
	fmt.Printf("  media_id: %s\n", mediaID)
	fmt.Printf("  Backend can poll: GET /%s/live_media\n", c.session.GetAccountID())
}

// End ends the current live
func (c *Commands) End() {
	if !c.session.IsLiveActive() {
		color.Yellow("No active live to end")
		return
	}

	c.session.EndLive()
	color.Green("Live ended!")
	fmt.Println("  Backend will see empty data[] in GET /live_media")
}

// Comment sends a live_comments webhook
func (c *Commands) Comment(args []string) {
	if !c.session.IsLiveActive() {
		color.Yellow("No active live. Start one with 'start'")
		return
	}

	var user simulator.SimulatedUser
	var text string

	// Parse args: either "text" or "--user username text"
	if len(args) >= 3 && args[0] == "--user" {
		user = *c.session.GetGenerator().GetUserByUsername(args[1])
		text = strings.Join(args[2:], " ")
	} else if len(args) >= 1 {
		user = c.session.GetGenerator().RandomUser()
		text = strings.Join(args, " ")
	} else {
		color.Red("Usage: comment <text> or comment --user <username> <text>")
		return
	}

	commentID := c.session.GetGenerator().GenerateCommentID()
	payload := c.builder.BuildLiveComment(
		user.ID,
		user.Username,
		commentID,
		text,
		c.session.GetMediaID(),
	)

	if err := c.sender.Send(payload); err != nil {
		color.Red("Failed to send webhook: %v", err)
		return
	}

	color.Green("Webhook live_comments sent!")
	fmt.Printf("  @%s: \"%s\"\n", user.Username, text)
}

// DM sends a messages webhook (direct message)
func (c *Commands) DM(args []string) {
	if len(args) == 0 {
		color.Red("Usage: dm <text> or dm --user <username> <text>")
		return
	}

	var user simulator.SimulatedUser
	var text string

	// Parse args: either "text" or "--user username text"
	if len(args) >= 3 && args[0] == "--user" {
		user = *c.session.GetGenerator().GetUserByUsername(args[1])
		text = strings.Join(args[2:], " ")
	} else {
		user = c.session.GetGenerator().RandomUser()
		text = strings.Join(args, " ")
	}

	messageID := c.session.GetGenerator().GenerateMessageID()
	payload := c.builder.BuildMessage(user.ID, messageID, text)

	if err := c.sender.Send(payload); err != nil {
		color.Red("Failed to send webhook: %v", err)
		return
	}

	color.Green("Webhook messages sent!")
	fmt.Printf("  DM from @%s: \"%s\"\n", user.Username, text)
}

// Burst sends multiple random comments
func (c *Commands) Burst(args []string) {
	if !c.session.IsLiveActive() {
		color.Yellow("No active live. Start one with 'start'")
		return
	}

	count := 5 // default
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			count = n
		}
	}

	if count > 100 {
		color.Yellow("Limiting burst to 100 comments")
		count = 100
	}

	color.Cyan("Sending %d comments...", count)

	for i := 0; i < count; i++ {
		user := c.session.GetGenerator().RandomUser()
		text := c.session.GetGenerator().RandomComment()
		commentID := c.session.GetGenerator().GenerateCommentID()

		payload := c.builder.BuildLiveComment(
			user.ID,
			user.Username,
			commentID,
			text,
			c.session.GetMediaID(),
		)

		if err := c.sender.Send(payload); err != nil {
			color.Red("  [%d] Failed: %v", i+1, err)
			continue
		}

		fmt.Printf("  [%d] @%s: \"%s\"\n", i+1, user.Username, text)

		// Small delay between comments
		time.Sleep(100 * time.Millisecond)
	}

	color.Green("Burst complete! %d webhooks sent", count)
}

// Users lists all simulated users
func (c *Commands) Users() {
	users := c.session.GetGenerator().ListUsers()

	color.Cyan("Available simulated users:")
	for _, user := range users {
		fmt.Printf("  @%s (id: %s)\n", user.Username, user.ID)
	}
}

// Status shows current session status
func (c *Commands) Status() {
	color.Cyan("Session Status:")
	fmt.Printf("  Account: @%s (%s)\n", c.session.GetUsername(), c.session.GetAccountID())
	fmt.Printf("  Webhook URL: %s\n", c.sender.GetWebhookURL())

	if c.session.IsLiveActive() {
		color.Green("  Live: ACTIVE (media_id: %s)", c.session.GetMediaID())
	} else {
		color.Yellow("  Live: INACTIVE")
	}
}

// Help shows available commands
func (c *Commands) Help() {
	color.Cyan("Available Commands:")
	fmt.Println()

	commands := []struct {
		cmd  string
		desc string
	}{
		{"start", "Start a new live session"},
		{"end", "End the current live session"},
		{"comment <text>", "Send a live comment (random user)"},
		{"comment --user <username> <text>", "Send comment as specific user"},
		{"dm <text>", "Send a DM (random user)"},
		{"dm --user <username> <text>", "Send DM as specific user"},
		{"burst [count]", "Send multiple random comments (default: 5)"},
		{"users", "List available simulated users"},
		{"status", "Show current session status"},
		{"help", "Show this help"},
		{"exit", "Exit the emulator"},
	}

	for _, c := range commands {
		color.Green("  %-35s", c.cmd)
		fmt.Printf("%s\n", c.desc)
	}
	fmt.Println()
}
