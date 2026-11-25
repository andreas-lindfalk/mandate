package events

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
)

// Exported is an event with its hashes, as written to an export. Anyone with
// the tenant id can recompute the chain from these fields alone, without
// the database.
type Exported struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	ActorKind string          `json:"actorKind"`
	ActorId   *uuid.UUID      `json:"actorId"`
	SubjectId *uuid.UUID      `json:"subjectId"`
	RunId     *string         `json:"runId"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"createdAt"`
	PrevHash  string          `json:"prevHash"`
	Hash      string          `json:"hash"`
}

// Export returns the whole chain of the tenant in ctx, oldest first.
func Export(ctx context.Context, tx pgx.Tx) ([]Exported, error) {
	if _, ok := actor.TenantFrom(ctx); !ok {
		return nil, errors.New("events: no tenant in context")
	}
	rows, err := tx.Query(ctx, `SELECT seq, type, actor_kind, actor_id, subject_id, run_id, payload,
		prev_hash, hash, created_at FROM mandate.events ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Exported, error) {
		var stored Stored
		var prevHash, hash []byte
		if err := row.Scan(&stored.Seq, &stored.Type, &stored.ActorKind, &stored.ActorId, &stored.SubjectId,
			&stored.RunId, &stored.Payload, &prevHash, &hash, &stored.CreatedAt); err != nil {
			return Exported{}, err
		}
		payload, err := canonicalJson(stored.Payload)
		if err != nil {
			return Exported{}, err
		}
		return Exported{
			Seq: stored.Seq, Type: stored.Type, ActorKind: stored.ActorKind, ActorId: stored.ActorId,
			SubjectId: stored.SubjectId, RunId: stored.RunId, Payload: payload,
			CreatedAt: stored.CreatedAt.UTC().Format(time.RFC3339Nano),
			PrevHash:  hex.EncodeToString(prevHash), Hash: hex.EncodeToString(hash),
		}, nil
	})
}

// VerifyExported recomputes an exported chain without a database. It returns
// the head hash, or ErrBroken with the first sequence number that fails.
func VerifyExported(tenantId uuid.UUID, events []Exported) (string, error) {
	expectedPrev := genesisHash
	for i, event := range events {
		prevHash, errPrev := hex.DecodeString(event.PrevHash)
		hash, errHash := hex.DecodeString(event.Hash)
		createdAt, errTime := time.Parse(time.RFC3339Nano, event.CreatedAt)
		if errPrev != nil || errHash != nil || errTime != nil || event.Seq != int64(i+1) || !bytes.Equal(prevHash, expectedPrev) {
			return "", fmt.Errorf("%w at seq %d", ErrBroken, event.Seq)
		}
		payload, err := canonicalJson(event.Payload)
		if err != nil {
			return "", fmt.Errorf("%w at seq %d: %w", ErrBroken, event.Seq, err)
		}
		want, err := chainHash(tenantId, prevHash, Stored{
			Seq: event.Seq, Type: event.Type, ActorKind: event.ActorKind, ActorId: event.ActorId, SubjectId: event.SubjectId,
			RunId: event.RunId, Payload: payload, CreatedAt: createdAt,
		})
		if err != nil {
			return "", err
		}
		if !bytes.Equal(want, hash) {
			return "", fmt.Errorf("%w at seq %d", ErrBroken, event.Seq)
		}
		expectedPrev = hash
	}
	return hex.EncodeToString(expectedPrev), nil
}
