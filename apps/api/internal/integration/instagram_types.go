package integration

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
	ID       string `json:"id"`
	Username string `json:"username"`
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
	MID  string `json:"mid"`
	Text string `json:"text"`
}

// ProcessInstagramCommentInput represents input for processing a live comment
type ProcessInstagramCommentInput struct {
	AccountID      string
	MediaID        string
	CommentID      string
	UserID         string
	Username       string
	Text           string
	Timestamp      int64
}

// ProcessInstagramMessageInput represents input for processing a DM
type ProcessInstagramMessageInput struct {
	AccountID string
	SenderID  string
	MessageID string
	Text      string
	Timestamp int64
}
