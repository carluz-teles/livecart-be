package webhook

// WebhookPayload represents the root payload sent by Meta webhooks
type WebhookPayload struct {
	Object string  `json:"object"`
	Entry  []Entry `json:"entry"`
}

// Entry represents a single entry in the webhook payload
type Entry struct {
	ID        string    `json:"id"`
	Time      int64     `json:"time"`
	Changes   []Change  `json:"changes,omitempty"`
	Messaging []Message `json:"messaging,omitempty"`
}

// Change represents a change event (used for comments, live_comments, etc.)
type Change struct {
	Field string      `json:"field"`
	Value interface{} `json:"value"`
}

// LiveCommentValue represents the value for live_comments field
type LiveCommentValue struct {
	From      User   `json:"from"`
	CommentID string `json:"comment_id"`
	ParentID  string `json:"parent_id,omitempty"`
	Text      string `json:"text"`
	Media     Media  `json:"media"`
}

// User represents an Instagram user
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Media represents media information in a comment
type Media struct {
	ID               string `json:"id"`
	MediaProductType string `json:"media_product_type"`
}

// Message represents a direct message in the webhook
type Message struct {
	Sender    IDOnly         `json:"sender"`
	Recipient IDOnly         `json:"recipient"`
	Timestamp int64          `json:"timestamp"`
	Message   MessageContent `json:"message"`
}

// IDOnly represents an object with just an ID
type IDOnly struct {
	ID string `json:"id"`
}

// MessageContent represents the content of a direct message
type MessageContent struct {
	MID  string `json:"mid"`
	Text string `json:"text"`
}

// LiveMediaResponse represents the response for GET /live_media endpoint
type LiveMediaResponse struct {
	Data   []LiveMedia `json:"data"`
	Paging Paging      `json:"paging"`
}

// LiveMedia represents a live media object
type LiveMedia struct {
	ID               string        `json:"id"`
	MediaType        string        `json:"media_type"`
	MediaProductType string        `json:"media_product_type"`
	Owner            Owner         `json:"owner"`
	Username         string        `json:"username"`
	Comments         CommentsData  `json:"comments,omitempty"`
}

// Owner represents the owner of the live media
type Owner struct {
	ID string `json:"id"`
}

// CommentsData represents comments data in live media
type CommentsData struct {
	Data []interface{} `json:"data"`
}

// Paging represents pagination info
type Paging struct {
	Cursors Cursors `json:"cursors,omitempty"`
}

// Cursors represents pagination cursors
type Cursors struct {
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}
