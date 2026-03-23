package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type Client struct {
	sqs      *sqs.Client
	queueURL string
}

func NewClient(ctx context.Context, queueURL string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}

	return &Client{
		sqs:      sqs.NewFromConfig(cfg),
		queueURL: queueURL,
	}, nil
}

func (c *Client) Publish(ctx context.Context, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}

	_, err = c.sqs.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(c.queueURL),
		MessageBody: aws.String(string(body)),
	})
	if err != nil {
		return fmt.Errorf("sending sqs message: %w", err)
	}

	return nil
}

type Message struct {
	Body          string
	ReceiptHandle string
}

func (c *Client) Receive(ctx context.Context, maxMessages int32) ([]Message, error) {
	out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(c.queueURL),
		MaxNumberOfMessages: maxMessages,
		WaitTimeSeconds:     20,
	})
	if err != nil {
		return nil, fmt.Errorf("receiving sqs messages: %w", err)
	}

	msgs := make([]Message, len(out.Messages))
	for i, m := range out.Messages {
		msgs[i] = Message{
			Body:          *m.Body,
			ReceiptHandle: *m.ReceiptHandle,
		}
	}
	return msgs, nil
}

func (c *Client) Delete(ctx context.Context, receiptHandle string) error {
	_, err := c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("deleting sqs message: %w", err)
	}
	return nil
}
