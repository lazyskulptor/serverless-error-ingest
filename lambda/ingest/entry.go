package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

func bytesReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}

func nowFunc() func() time.Time {
	return time.Now
}

func main() {
	// Structured JSON logs for CloudWatch Logs Insights (id/count only — the
	// handler never logs event content).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	ctx := context.Background()

	d, err := newDefaultDeps(ctx)
	if err != nil {
		log.Fatalf("initializing deps: %v", err)
	}

	lambda.Start(func(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return handleRequest(ctx, req, d)
	})
}
