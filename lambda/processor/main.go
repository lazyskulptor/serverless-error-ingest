// Package main implements the post-ingest grouping/summarization processor.
//
// It runs on a schedule (EventBridge), scans DynamoDB for `event` items with
// status "received", loads each raw event payload from S3, computes a
// deterministic issue_id from a normalized exception fingerprint, optionally
// sends only the extracted summary (never the raw payload) to a configured AI
// model for a short human summary, and updates the metadata row
// (status -> "grouped").
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// eventRow mirrors the events table metadata written by the ingest handler.
type eventRow struct {
	ProjectID string `dynamodbav:"project_id"`
	EventID   string `dynamodbav:"event_id"`
	Timestamp string `dynamodbav:"timestamp"`
	Level     string `dynamodbav:"level,omitempty"`
	Platform  string `dynamodbav:"platform,omitempty"`
	ItemType  string `dynamodbav:"item_type"`
	Count     int    `dynamodbav:"count"`
	Status    string `dynamodbav:"status"`
	S3Path    string `dynamodbav:"s3_path"`
	IssueID   string `dynamodbav:"issue_id,omitempty"`
	Summary   string `dynamodbav:"summary,omitempty"`
}

// stackFrame is one Sentry stacktrace frame.
type stackFrame struct {
	File     string `json:"filename"`
	Function string `json:"function"`
	Lineno   int    `json:"lineno"`
}

// exceptionValue is one Sentry exception value with its stacktrace.
type exceptionValue struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Stacktrace struct {
		Frames []stackFrame `json:"frames"`
	} `json:"stacktrace"`
}

// sentryEvent is the subset of the Sentry event schema the processor reads.
type sentryEvent struct {
	Platform  string `json:"platform"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Exception struct {
		Values []exceptionValue `json:"values"`
	} `json:"exception"`
}

// deps bundles the processor dependencies (injected for tests).
type deps struct {
	fetch  func(ctx context.Context, s3Path string) ([]byte, error)
	scan   func(ctx context.Context) ([]eventRow, error)
	update func(ctx context.Context, row eventRow) error
	now    func() time.Time
	ai     *aiClient
}

// aiClient sends extracted summaries (never raw payloads) to an
// OpenAI-compatible chat endpoint.
type aiClient struct {
	endpoint string
	key      string
	model    string
	client   *http.Client
}

func (c *aiClient) summarize(ctx context.Context, prompt string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": "You summarize software exceptions concisely for a developer."},
			{"role": "user", "content": prompt},
		},
		"max_tokens": 60,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AI endpoint status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("AI endpoint returned no choices")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// handleRun processes one scheduled batch.
func handleRun(ctx context.Context, d *deps) error {
	rows, err := d.scan(ctx)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	grouped := 0
	for _, row := range rows {
		raw, err := d.fetch(ctx, row.S3Path)
		if err != nil {
			log.Printf("processor fetch failed project=%s event=%s: %v", row.ProjectID, row.EventID, err)
			continue
		}
		ev, err := parseSentryEvent(raw)
		if err != nil {
			log.Printf("processor parse failed project=%s event=%s: %v", row.ProjectID, row.EventID, err)
			continue
		}

		row.IssueID = computeIssueID(ev)
		row.Summary = summarize(ev)
		if d.ai != nil {
			if summary, err := d.ai.summarize(ctx, aiPrompt(ev)); err == nil {
				row.Summary = summary
			} else {
				log.Printf("processor ai summary failed project=%s event=%s: %v", row.ProjectID, row.EventID, err)
			}
		}
		row.Status = "grouped"

		if err := d.update(ctx, row); err != nil {
			log.Printf("processor update failed project=%s event=%s: %v", row.ProjectID, row.EventID, err)
			continue
		}
		grouped++
	}

	log.Printf("processor batch done: scanned=%d grouped=%d", len(rows), grouped)
	return nil
}

// parseSentryEvent decodes the raw Sentry event payload.
func parseSentryEvent(raw []byte) (sentryEvent, error) {
	var ev sentryEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ev, err
	}
	return ev, nil
}

// computeIssueID produces a deterministic grouping key from the normalized
// exception fingerprint: platform + first exception type/value + top
// stacktrace frame location. Identical crashes map to the same issue_id.
func computeIssueID(ev sentryEvent) string {
	h := sha256.New()
	// hash.Hash.Write never returns an error; ignore for lint clarity.
	_, _ = fmt.Fprintf(h, "%s|", normalize(ev.Platform))
	if len(ev.Exception.Values) > 0 {
		v := ev.Exception.Values[0]
		_, _ = fmt.Fprintf(h, "%s|%s|", normalize(v.Type), normalize(v.Value))
		if len(v.Stacktrace.Frames) > 0 {
			f := v.Stacktrace.Frames[0]
			_, _ = fmt.Fprintf(h, "%s|%s|%d", normalize(f.File), normalize(f.Function), f.Lineno)
		}
	} else {
		_, _ = fmt.Fprintf(h, "no-exception|%s", normalize(ev.Message))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// summarize produces a short human summary without AI: "Type: value" or the
// event message when no exception is present.
func summarize(ev sentryEvent) string {
	if len(ev.Exception.Values) > 0 {
		v := ev.Exception.Values[0]
		value := strings.TrimSpace(v.Value)
		if value == "" {
			return v.Type
		}
		return v.Type + ": " + value
	}
	msg := strings.TrimSpace(ev.Message)
	if msg != "" {
		return msg
	}
	return "unknown error"
}

// aiPrompt builds the prompt sent to the AI model. It contains ONLY the
// extracted fingerprint components — never the raw event payload.
func aiPrompt(ev sentryEvent) string {
	var b strings.Builder
	b.WriteString("Summarize this exception for a developer:\n")
	if len(ev.Exception.Values) > 0 {
		v := ev.Exception.Values[0]
		fmt.Fprintf(&b, "type: %s\nvalue: %s\n", v.Type, v.Value)
		for i, f := range v.Stacktrace.Frames {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "frame: %s:%d %s\n", f.File, f.Lineno, f.Function)
		}
	} else if ev.Message != "" {
		fmt.Fprintf(&b, "message: %s\n", ev.Message)
	}
	return b.String()
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// --- AWS-backed implementations ---

type processorDeps struct {
	ddb         *dynamodb.Client
	s3c         *s3.Client
	eventsTable string
	rawBucket   string
}

func (p *processorDeps) scan(ctx context.Context) ([]eventRow, error) {
	var rows []eventRow
	var startKey map[string]types.AttributeValue
	for {
		out, err := p.ddb.Scan(ctx, &dynamodb.ScanInput{
			TableName:        aws.String(p.eventsTable),
			FilterExpression: aws.String("item_type = :it AND status = :st"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":it": &types.AttributeValueMemberS{Value: "event"},
				":st": &types.AttributeValueMemberS{Value: "received"},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range out.Items {
			var row eventRow
			if err := attributevalue.UnmarshalMap(item, &row); err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return rows, nil
}

func (p *processorDeps) fetch(ctx context.Context, s3Path string) ([]byte, error) {
	out, err := p.s3c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.rawBucket),
		Key:    aws.String(s3Path),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (p *processorDeps) update(ctx context.Context, row eventRow) error {
	updates := map[string]types.AttributeValue{
		":status":  &types.AttributeValueMemberS{Value: row.Status},
		":issue":   &types.AttributeValueMemberS{Value: row.IssueID},
		":summary": &types.AttributeValueMemberS{Value: row.Summary},
	}
	_, err := p.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(p.eventsTable),
		Key: map[string]types.AttributeValue{
			"project_id": &types.AttributeValueMemberS{Value: row.ProjectID},
			"event_id":   &types.AttributeValueMemberS{Value: row.EventID},
		},
		UpdateExpression:          aws.String("SET #st = :status, issue_id = :issue, summary = :summary"),
		ConditionExpression:       aws.String("attribute_not_exists(issue_id)"),
		ExpressionAttributeNames:  map[string]string{"#st": "status"},
		ExpressionAttributeValues: updates,
	})
	return err
}

func newDefaultDeps(ctx context.Context) (*deps, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	eventsTable := os.Getenv("EVENTS_TABLE")
	rawBucket := os.Getenv("RAW_BUCKET")
	if eventsTable == "" || rawBucket == "" {
		return nil, fmt.Errorf("EVENTS_TABLE and RAW_BUCKET must be set")
	}
	d := &deps{
		now: time.Now,
	}
	impl := &processorDeps{
		ddb:         dynamodb.NewFromConfig(cfg),
		s3c:         s3.NewFromConfig(cfg),
		eventsTable: eventsTable,
		rawBucket:   rawBucket,
	}
	d.scan = impl.scan
	d.fetch = impl.fetch
	d.update = impl.update

	// AI is opt-in only. The key is never in source control (SSM/env). The
	// raw payload is never sent — only the extracted fingerprint summary —
	// and only when explicitly allowed.
	key := os.Getenv("AI_API_KEY")
	endpoint := os.Getenv("AI_ENDPOINT")
	if key != "" && endpoint != "" && os.Getenv("PROJECT_ALLOW_AI") == "true" {
		d.ai = &aiClient{
			endpoint: endpoint,
			key:      key,
			model:    os.Getenv("AI_MODEL"),
			client:   &http.Client{Timeout: 10 * time.Second},
		}
		if d.ai.model == "" {
			d.ai.model = "gpt-4o-mini"
		}
	}
	return d, nil
}

func main() {
	ctx := context.Background()
	d, err := newDefaultDeps(ctx)
	if err != nil {
		log.Fatalf("initializing deps: %v", err)
	}
	lambda.Start(func(ctx context.Context, _ events.CloudWatchEvent) error {
		return handleRun(ctx, d)
	})
}
