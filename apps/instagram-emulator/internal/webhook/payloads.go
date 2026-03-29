package webhook

import (
	"time"
)

// PayloadBuilder helps build webhook payloads
type PayloadBuilder struct {
	accountID string
}

// NewPayloadBuilder creates a new payload builder
func NewPayloadBuilder(accountID string) *PayloadBuilder {
	return &PayloadBuilder{
		accountID: accountID,
	}
}

// BuildLiveComment builds a live_comments webhook payload
func (b *PayloadBuilder) BuildLiveComment(userID, username, commentID, text, mediaID string) *WebhookPayload {
	return &WebhookPayload{
		Object: "instagram",
		Entry: []Entry{
			{
				ID:   b.accountID,
				Time: time.Now().Unix(),
				Changes: []Change{
					{
						Field: "live_comments",
						Value: LiveCommentValue{
							From: User{
								ID:       userID,
								Username: username,
							},
							CommentID: commentID,
							Text:      text,
							Media: Media{
								ID:               mediaID,
								MediaProductType: "LIVE",
							},
						},
					},
				},
			},
		},
	}
}

// BuildMessage builds a messages webhook payload (DM)
func (b *PayloadBuilder) BuildMessage(senderID, messageID, text string) *WebhookPayload {
	return &WebhookPayload{
		Object: "instagram",
		Entry: []Entry{
			{
				ID:   b.accountID,
				Time: time.Now().Unix(),
				Messaging: []Message{
					{
						Sender: IDOnly{
							ID: senderID,
						},
						Recipient: IDOnly{
							ID: b.accountID,
						},
						Timestamp: time.Now().Unix(),
						Message: MessageContent{
							MID:  messageID,
							Text: text,
						},
					},
				},
			},
		},
	}
}
