package main

import (
	"testing"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTopicPublisher_EmptyARN_ReturnsNoop(t *testing.T) {
	pub, err := buildTopicPublisher("", events.NoopCodec{}, nil)
	require.NoError(t, err)
	_, ok := pub.(eventbusadapter.NoopPublisher)
	assert.True(t, ok, "unset SNS_TOPIC_*_ARN must wire the local noop publisher")
}
