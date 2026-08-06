// Command register creates a project in the DynamoDB projects table and
// mints a DSN-compatible public key, printing the DSN to hand to a Sentry SDK.
//
// Usage:
//
//	go run . -project my-app -host <api-gateway-host> [-region ap-northeast-2] [-table sentry-ingest-projects] [-secret]
//
// The host is only used to render the DSN string; it is not stored.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type projectRow struct {
	ProjectID string `dynamodbav:"project_id"`
	PublicKey string `dynamodbav:"public_key"`
	SecretKey string `dynamodbav:"secret_key,omitempty"`
	CreatedAt string `dynamodbav:"created_at"`
	Status    string `dynamodbav:"status"`
}

func randKey(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func main() {
	var (
		projectID  = flag.String("project", "", "project id (required)")
		host       = flag.String("host", "", "API Gateway host for the DSN, e.g. abc123.execute-api.ap-northeast-2.amazonaws.com (required)")
		region     = flag.String("region", "ap-northeast-2", "AWS region")
		table      = flag.String("table", "sentry-ingest-projects", "DynamoDB projects table name")
		withSecret = flag.Bool("secret", false, "also mint a legacy secret key (optional/deprecated)")
	)
	flag.Parse()

	if *projectID == "" {
		log.Fatal("-project is required")
	}
	if *host == "" {
		log.Fatal("-host is required (API Gateway host for DSN rendering)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
	if err != nil {
		log.Fatalf("loading AWS config: %v", err)
	}
	ddb := dynamodb.NewFromConfig(cfg)

	publicKey, err := randKey(16)
	if err != nil {
		log.Fatalf("generating public key: %v", err)
	}
	secretKey := ""
	if *withSecret {
		if secretKey, err = randKey(16); err != nil {
			log.Fatalf("generating secret key: %v", err)
		}
	}

	row := projectRow{
		ProjectID: *projectID,
		PublicKey: publicKey,
		SecretKey: secretKey,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "active",
	}
	item, err := attributevalue.MarshalMap(row)
	if err != nil {
		log.Fatalf("marshaling project row: %v", err)
	}

	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(*table),
		Item:      item,
	}); err != nil {
		log.Fatalf("writing project to %s: %v", *table, err)
	}

	dsn := fmt.Sprintf("https://%s@%s/%s", publicKey, *host, *projectID)
	// Output errors are not actionable for a CLI; explicitly ignore them.
	_, _ = fmt.Fprintf(os.Stdout, "project_id:  %s\n", *projectID)
	_, _ = fmt.Fprintf(os.Stdout, "public_key:  %s\n", publicKey)
	if *withSecret {
		_, _ = fmt.Fprintf(os.Stdout, "secret_key:  %s\n", secretKey)
	}
	_, _ = fmt.Fprintf(os.Stdout, "status:      %s\n", row.Status)
	_, _ = fmt.Fprintf(os.Stdout, "\nDSN for Sentry SDK:\n%s\n", dsn)
}
