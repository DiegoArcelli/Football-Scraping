package db

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// RegionsCollection is the Mongo collection regions are stored in.
const RegionsCollection = "regions"

// SaveRegions replaces the contents of the regions collection with the
// given list, so the collection always reflects the latest scrape. It takes
// a type parameter (rather than depending on the scrape package's Region
// type directly) so the scrape package can depend on db without an import
// cycle.
func SaveRegions[T any](ctx context.Context, client *mongo.Client, regions []T) error {
	coll := client.Database(DatabaseName).Collection(RegionsCollection)

	if _, err := coll.DeleteMany(ctx, bson.M{}); err != nil {
		return err
	}

	if len(regions) == 0 {
		return nil
	}

	docs := make([]interface{}, len(regions))
	for i, r := range regions {
		docs[i] = r
	}

	_, err := coll.InsertMany(ctx, docs)
	return err
}

// LoadRegions fetches every document in the regions collection. Like
// SaveRegions, it's generic rather than tied to the scrape package's Region
// type so db doesn't need to import scrape (which imports db).
func LoadRegions[T any](ctx context.Context, client *mongo.Client) ([]T, error) {
	coll := client.Database(DatabaseName).Collection(RegionsCollection)

	cursor, err := coll.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var regions []T
	if err := cursor.All(ctx, &regions); err != nil {
		return nil, err
	}

	return regions, nil
}
