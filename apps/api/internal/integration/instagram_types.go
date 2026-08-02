package integration

import "livecart/apps/api/internal/live"

// InstagramWebhookPayload represents the root payload sent by Meta webhooks
type InstagramWebhookPayload struct {
	Object string           `json:"object"`
	Entry  []InstagramEntry `json:"entry"`
}

// InstagramEntry represents a single entry in the webhook payload
type InstagramEntry struct {
	ID        string             `json:"id"`
	Time      int64              `json:"time"`
	Changes   []InstagramChange  `json:"changes,omitempty"`
	Messaging []InstagramMessage `json:"messaging,omitempty"`
}

// InstagramChange represents a change event (used for live_comments)
type InstagramChange struct {
	Field string      `json:"field"`
	Value interface{} `json:"value"`
}

// InstagramLiveCommentValue represents the value for live_comments field
type InstagramLiveCommentValue struct {
	From      InstagramUser  `json:"from"`
	CommentID string         `json:"id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Text      string         `json:"text"`
	Media     InstagramMedia `json:"media"`
}

// InstagramUser represents an Instagram user
type InstagramUser struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	SelfIGScopedID string `json:"self_ig_scoped_id"`
}

// InstagramMedia represents media information
type InstagramMedia struct {
	ID               string `json:"id"`
	MediaProductType string `json:"media_product_type"`
}

// InstagramMessage represents a direct message in the webhook
type InstagramMessage struct {
	Sender    InstagramIDOnly         `json:"sender"`
	Recipient InstagramIDOnly         `json:"recipient"`
	Timestamp int64                   `json:"timestamp"`
	Message   InstagramMessageContent `json:"message"`
}

// InstagramIDOnly represents an object with just an ID
type InstagramIDOnly struct {
	ID string `json:"id"`
}

// InstagramMessageContent represents the content of a direct message
type InstagramMessageContent struct {
	MID     string                 `json:"mid"`
	Text    string                 `json:"text"`
	IsEcho  bool                   `json:"is_echo,omitempty"`
	ReplyTo *InstagramMessageReply `json:"reply_to,omitempty"`
}

// InstagramMessageReply carries the context a DM is replying to. For story
// replies it holds the story the buyer tapped reply on — that story id maps to
// our published story-commerce event.
type InstagramMessageReply struct {
	MID   string               `json:"mid,omitempty"`
	Story *InstagramReplyStory `json:"story,omitempty"`
}

// InstagramReplyStory is the story a DM is replying to.
type InstagramReplyStory struct {
	ID  string `json:"id"`
	URL string `json:"url,omitempty"`
}

// ProcessInstagramCommentInput represents input for processing a live comment
// ProcessInstagramCommentInput's canonical home moved to internal/live (Bloco
// B4b): the comment.received consumer now lives in live.Service and deserializes
// into it. This alias keeps the webhook edge (ProcessInstagramMessage /
// processStoryReply / pollPostCommentsOnce) and the wire contract unchanged.
type ProcessInstagramCommentInput = live.ProcessInstagramCommentInput

// ProcessInstagramMessageInput represents input for processing a DM
type ProcessInstagramMessageInput struct {
	AccountID string
	SenderID  string
	MessageID string
	Text      string
	Timestamp int64
	// ReplyToStoryID is set when the DM is a reply to a story (reply_to.story.id),
	// mapping it to a published story-commerce event.
	ReplyToStoryID string
	// IsEcho is true for echoes of our own outbound messages — skip those.
	IsEcho     bool
	RawPayload []byte // Original webhook payload for audit storage
}
