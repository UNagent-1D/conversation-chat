package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/UNagent-1D/conversation-chat/internal/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// SessionRepo handles MongoDB persistence for session records.
type SessionRepo struct {
	db *mongo.Database
}

// NewSessionRepo creates a new SessionRepo.
func NewSessionRepo(db *mongo.Database) *SessionRepo {
	return &SessionRepo{db: db}
}

// collection returns the tenant-scoped collection name.
func (r *SessionRepo) collection(tenantSlug string) *mongo.Collection {
	return r.db.Collection("sessions_" + tenantSlug)
}

// EnsureIndexes creates indexes on a tenant collection (call once at session creation if needed).
func (r *SessionRepo) EnsureIndexes(ctx context.Context, tenantSlug string) error {
	coll := r.collection(tenantSlug)
	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "end_user_id", Value: 1}}},
		{Keys: bson.D{{Key: "state", Value: 1}}},
		{Keys: bson.D{{Key: "opened_at", Value: -1}}},
	}
	_, err := coll.Indexes().CreateMany(ctx, indexes)
	return err
}

// Create persists a new session document.
func (r *SessionRepo) Create(ctx context.Context, session domain.SessionRecord) error {
	coll := r.collection(session.TenantSlug)
	_, err := coll.InsertOne(ctx, session)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetByID retrieves a session by its ID within a tenant's collection.
func (r *SessionRepo) GetByID(ctx context.Context, tenantSlug, sessionID string) (*domain.SessionRecord, error) {
	coll := r.collection(tenantSlug)
	var session domain.SessionRecord
	err := coll.FindOne(ctx, bson.M{"_id": sessionID}).Decode(&session)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find session: %w", err)
	}
	return &session, nil
}

// UpdateState transitions the session to a new state.
func (r *SessionRepo) UpdateState(ctx context.Context, tenantSlug, sessionID string, state domain.SessionState) error {
	coll := r.collection(tenantSlug)
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": sessionID},
		bson.M{"$set": bson.M{"state": state}},
	)
	return err
}

// AppendTurn appends a turn to the session's turns array.
func (r *SessionRepo) AppendTurn(ctx context.Context, tenantSlug, sessionID string, turn domain.Turn) error {
	coll := r.collection(tenantSlug)
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": sessionID},
		bson.M{"$push": bson.M{"turns": turn}},
	)
	return err
}

// AppendEscalationEntry appends an escalation log entry to the session.
func (r *SessionRepo) AppendEscalationEntry(ctx context.Context, tenantSlug, sessionID string, entry domain.EscalationEntry) error {
	coll := r.collection(tenantSlug)
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": sessionID},
		bson.M{"$push": bson.M{"escalation_log": entry}},
	)
	return err
}

// UpdateEscalationOperator sets the operator_id and resolved_at on the last escalation entry.
// Reads the document to find the last index, then updates that specific element.
func (r *SessionRepo) UpdateEscalationOperator(ctx context.Context, tenantSlug, sessionID string, operatorID string, resolvedAt *time.Time) error {
	coll := r.collection(tenantSlug)

	var doc struct {
		EscalationLog []bson.M `bson:"escalation_log"`
	}
	if err := coll.FindOne(ctx, bson.M{"_id": sessionID}).Decode(&doc); err != nil {
		return err
	}
	if len(doc.EscalationLog) == 0 {
		return nil
	}

	lastIdx := len(doc.EscalationLog) - 1
	key := fmt.Sprintf("escalation_log.%d", lastIdx)
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": sessionID},
		bson.M{"$set": bson.M{
			key + ".operator_id": operatorID,
			key + ".resolved_at": resolvedAt,
		}},
	)
	return err
}

// Close marks a session as closed and sets closed_at timestamp.
func (r *SessionRepo) Close(ctx context.Context, tenantSlug, sessionID string) error {
	now := time.Now().UTC()
	coll := r.collection(tenantSlug)
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": sessionID},
		bson.M{"$set": bson.M{
			"state":     domain.StateClosed,
			"closed_at": now,
		}},
	)
	return err
}

// Ping checks MongoDB connectivity (used by health handler).
func (r *SessionRepo) Ping(ctx context.Context) error {
	return r.db.Client().Ping(ctx, nil)
}
