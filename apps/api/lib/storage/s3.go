package storage

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	appconfig "livecart/apps/api/lib/config"
)

// S3Client wraps the AWS S3 client
type S3Client struct {
	client     *s3.Client
	bucket     string
	region     string
	endpoint   string
	cdnBaseURL string
}

// NewS3Client creates a new S3 client
func NewS3Client() (*S3Client, error) {
	region := appconfig.AWSRegion.StringOr("auto")
	bucket := appconfig.S3Bucket.StringOr("livecart-uploads")
	endpoint := appconfig.S3Endpoint.String()
	cdnBaseURL := appconfig.CDNBaseURL.String()

	accessKeyID := appconfig.AWSAccessKeyID.String()
	secretAccessKey := appconfig.AWSSecretAccessKey.String()

	var cfg aws.Config
	var err error

	if accessKeyID != "" && secretAccessKey != "" {
		cfg, err = config.LoadDefaultConfig(context.Background(),
			config.WithRegion(region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				accessKeyID,
				secretAccessKey,
				"",
			)),
		)
	} else {
		cfg, err = config.LoadDefaultConfig(context.Background(),
			config.WithRegion(region),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client with custom endpoint if provided
	var client *s3.Client
	if endpoint != "" {
		client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true // Required for most S3-compatible services
		})
	} else {
		client = s3.NewFromConfig(cfg)
	}

	return &S3Client{
		client:     client,
		bucket:     bucket,
		region:     region,
		endpoint:   endpoint,
		cdnBaseURL: cdnBaseURL,
	}, nil
}

// UploadFile uploads a file to S3 and returns the S3 key (not a URL)
func (c *S3Client) UploadFile(ctx context.Context, reader io.Reader, filename string, contentType string, folder string) (string, error) {
	ext := filepath.Ext(filename)
	if ext == "" {
		ext = ".jpg"
	}

	key := fmt.Sprintf("%s/%s%s", folder, uuid.New().String(), ext)

	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        reader,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload file: %w", err)
	}

	// Return just the key, not a URL
	return key, nil
}

// GeneratePresignedGetURL generates a presigned URL for reading a file
// The URL expires after the specified duration (default 24 hours)
func (c *S3Client) GeneratePresignedGetURL(ctx context.Context, key string, expiration time.Duration) (string, error) {
	if key == "" {
		return "", nil
	}

	// If it looks like a full URL (legacy data), extract the key
	if strings.HasPrefix(key, "http") {
		key = c.extractKeyFromURL(key)
		if key == "" {
			return "", nil
		}
	}

	if expiration == 0 {
		expiration = 24 * time.Hour
	}

	presignClient := s3.NewPresignClient(c.client)
	presignedReq, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiration))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	return presignedReq.URL, nil
}

// GetPublicURL returns the public URL for a file
func (c *S3Client) GetPublicURL(key string) string {
	if c.cdnBaseURL != "" {
		return fmt.Sprintf("%s/%s", strings.TrimSuffix(c.cdnBaseURL, "/"), key)
	}
	// For custom S3-compatible endpoints (Tigris, R2, etc.)
	if c.endpoint != "" {
		return fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(c.endpoint, "/"), c.bucket, key)
	}
	// Standard AWS S3
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", c.bucket, c.region, key)
}

// DeleteFile deletes a file from S3
func (c *S3Client) DeleteFile(ctx context.Context, url string) error {
	key := c.extractKeyFromURL(url)
	if key == "" {
		return nil
	}

	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

// GeneratePresignedURL generates a presigned URL for uploading
func (c *S3Client) GeneratePresignedURL(ctx context.Context, filename string, contentType string, folder string) (string, string, error) {
	ext := filepath.Ext(filename)
	if ext == "" {
		ext = ".jpg"
	}

	key := fmt.Sprintf("%s/%s%s", folder, uuid.New().String(), ext)

	presignClient := s3.NewPresignClient(c.client)
	presignedReq, err := presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		return "", "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	publicURL := c.GetPublicURL(key)
	return presignedReq.URL, publicURL, nil
}

func (c *S3Client) extractKeyFromURL(url string) string {
	if c.cdnBaseURL != "" && strings.HasPrefix(url, c.cdnBaseURL) {
		return strings.TrimPrefix(url, c.cdnBaseURL+"/")
	}

	// Custom S3-compatible endpoint
	if c.endpoint != "" {
		prefix := fmt.Sprintf("%s/%s/", strings.TrimSuffix(c.endpoint, "/"), c.bucket)
		if strings.HasPrefix(url, prefix) {
			return strings.TrimPrefix(url, prefix)
		}
	}

	// Standard AWS S3
	s3Prefix := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/", c.bucket, c.region)
	if strings.HasPrefix(url, s3Prefix) {
		return strings.TrimPrefix(url, s3Prefix)
	}

	return ""
}
