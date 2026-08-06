package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// projectRow mirrors the projects registry schema written by
// scripts/register (partition key `project_id`).
type projectRow struct {
	ProjectID string `dynamodbav:"project_id"`
	PublicKey string `dynamodbav:"public_key"`
	SecretKey string `dynamodbav:"secret_key,omitempty"`
	CreatedAt string `dynamodbav:"created_at"`
	Status    string `dynamodbav:"status"`
}

// --- keyStore: DynamoDB projects registry ---

type dynamoKeyStore struct {
	db    *dynamodb.Client
	table string
}

func (s *dynamoKeyStore) ValidateKey(ctx context.Context, projectID, publicKey string) (bool, error) {
	if projectID == "" || publicKey == "" {
		return false, nil
	}
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"project_id": &types.AttributeValueMemberS{Value: projectID},
		},
	})
	if err != nil {
		return false, fmt.Errorf("get project: %w", err)
	}
	if len(out.Item) == 0 {
		return false, nil
	}
	var row projectRow
	if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
		return false, fmt.Errorf("unmarshal project: %w", err)
	}
	return row.PublicKey == publicKey && row.Status == "active", nil
}

// --- archiveStore: S3 raw archive ---

type s3ArchiveStore struct {
	client *s3.Client
	bucket string
}

func (s *s3ArchiveStore) PutObject(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytesReader(data),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// --- indexStore: DynamoDB events index ---

type dynamoIndexStore struct {
	db    *dynamodb.Client
	table string
}

func (s *dynamoIndexStore) PutEvent(ctx context.Context, row eventRow) error {
	item, err := attributevalue.MarshalMap(row)
	if err != nil {
		return fmt.Errorf("marshal event row: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("ddb put event: %w", err)
	}
	return nil
}

// newDefaultDeps builds the AWS-backed dependency set from Lambda environment
// variables. RATE_LIMIT_RPS / RATE_LIMIT_BURST tune the per-key limiter.
func newDefaultDeps(ctx context.Context) (*deps, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	eventsTable := os.Getenv("EVENTS_TABLE")
	projectsTable := os.Getenv("PROJECTS_TABLE")
	rawBucket := os.Getenv("RAW_BUCKET")
	if eventsTable == "" || projectsTable == "" || rawBucket == "" {
		return nil, errors.New("EVENTS_TABLE, PROJECTS_TABLE and RAW_BUCKET must be set")
	}

	rate := envFloat("RATE_LIMIT_RPS", 5)
	burst := envFloat("RATE_LIMIT_BURST", 10)
	maxBody := envInt("MAX_BODY_BYTES", 1<<20) // default 1 MiB
	ttlDays := envInt("EVENT_TTL_DAYS", 90)

	return &deps{
		keys: &dynamoKeyStore{
			db:    dynamodb.NewFromConfig(cfg),
			table: projectsTable,
		},
		arch: &s3ArchiveStore{
			client: s3.NewFromConfig(cfg),
			bucket: rawBucket,
		},
		index: &dynamoIndexStore{
			db:    dynamodb.NewFromConfig(cfg),
			table: eventsTable,
		},
		limiter:      newRateLimiter(rate, burst),
		now:          nowFunc(),
		maxBodyBytes: maxBody,
		eventTTLDays: ttlDays,
	}, nil
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("invalid env value, using default", "name", name, "value", v, "default", def)
		return def
	}
	return n
}

func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil || f <= 0 {
		slog.Warn("invalid env value, using default", "name", name, "value", v, "default", def)
		return def
	}
	return f
}
