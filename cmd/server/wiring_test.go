package main

import (
	"testing"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTopicPublisher_EmptyARN_ReturnsNoop(t *testing.T) {
	pub, err := buildTopicPublisher("", events.NoopCodec{}, nil)
	require.NoError(t, err)
	_, ok := pub.(eventbusadapter.NoopPublisher)
	assert.True(t, ok, "unset SNS_TOPIC_*_ARN must wire the local noop publisher")
}

func TestSqsEnvForQueue_OverridesURLAndConcurrency(t *testing.T) {
	base := eventcfg.SQSConfigEnv{
		QueueURL:    "https://unused.example/SQS_QUEUE_URL",
		Region:      "ap-south-1",
		Concurrency: 1,
		MaxMessages: 10,
	}
	got := sqsEnvForQueue(base, "https://sqs.example/tenant-orgm-q", 4)
	assert.Equal(t, "https://sqs.example/tenant-orgm-q", got.QueueURL)
	assert.Equal(t, 4, got.Concurrency)
	assert.Equal(t, "ap-south-1", got.Region)
	assert.Equal(t, int32(10), got.MaxMessages)
	assert.Equal(t, 1, base.Concurrency, "base env must not be mutated")
}
