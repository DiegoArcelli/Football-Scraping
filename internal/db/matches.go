package db

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// MatchesCollection is the Mongo collection scraped match-centre data is
// stored in.
const MatchesCollection = "matches"

// Match is the scraped match-centre data produced by a single job.
type Match struct {
	ID        primitive.ObjectID     `bson:"_id,omitempty" json:"id"`
	JobID     primitive.ObjectID     `bson:"job_id" json:"job_id"`
	League    string                 `bson:"league" json:"league"`
	Season    string                 `bson:"season" json:"season"`
	URL       string                 `bson:"url" json:"url"`
	Data      map[string]interface{} `bson:"data" json:"data"`
	CreatedAt time.Time              `bson:"created_at" json:"created_at"`
}

// MatchExists reports whether the matches collection already has an entry
// for the given match ID (stored as a key under the "data" field, e.g.
// data.matchInfo.matchId).
func MatchExists(ctx context.Context, client *mongo.Client, matchID string) (bool, error) {
	coll := client.Database(DatabaseName).Collection(MatchesCollection)

	err := coll.FindOne(ctx, bson.M{"data." + matchID: bson.M{"$exists": true}}).Err()
	if err == mongo.ErrNoDocuments {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

// SaveMatch stores a job's scraped match data in the matches collection.
func SaveMatch(ctx context.Context, client *mongo.Client, jobID primitive.ObjectID, league, season, url string, data map[string]interface{}) error {
	match := Match{
		ID:        primitive.NewObjectID(),
		JobID:     jobID,
		League:    league,
		Season:    season,
		URL:       url,
		Data:      data,
		CreatedAt: time.Now().UTC(),
	}

	coll := client.Database(DatabaseName).Collection(MatchesCollection)
	_, err := coll.InsertOne(ctx, match)
	return err
}
