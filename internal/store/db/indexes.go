package db

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// EnsureIndexes creates all required indexes on the transactions collection.
// Safe to call on every startup — MongoDB ignores already-existing indexes.
func EnsureIndexes(ctx context.Context, coll *mongo.Collection) error {
	indexes := []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "client_order_id", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("unique_client_order_id"),
		},
		{
			Keys:    bson.D{{Key: "namespace", Value: 1}, {Key: "created_at", Value: -1}},
			Options: options.Index().SetName("namespace_created_at"),
		},
		{
			Keys:    bson.D{{Key: "namespace", Value: 1}, {Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}},
			Options: options.Index().SetName("namespace_user_created_at"),
		},
		{
			Keys:    bson.D{{Key: "namespace", Value: 1}, {Key: "provider_tx_id", Value: 1}},
			Options: options.Index().SetName("namespace_provider_tx_id"),
		},
		{
			Keys:    bson.D{{Key: "namespace", Value: 1}, {Key: "item_id", Value: 1}},
			Options: options.Index().SetName("namespace_item_id"),
		},
		{
			Keys:    bson.D{{Key: "namespace", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: 1}},
			Options: options.Index().SetName("namespace_status_updated_at"),
		},
		{
			// TTL index: DocumentDB auto-deletes documents when delete_at is reached.
			Keys:    bson.D{{Key: "delete_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(0).SetName("ttl_delete_at"),
		},
	}

	for _, idx := range indexes {
		_, err := coll.Indexes().CreateOne(ctx, idx)
		if err != nil {
			return err
		}
	}
	return nil
}
