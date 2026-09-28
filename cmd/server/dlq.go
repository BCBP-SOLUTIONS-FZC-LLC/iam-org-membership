package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// DLQReason message-attribute values, matching the
// platform_dlq_messages_total{reason} label incremented at each reject site.
const (
	poisonPillReason      = "future_time_clamp"
	schemaViolationReason = "schema_violation"
)

// dlqReason reports whether err is a permanent reject that belongs in the
// DLQ now rather than after maxReceiveCount retries, and under which reason.
func dlqReason(err error) (string, bool) {
	switch {
	case errors.Is(err, consumeradapter.ErrPoisonPill):
		return poisonPillReason, true
	case errors.Is(err, errSchemaViolation):
		return schemaViolationReason, true
	default:
		return "", false
	}
}

// dlqTransport is the minimal interface the DLQ router needs from the
// underlying SQS transport. The concrete implementation — sqsDLQAdapter —
// lives in main.go, the only file allowed to hold the raw *sqs.Client.
// Keeping the interface here means dlq.go has zero AWS SDK imports while
// the composition root retains full visibility of what the adapter does.
//
// platform-events has no DLQ-routing primitive (any handler error simply
// leaves the message visible for SQS redrive), so a thin adapter over the
// raw SQS SDK is the only option. The check-forbidden-events-bypass.sh
// Rule-2 exemption for cmd/server/main.go documents this exception.
type dlqTransport interface {
	// RedriveTargetURL resolves queueURL's RedrivePolicy deadLetterTargetArn
	// and returns it as a sibling SQS queue URL.
	RedriveTargetURL(ctx context.Context, queueURL string) (string, error)
	// SendToDLQ sends body to dlqURL with EventType and DLQReason message attributes.
	SendToDLQ(ctx context.Context, dlqURL, body, eventType, reason string) error
}

// withDLQRouting wraps h with routeRejectsToDLQ, resolving the DLQ from
// queueURL's own RedrivePolicy so it can never drift from the infra-configured
// redrive target. If resolution fails the handler is returned unwrapped:
// permanent rejects still reach the DLQ via SQS redrive after maxReceiveCount.
func withDLQRouting(ctx context.Context, transport dlqTransport, queueURL string, h events.Handler, log port.Logger) events.Handler {
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dlqURL, err := transport.RedriveTargetURL(resolveCtx, queueURL)
	if err != nil {
		log.Warn("DLQ routing disabled — permanent rejects fall back to SQS redrive after maxReceiveCount",
			map[string]any{"queue_url": queueURL, "error": err.Error()})
		return h
	}
	log.Info("DLQ routing wired", map[string]any{"queue_url": queueURL, "dlq_url": dlqURL})
	return routeRejectsToDLQ(h, transport, dlqURL, log)
}

// routeRejectsToDLQ sends an envelope whose handler returned a permanent
// reject (see dlqReason) straight to dlqURL and acks the source message.
// platform-events has no "permanent failure" signal — any handler error
// just leaves the message visible — so without this:
//   - an EVT-15 reject would be redelivered until SQS redrive, and because
//     the clamp is re-evaluated on every delivery, a skewed event could be
//     accepted once wall-clock caught up with it;
//   - a schema violation would burn the full retry budget on a payload that
//     can never pass.
//
// If the DLQ send fails the original error is returned, so the message
// falls back to normal retry + redrive.
func routeRejectsToDLQ(h events.Handler, transport dlqTransport, dlqURL string, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := h(ctx, env)
		reason, ok := dlqReason(err)
		if !ok {
			return err
		}
		// The consumer already Glue-decoded Payload; clear dataschema so a
		// later DLQ redrive back to the source queue isn't decoded twice.
		env.SchemaID = ""
		body, mErr := json.Marshal(env)
		if mErr != nil {
			return errors.Join(err, fmt.Errorf("marshal envelope for DLQ: %w", mErr))
		}
		sErr := transport.SendToDLQ(ctx, dlqURL, string(body), env.Type, reason)
		if sErr != nil {
			if log != nil {
				log.Warn("DLQ send failed — falling back to SQS redrive",
					map[string]any{"event_id": env.ID, "event_type": env.Type, "reason": reason, "dlq_url": dlqURL, "error": sErr.Error()})
			}
			if metrics.DLQMessages != nil {
				metrics.DLQMessages.WithLabelValues(env.Type, reason).Inc()
			}
			return err
		}
		return nil
	}
}

// redriveURL parses a RedrivePolicy JSON string and returns the sibling DLQ URL.
// This is the pure-logic half of DLQ resolution; the SQS GetQueueAttributes call
// that fetches the policy string lives in sqsDLQAdapter.RedriveTargetURL (main.go).
func redriveURL(queueURL, policyJSON string) (string, error) {
	if policyJSON == "" {
		return "", errors.New("queue has no RedrivePolicy")
	}
	var rp struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
	}
	if err := json.Unmarshal([]byte(policyJSON), &rp); err != nil {
		return "", fmt.Errorf("parse RedrivePolicy: %w", err)
	}
	return siblingQueueURL(queueURL, rp.DeadLetterTargetArn)
}

// siblingQueueURL builds the URL of the queue named by arn
// (arn:aws:sqs:<region>:<account>:<name>) by swapping the account and name
// path segments of queueURL. SQS requires a DLQ to share its source queue's
// account and region, so the endpoint (real AWS or floci) carries over.
func siblingQueueURL(queueURL, arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sqs" || parts[4] == "" || parts[5] == "" {
		return "", fmt.Errorf("deadLetterTargetArn %q is not an SQS queue ARN", arn)
	}
	u, err := url.Parse(queueURL)
	if err != nil {
		return "", fmt.Errorf("parse queue URL: %w", err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("queue URL %q has no /<account>/<name> path", queueURL)
	}
	segs[len(segs)-2], segs[len(segs)-1] = parts[4], parts[5]
	u.Path = "/" + strings.Join(segs, "/")
	return u.String(), nil
}
