package queue

import (
	"context"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// JobsStream is the Redis stream worker processes read job IDs from. We use
// a stream (rather than a plain list) so jobs go through a consumer group:
// a message stays in the group's pending entries list until a worker XAcks
// it, so if a worker dies mid-job another worker can reclaim and retry it
// instead of the job silently being lost.
const JobsStream = "match_jobs"

// JobsConsumerGroup is the consumer group workers read the jobs stream
// through.
const JobsConsumerGroup = "match_jobs_workers"

// Connect dials the Redis instance pointed to by the REDIS_ADDR env var
// (falling back to a local instance) and verifies the connection.
func Connect(ctx context.Context) (*redis.Client, error) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: addr})

	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, err
	}

	return client, nil
}

// EnsureJobsStream makes sure the jobs stream and its consumer group exist,
// creating them on first use. Safe to call every time jobs are enqueued.
func EnsureJobsStream(ctx context.Context, client *redis.Client) error {
	err := client.XGroupCreateMkStream(ctx, JobsStream, JobsConsumerGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// PushJob adds a job ID to the jobs stream so a worker process can read it,
// look the job up in Mongo, and process it.
func PushJob(ctx context.Context, client *redis.Client, jobID string) error {
	return client.XAdd(ctx, &redis.XAddArgs{
		Stream: JobsStream,
		Values: map[string]interface{}{"job_id": jobID},
	}).Err()
}
