package db

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// JobsCollection is the Mongo collection scrape job status is tracked in.
const JobsCollection = "jobs"

type JobStatus string

const (
	// JobStatusPending marks a job as created or picked up by a worker but
	// not finished yet.
	JobStatusPending JobStatus = "pending"
	// JobStatusCompleted marks a job whose result was scraped and stored
	// successfully.
	JobStatusCompleted JobStatus = "completed"
	// JobStatusError marks a job that failed while being processed.
	JobStatusError JobStatus = "error"
)

// Job tracks the state of a single match-scrape job.
type Job struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	MatchID   string             `bson:"match_id" json:"match_id"`
	League    string             `bson:"league" json:"league"`
	Season    string             `bson:"season" json:"season"`
	URL       string             `bson:"url" json:"url"`
	Status    JobStatus          `bson:"status" json:"status"`
	CreatedAt time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time          `bson:"updated_at" json:"updated_at"`
}

// CreateJob inserts a new pending job for the given match URL, match ID,
// league, and season, and returns it with its assigned ID.
func CreateJob(ctx context.Context, client *mongo.Client, url, matchID, league, season string) (Job, error) {
	now := time.Now().UTC()
	job := Job{
		ID:        primitive.NewObjectID(),
		MatchID:   matchID,
		League:    league,
		Season:    season,
		URL:       url,
		Status:    JobStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}

	coll := client.Database(DatabaseName).Collection(JobsCollection)
	if _, err := coll.InsertOne(ctx, job); err != nil {
		return Job{}, err
	}

	return job, nil
}

// GetJob fetches a job by its ID.
func GetJob(ctx context.Context, client *mongo.Client, id primitive.ObjectID) (Job, error) {
	coll := client.Database(DatabaseName).Collection(JobsCollection)

	var job Job
	if err := coll.FindOne(ctx, bson.M{"_id": id}).Decode(&job); err != nil {
		return Job{}, err
	}

	return job, nil
}

// UpdateJobStatus sets a job's status (and updated_at) in the jobs
// collection.
func UpdateJobStatus(ctx context.Context, client *mongo.Client, id primitive.ObjectID, status JobStatus) error {
	coll := client.Database(DatabaseName).Collection(JobsCollection)

	_, err := coll.UpdateOne(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"status": status, "updated_at": time.Now().UTC()}},
	)
	return err
}
