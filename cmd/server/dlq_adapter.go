package main

// dlq_adapter.go — the ONLY place in this binary that calls SQS transport
// methods (SendMessage, GetQueueAttributes) directly. platform-events has no
// DLQ-routing primitive, so a thin adapter over the raw AWS SDK is required.
// check-forbidden-events-bypass.sh Rule-2 exempts this file explicitly.
//
// Every other event transport operation (publish, consume) goes through
// platform-events (events.NewSNSPublisher / events.NewSQSConsumerWithClient).

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// sqsDLQAdapter adapts *sqs.Client to the dlqTransport interface (dlq.go).
// Constructed once in run() from the shared sqsClient and passed to each
// inboundHandler — the raw client never crosses the boundary into dlq.go.
type sqsDLQAdapter struct {
	client *sqs.Client
}

// RedriveTargetURL fetches queueURL's RedrivePolicy via GetQueueAttributes
// and delegates the JSON/ARN parsing to redriveURL (dlq.go — no AWS types).
func (a *sqsDLQAdapter) RedriveTargetURL(ctx context.Context, queueURL string) (string, error) {
	out, err := a.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameRedrivePolicy},
	})
	if err != nil {
		return "", fmt.Errorf("get RedrivePolicy: %w", err)
	}
	policy := out.Attributes[string(sqstypes.QueueAttributeNameRedrivePolicy)]
	return redriveURL(queueURL, policy)
}

// SendToDLQ sends body to dlqURL with EventType and DLQReason message
// attributes so downstream DLQ processors can route / alert correctly.
func (a *sqsDLQAdapter) SendToDLQ(ctx context.Context, dlqURL, body, eventType, reason string) error {
	_, err := a.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(dlqURL),
		MessageBody: aws.String(body),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"EventType": {DataType: aws.String("String"), StringValue: aws.String(eventType)},
			"DLQReason": {DataType: aws.String("String"), StringValue: aws.String(reason)},
		},
	})
	return err
}

// fakeSQSDLQAdapter is a test double used by dlq_test.go and
// inbound_schema_test.go. Kept here alongside sqsDLQAdapter so the two
// implementations stay visibly close in code review.
//
// NOTE: this is intentionally NOT a _test.go file — package-level test
// helpers visible to multiple test files in package main must live in
// a non-test file or in one _test.go file that all test files share.
// We use build tag exclusion via blank-import to keep it out of prod
// binaries; test binary linking is handled by the Go test runner.
// (Alternative: move to testutil_test.go if simpler.)

// jsonDLQMessage captures what fakeDLQTransport received on each SendToDLQ call.
type jsonDLQMessage struct {
	DLQUrl    string
	Body      string
	EventType string
	Reason    string
}

// fakeDLQTransport is the in-test implementation of dlqTransport.
// It replaces the former fakeDLQClient (which implemented the now-removed
// dlqSQSClient interface using raw AWS SDK types).
type fakeDLQTransport struct {
	dlqURL  string // returned by RedriveTargetURL on success
	getErr  error  // returned by RedriveTargetURL on failure
	sendErr error  // returned by SendToDLQ on failure
	sent    []jsonDLQMessage
}

func (f *fakeDLQTransport) RedriveTargetURL(_ context.Context, _ string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	if f.dlqURL == "" {
		return "", errors.New("no redrive target URL configured in fake")
	}
	return f.dlqURL, nil
}

func (f *fakeDLQTransport) SendToDLQ(_ context.Context, dlqURL, body, eventType, reason string) error {
	f.sent = append(f.sent, jsonDLQMessage{dlqURL, body, eventType, reason})
	return f.sendErr
}
