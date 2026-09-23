//go:build integration

// Package integration_test hosts Phase 12 integration tests that exercise
// the async event pipeline end-to-end — outbox → SNS → SQS → consumer
// round-trip — against real Postgres + real SNS/SQS (via floci, an
// open-source AWS emulator running in a testcontainer).
//
// Design notes:
//
//   - One floci container is spun up per test package via TestMain and
//     shared across every test (cost: ~5–8s once). Postgres containers stay
//     per-test to preserve the RLS-role isolation the sibling test/postgres
//     suite depends on (~2s each).
//
//   - Every test provisions its OWN SNS topics + SQS queues with a unique
//     suffix (test-name-derived) so parallel tests never see each other's
//     traffic on the shared floci instance. Cleanup deletes those resources.
//
//   - The outbox runner and SQS consumers run as goroutines with tight
//     drain timeouts scoped to the test. `require.Eventually` polls the
//     observable side (SQS messages received, DB rows updated, processed_events
//     inserted) with sensible bounds so hangs surface fast.
//
//   - Filter-policy tests bypass the consumer and drain queues directly via
//     the raw SQS client so we can assert which queues received which
//     EventType attributes without relying on projection side effects.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
)

// ── Shared floci container ─────────────────────────────────────────────────

const (
	flociImage   = "floci/floci:2.1.0"
	flociAccount = "000000000000"
	flociRegion  = "ap-south-1"
)

var (
	sharedFloci           testcontainers.Container
	sharedEndpointURL     string
	sharedFlociErr        error
	sharedFlociOnce       sync.Once
	sharedFlociCtx        = context.Background()
	sharedFlociTerminated bool
)

// TestMain boots one floci container for the whole package and tears it
// down on exit. Individual tests get a lightweight "phase12Env" via
// newPhase12Env that reuses this floci instance.
func TestMain(m *testing.M) {
	ensureFlociCredentials()
	code := m.Run()
	if sharedFloci != nil && !sharedFlociTerminated {
		_ = sharedFloci.Terminate(sharedFlociCtx)
	}
	os.Exit(code)
}

// ensureFlociCredentials pins dummy static AWS credentials into the
// process environment when none are already configured. phase12Env's own
// snsCli/sqsCli pass explicit static credentials, but the outbox-runner
// SNS publishers the tests build via events.NewSNSPublisher only expose
// Region/EndpointURL/Logger — no credentials override — so they fall back
// to the AWS SDK's default credential chain. On a machine with no
// ~/.aws/credentials and no AWS_* env vars, that chain finds nothing and
// every real Publish call to floci fails at credential-resolution time
// (floci accepts any credentials, real or fake, but the SDK still requires
// *some*). Only set when unset, so a CI runner or developer machine with
// real/intentional credentials is left untouched.
func ensureFlociCredentials() {
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		_ = os.Setenv("AWS_ACCESS_KEY_ID", "test")
	}
	if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
		_ = os.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	}
}

// startFloci lazily boots the shared floci container. Idempotent across
// concurrent tests. Skips the test with a Docker-not-available message if
// the container fails to start (matches sibling postgres helpers). Floci
// starts every service (including Glue) by default — no LocalStack-style
// SERVICES= selection var needed.
func startFloci(t *testing.T) string {
	t.Helper()
	sharedFlociOnce.Do(func() {
		req := testcontainers.ContainerRequest{
			Image:        flociImage,
			ExposedPorts: []string{"4566/tcp"},
			Env: map[string]string{
				"FLOCI_DEFAULT_REGION":     flociRegion,
				"FLOCI_DEFAULT_ACCOUNT_ID": flociAccount,
			},
			WaitingFor: wait.ForLog("Ready.").
				WithStartupTimeout(60 * time.Second),
		}
		c, err := testcontainers.GenericContainer(sharedFlociCtx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if err != nil {
			sharedFlociErr = fmt.Errorf("start floci: %w", err)
			return
		}
		sharedFloci = c

		host, hErr := c.Host(sharedFlociCtx)
		if hErr != nil {
			sharedFlociErr = fmt.Errorf("floci host: %w", hErr)
			return
		}
		port, pErr := c.MappedPort(sharedFlociCtx, "4566/tcp")
		if pErr != nil {
			sharedFlociErr = fmt.Errorf("floci port: %w", pErr)
			return
		}
		sharedEndpointURL = fmt.Sprintf("http://%s:%s", host, port.Port())
		log.Printf("Phase 12 floci ready at %s", sharedEndpointURL)
	})
	if sharedFlociErr != nil {
		t.Skipf("skipping — floci unavailable (%v)", sharedFlociErr)
	}
	return sharedEndpointURL
}

// ── Per-test env ───────────────────────────────────────────────────────────

// phase12Env bundles everything a Phase 12 test needs: a fresh Postgres
// container with migrations applied, an SNS+SQS client aimed at the shared
// floci instance, and a unique topic/queue namespace so parallel tests never
// collide. Tests inject events via publishToTopic or writeOutboxRow, then
// observe on receiveMessages or via the DB.
type phase12Env struct {
	ctx      context.Context
	endpoint string

	// AWS clients (base-endpoint pinned to floci).
	snsCli *sns.Client
	sqsCli *sqs.Client

	// Postgres — pgxpool (raw superuser) is sufficient for wire tests; the
	// RLS pool isn't needed at the SNS/SQS boundary. If a test wants to
	// exercise projection it uses the raw pool for setup then invokes the
	// consumer with its own pgcommon.Pool wired inline.
	rawPool *dbseed.Pool
	dsn     string

	// Unique per-test suffix so parallel tests don't cross-contaminate.
	suffix string

	// Provisioned topology (test decides which resources it needs — the
	// harness helpers below take names, not fixed identifiers).
	createdTopics []string
	createdQueues []string
}

// newPhase12Env spins up a fresh Postgres container, applies migrations,
// grabs an SNS+SQS client aimed at the shared floci instance, and returns a
// per-test env with a unique namespace suffix.
func newPhase12Env(t *testing.T) *phase12Env {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping floci integration test in short mode")
	}
	ctx := context.Background()

	endpoint := startFloci(t)

	// Fresh Postgres per test — mirrors test/postgres/setupTestDB pattern
	// (superuser-only; RLS roles aren't relevant at the wire boundary).
	pgDSN, closePg := startPostgres(t, ctx)
	t.Cleanup(closePg)

	// Apply outbox schema FIRST (creates outbox_events), then domain migrations
	// (the domain migration alters outbox_events and must run after).
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: pgDSN}))
	require.NoError(t, pgadapter.RunMigrations(ctx, pgDSN))

	rawPool, err := dbseed.New(ctx, pgDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(flociRegion),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err)

	snsCli := sns.NewFromConfig(awsCfg, func(o *sns.Options) { o.BaseEndpoint = &endpoint })
	sqsCli := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) { o.BaseEndpoint = &endpoint })

	env := &phase12Env{
		ctx:      ctx,
		endpoint: endpoint,
		snsCli:   snsCli,
		sqsCli:   sqsCli,
		rawPool:  rawPool,
		dsn:      pgDSN,
		suffix:   strings.ToLower(strings.ReplaceAll(uuid.NewString(), "-", ""))[:12],
	}

	// Best-effort cleanup — delete every topic and queue this test
	// provisioned so floci state doesn't accumulate across the run.
	t.Cleanup(func() { env.cleanupAWSResources() })

	return env
}

// createTopic creates an SNS topic named "<base>-<suffix>" (unique per test)
// and returns its ARN. Registers for cleanup.
func (e *phase12Env) createTopic(t *testing.T, base string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%s", base, e.suffix)
	out, err := e.snsCli.CreateTopic(e.ctx, &sns.CreateTopicInput{Name: &name})
	require.NoError(t, err, "create SNS topic %s", name)
	arn := *out.TopicArn
	e.createdTopics = append(e.createdTopics, arn)
	return arn
}

// createQueue creates an SQS queue named "<base>-<suffix>" and returns its
// URL. If dlqArn is non-empty, wires RedrivePolicy with maxReceiveCount=5.
// Registers for cleanup.
func (e *phase12Env) createQueue(t *testing.T, base, dlqArn string, maxReceiveCount int) string {
	t.Helper()
	name := fmt.Sprintf("%s-%s", base, e.suffix)
	input := &sqs.CreateQueueInput{QueueName: &name}
	if dlqArn != "" {
		if maxReceiveCount <= 0 {
			maxReceiveCount = 5
		}
		policy := fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"%d"}`, dlqArn, maxReceiveCount)
		input.Attributes = map[string]string{"RedrivePolicy": policy}
	}
	out, err := e.sqsCli.CreateQueue(e.ctx, input)
	require.NoError(t, err, "create SQS queue %s", name)
	url := *out.QueueUrl
	e.createdQueues = append(e.createdQueues, url)
	return url
}

// queueArn returns the ARN attribute of a queue given its URL.
func (e *phase12Env) queueArn(t *testing.T, queueURL string) string {
	t.Helper()
	out, err := e.sqsCli.GetQueueAttributes(e.ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	require.NoError(t, err)
	return out.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
}

// subscribeQueue subscribes an SQS queue to an SNS topic with
// RawMessageDelivery=true (per LLD §7.3.2). If filterPolicy is non-empty
// it's applied as the FilterPolicy attribute.
func (e *phase12Env) subscribeQueue(t *testing.T, topicARN, queueURL, filterPolicy string) {
	t.Helper()
	qArn := e.queueArn(t, queueURL)
	subOut, err := e.snsCli.Subscribe(e.ctx, &sns.SubscribeInput{
		TopicArn:              &topicARN,
		Protocol:              strPtr("sqs"),
		Endpoint:              &qArn,
		ReturnSubscriptionArn: true,
		Attributes: map[string]string{
			"RawMessageDelivery": "true",
		},
	})
	require.NoError(t, err, "subscribe %s -> %s", topicARN, qArn)

	if filterPolicy != "" {
		_, err = e.snsCli.SetSubscriptionAttributes(e.ctx, &sns.SetSubscriptionAttributesInput{
			SubscriptionArn: subOut.SubscriptionArn,
			AttributeName:   strPtr("FilterPolicy"),
			AttributeValue:  &filterPolicy,
		})
		require.NoError(t, err, "set filter policy on %s", *subOut.SubscriptionArn)
	}

	// SNS+SQS subscriptions in floci settle quickly but not instantly —
	// give the propagation a tiny head-start so the first Publish doesn't
	// race the subscription registry.
	time.Sleep(150 * time.Millisecond)
}

// receiveMessages long-polls a queue and returns up to want messages within
// the timeout. Deletes each received message so a follow-up receive won't
// re-see it. Returns nil (not an error) if fewer than want arrive.
func (e *phase12Env) receiveMessages(t *testing.T, queueURL string, want int, timeout time.Duration) []sqstypes.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got []sqstypes.Message
	for time.Now().Before(deadline) && len(got) < want {
		remaining := int(time.Until(deadline).Seconds())
		if remaining <= 0 {
			remaining = 1
		}
		if remaining > 5 {
			remaining = 5
		}
		out, err := e.sqsCli.ReceiveMessage(e.ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              &queueURL,
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       int32(remaining),
			MessageAttributeNames: []string{"All"},
		})
		require.NoError(t, err, "receive from %s", queueURL)
		for _, m := range out.Messages {
			mm := m
			got = append(got, mm)
			_, err := e.sqsCli.DeleteMessage(e.ctx, &sqs.DeleteMessageInput{
				QueueUrl:      &queueURL,
				ReceiptHandle: mm.ReceiptHandle,
			})
			require.NoError(t, err, "delete from %s", queueURL)
			if len(got) >= want {
				break
			}
		}
	}
	return got
}

// drainQueue receives (and deletes) every message currently visible on the
// queue, subject to a short timeout. Used to assert a queue is EMPTY (i.e.
// filter policy rejected the message).
//
//nolint:unparam // test helper — callers pass the timeout explicitly so a slow case can raise it
func (e *phase12Env) drainQueue(t *testing.T, queueURL string, timeout time.Duration) []sqstypes.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got []sqstypes.Message
	for time.Now().Before(deadline) {
		out, err := e.sqsCli.ReceiveMessage(e.ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              &queueURL,
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       1,
			MessageAttributeNames: []string{"All"},
		})
		require.NoError(t, err)
		if len(out.Messages) == 0 {
			return got
		}
		for _, m := range out.Messages {
			mm := m
			got = append(got, mm)
			_, err := e.sqsCli.DeleteMessage(e.ctx, &sqs.DeleteMessageInput{
				QueueUrl:      &queueURL,
				ReceiptHandle: mm.ReceiptHandle,
			})
			require.NoError(t, err)
		}
	}
	return got
}

// publishEnvelope publishes a raw envelope JSON to the given topic. Stamps
// the "EventType" MessageAttribute so filter policies work (matches the
// platform-events SNS publisher exactly).
//
//nolint:unparam // extraAttrs kept for message-attribute scenarios; every current caller passes nil
func (e *phase12Env) publishEnvelope(t *testing.T, topicARN string, eventType string, body []byte, extraAttrs map[string]string) {
	t.Helper()
	attrs := map[string]snstypes.MessageAttributeValue{
		"EventType": {DataType: strPtr("String"), StringValue: strPtr(eventType)},
	}
	for k, v := range extraAttrs {
		v := v
		attrs[k] = snstypes.MessageAttributeValue{DataType: strPtr("String"), StringValue: &v}
	}
	msg := string(body)
	_, err := e.snsCli.Publish(e.ctx, &sns.PublishInput{
		TopicArn:          &topicARN,
		Message:           &msg,
		MessageAttributes: attrs,
	})
	require.NoError(t, err, "publish to %s", topicARN)
}

// cleanupAWSResources best-effort deletes topics + queues (+ their DLQs
// are separate queues owned by the test, so they're already tracked).
func (e *phase12Env) cleanupAWSResources() {
	for _, q := range e.createdQueues {
		_, _ = e.sqsCli.DeleteQueue(e.ctx, &sqs.DeleteQueueInput{QueueUrl: &q})
	}
	for _, tpc := range e.createdTopics {
		_, _ = e.snsCli.DeleteTopic(e.ctx, &sns.DeleteTopicInput{TopicArn: &tpc})
	}
}

// mustMarshal is a tiny helper for building event bodies inline.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func strPtr(s string) *string { return &s }
