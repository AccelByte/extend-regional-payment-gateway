package docdb

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
)

type Store struct {
	coll *mongo.Collection
}

func New(coll *mongo.Collection) *Store {
	return &Store{coll: coll}
}

func (s *Store) CreateTransaction(ctx context.Context, tx *model.Transaction) error {
	_, err := s.coll.InsertOne(ctx, tx)
	if mongo.IsDuplicateKeyError(err) {
		return store.ErrDuplicateClientOrderID
	}
	return err
}

func (s *Store) FindByID(ctx context.Context, id string) (*model.Transaction, error) {
	var tx model.Transaction
	err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&tx)
	if err == mongo.ErrNoDocuments {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

func (s *Store) FindByClientOrderID(ctx context.Context, clientOrderID string) (*model.Transaction, error) {
	var tx model.Transaction
	err := s.coll.FindOne(ctx, bson.D{{Key: "client_order_id", Value: clientOrderID}}).Decode(&tx)
	if err == mongo.ErrNoDocuments {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

func (s *Store) AtomicClaimFulfilling(ctx context.Context, txnID, providerTxID string) (*model.Transaction, error) {
	now := time.Now().UTC()
	var tx model.Transaction
	err := s.coll.FindOneAndUpdate(
		ctx,
		bson.D{{Key: "_id", Value: txnID}, {Key: "status", Value: model.StatusPending}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: model.StatusFulfilling},
			{Key: "provider_tx_id", Value: providerTxID},
			{Key: "updated_at", Value: now},
		}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&tx)
	if err == mongo.ErrNoDocuments {
		// Check if transaction exists at all
		existErr := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: txnID}}).Err()
		if existErr == mongo.ErrNoDocuments {
			return nil, store.ErrNotFound
		}
		return nil, store.ErrNoDocuments
	}
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

func (s *Store) CommitFulfilled(ctx context.Context, txnID, providerStatus string, deleteAt time.Time) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}, {Key: "status", Value: model.StatusFulfilling}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: model.StatusFulfilled},
			{Key: "provider_status", Value: providerStatus},
			{Key: "delete_at", Value: deleteAt},
			{Key: "updated_at", Value: now},
		}}},
	)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, txnID, reason string, deleteAt time.Time) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: model.StatusFailed},
			{Key: "failure_reason", Value: reason},
			{Key: "delete_at", Value: deleteAt},
			{Key: "updated_at", Value: now},
		}}},
	)
	return err
}

func (s *Store) MarkCanceledIfPending(ctx context.Context, txnID, reason, providerStatus string, deleteAt time.Time) error {
	return s.markTerminalIfPending(ctx, txnID, model.StatusCanceled, reason, providerStatus, deleteAt)
}

func (s *Store) MarkExpiredIfPending(ctx context.Context, txnID, reason, providerStatus string, deleteAt time.Time) error {
	return s.markTerminalIfPending(ctx, txnID, model.StatusExpired, reason, providerStatus, deleteAt)
}

func (s *Store) markTerminalIfPending(ctx context.Context, txnID, terminalStatus, reason, providerStatus string, deleteAt time.Time) error {
	now := time.Now().UTC()
	res, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}, {Key: "status", Value: model.StatusPending}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: terminalStatus},
			{Key: "failure_reason", Value: reason},
			{Key: "provider_status", Value: providerStatus},
			{Key: "delete_at", Value: deleteAt},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		existErr := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: txnID}}).Err()
		if existErr == mongo.ErrNoDocuments {
			return store.ErrNotFound
		}
		return store.ErrNoDocuments
	}
	return nil
}

func (s *Store) AttachProviderTransaction(ctx context.Context, txnID, provider, customProviderName, providerTxID, paymentURL string) error {
	now := time.Now().UTC()
	res, err := s.coll.UpdateOne(
		ctx,
		bson.D{
			{Key: "_id", Value: txnID},
			{Key: "status", Value: model.StatusPending},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "provider_tx_id", Value: bson.D{{Key: "$exists", Value: false}}}},
				bson.D{{Key: "provider_tx_id", Value: ""}},
			}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "provider", Value: provider},
			{Key: "custom_provider_name", Value: customProviderName},
			{Key: "provider_tx_id", Value: providerTxID},
			{Key: "payment_url", Value: paymentURL},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		existErr := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: txnID}}).Err()
		if existErr == mongo.ErrNoDocuments {
			return store.ErrNotFound
		}
		return store.ErrNoDocuments
	}
	return nil
}

func (s *Store) ClearProviderTransactionIfPending(ctx context.Context, txnID, providerTxID string) error {
	now := time.Now().UTC()
	res, err := s.coll.UpdateOne(
		ctx,
		bson.D{
			{Key: "_id", Value: txnID},
			{Key: "status", Value: model.StatusPending},
			{Key: "provider_tx_id", Value: providerTxID},
		},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "provider", Value: ""},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$unset", Value: bson.D{
				{Key: "custom_provider_name", Value: ""},
				{Key: "provider_tx_id", Value: ""},
				{Key: "payment_url", Value: ""},
				{Key: "provider_status", Value: ""},
			}},
		},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		existErr := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: txnID}}).Err()
		if existErr == mongo.ErrNoDocuments {
			return store.ErrNotFound
		}
		return store.ErrNoDocuments
	}
	return nil
}

func (s *Store) UpdateProviderTransactionID(ctx context.Context, txnID, providerTxID string) error {
	now := time.Now().UTC()
	res, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "provider_tx_id", Value: providerTxID},
			{Key: "updated_at", Value: now},
		}}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteTransaction(ctx context.Context, id string) error {
	_, err := s.coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
	return err
}

func (s *Store) ListTransactions(ctx context.Context, q store.ListQuery) ([]*model.Transaction, string, error) {
	pageSize := q.PageSize
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}

	filter := bson.D{}
	if q.Namespace != "" {
		filter = append(filter, bson.E{Key: "namespace", Value: q.Namespace})
	}
	if q.UserID != "" {
		filter = append(filter, bson.E{Key: "user_id", Value: q.UserID})
	}
	if q.StatusFilter != "" {
		filter = append(filter, bson.E{Key: "status", Value: q.StatusFilter})
	}
	if len(q.StatusFilters) > 0 {
		filter = append(filter, bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: q.StatusFilters}}})
	}
	if q.Provider != "" {
		filter = append(filter, bson.E{Key: "provider", Value: q.Provider})
	}
	if search := strings.TrimSpace(q.Search); search != "" {
		filter = append(filter, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "_id", Value: search}},
			bson.D{{Key: "provider_tx_id", Value: search}},
			bson.D{{Key: "item_id", Value: search}},
		}})
	}
	if q.Cursor != "" {
		filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$lt", Value: q.Cursor}}})
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetLimit(int64(pageSize) + 1)

	cursor, err := s.coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, "", err
	}
	defer cursor.Close(ctx)

	var results []*model.Transaction
	if err := cursor.All(ctx, &results); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if int32(len(results)) > pageSize {
		results = results[:pageSize]
		nextCursor = results[len(results)-1].ID
	}
	return results, nextCursor, nil
}

func (s *Store) CountPendingByUser(ctx context.Context, namespace, userID string) (int64, error) {
	return s.coll.CountDocuments(ctx, bson.D{
		{Key: "namespace", Value: namespace},
		{Key: "user_id", Value: userID},
		{Key: "status", Value: model.StatusPending},
	})
}

func (s *Store) FindStuckFulfilling(ctx context.Context, olderThan time.Time) ([]*model.Transaction, error) {
	cursor, err := s.coll.Find(ctx, bson.D{
		{Key: "status", Value: model.StatusFulfilling},
		{Key: "updated_at", Value: bson.D{{Key: "$lt", Value: olderThan}}},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var results []*model.Transaction
	return results, cursor.All(ctx, &results)
}

func (s *Store) FindStuckPending(ctx context.Context, olderThan time.Time) ([]*model.Transaction, error) {
	now := time.Now().UTC()
	cursor, err := s.coll.Find(ctx, bson.D{
		{Key: "status", Value: model.StatusPending},
		{Key: "expires_at", Value: bson.D{{Key: "$gt", Value: now}}},
		{Key: "created_at", Value: bson.D{{Key: "$lt", Value: olderThan}}},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var results []*model.Transaction
	return results, cursor.All(ctx, &results)
}

func (s *Store) FindExpiredPending(ctx context.Context, now time.Time) ([]*model.Transaction, error) {
	cursor, err := s.coll.Find(ctx, bson.D{
		{Key: "status", Value: model.StatusPending},
		{Key: "expires_at", Value: bson.D{{Key: "$lte", Value: now}}},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var results []*model.Transaction
	return results, cursor.All(ctx, &results)
}

func (s *Store) IncrementRetries(ctx context.Context, txnID string) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "retries", Value: 1}}}, {Key: "$set", Value: bson.D{{Key: "updated_at", Value: now}}}},
	)
	return err
}

func (s *Store) ResetToPending(ctx context.Context, txnID string) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: model.StatusPending}, {Key: "updated_at", Value: now}}}},
	)
	return err
}

func (s *Store) AtomicClaimRefunding(ctx context.Context, txnID, reason string) (*model.Transaction, error) {
	now := time.Now().UTC()
	existsErr := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: txnID}, {Key: "refund", Value: bson.D{{Key: "$exists", Value: true}}}}).Err()
	setFields := bson.D{
		{Key: "refund.status", Value: model.RefundStatusRefunding},
		{Key: "refund.reason", Value: reason},
		{Key: "refund.failure_reason", Value: ""},
		{Key: "refund.updated_at", Value: now},
		{Key: "updated_at", Value: now},
	}
	if existsErr == mongo.ErrNoDocuments {
		setFields = append(setFields,
			bson.E{Key: "refund.created_at", Value: now},
			bson.E{Key: "refund.provider_refunded", Value: false},
		)
	}

	var tx model.Transaction
	err := s.coll.FindOneAndUpdate(
		ctx,
		bson.D{
			{Key: "_id", Value: txnID},
			{Key: "status", Value: model.StatusFulfilled},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "refund", Value: bson.D{{Key: "$exists", Value: false}}}},
				bson.D{{Key: "refund.status", Value: model.RefundStatusRefundFailed}},
			}},
		},
		bson.D{{Key: "$set", Value: setFields}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&tx)
	if err == mongo.ErrNoDocuments {
		return nil, store.ErrNoDocuments
	}
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

func (s *Store) AtomicClaimExternalRefunding(ctx context.Context, txnID, reason string) (*model.Transaction, error) {
	now := time.Now().UTC()
	existsErr := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: txnID}, {Key: "refund", Value: bson.D{{Key: "$exists", Value: true}}}}).Err()
	setFields := bson.D{
		{Key: "refund.status", Value: model.RefundStatusRefunding},
		{Key: "refund.reason", Value: reason},
		{Key: "refund.failure_reason", Value: ""},
		{Key: "refund.provider_refunded", Value: true},
		{Key: "refund.updated_at", Value: now},
		{Key: "updated_at", Value: now},
	}
	if existsErr == mongo.ErrNoDocuments {
		setFields = append(setFields, bson.E{Key: "refund.created_at", Value: now})
	}

	var tx model.Transaction
	err := s.coll.FindOneAndUpdate(
		ctx,
		bson.D{
			{Key: "_id", Value: txnID},
			{Key: "status", Value: model.StatusFulfilled},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "refund", Value: bson.D{{Key: "$exists", Value: false}}}},
				bson.D{{Key: "refund.status", Value: model.RefundStatusRefundFailed}},
			}},
		},
		bson.D{{Key: "$set", Value: setFields}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&tx)
	if err == mongo.ErrNoDocuments {
		existErr := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: txnID}}).Err()
		if existErr == mongo.ErrNoDocuments {
			return nil, store.ErrNotFound
		}
		return nil, store.ErrNoDocuments
	}
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

func (s *Store) MarkRefundProviderSucceeded(ctx context.Context, txnID string) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "refund.provider_refunded", Value: true},
			{Key: "refund.updated_at", Value: now},
			{Key: "updated_at", Value: now},
		}}},
	)
	return err
}

func (s *Store) CommitRefund(ctx context.Context, txnID string) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "refund.status", Value: model.RefundStatusRefunded},
			{Key: "refund.provider_refunded", Value: true},
			{Key: "refund.updated_at", Value: now},
			{Key: "updated_at", Value: now},
		}}},
	)
	return err
}

func (s *Store) MarkRefundFailed(ctx context.Context, txnID, reason string) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: txnID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "refund.status", Value: model.RefundStatusRefundFailed},
			{Key: "refund.failure_reason", Value: reason},
			{Key: "refund.updated_at", Value: now},
			{Key: "updated_at", Value: now},
		}}},
	)
	return err
}
