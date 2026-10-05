package sqspipeline

import (
	"testing"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/stretchr/testify/assert"
)

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
