// Package events appends to and verifies the per-tenant, hash chained event
// chain. Each event's hash covers the previous hash and a canonical encoding
// of the event, so any edit, removal or reordering breaks verification. The
// chain is the evidence: it says who did what, under which mandate, and why
// something was not done.
package events

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
)

var genesisHash = make([]byte, sha256.Size)

type Event struct {
	Type      string
	SubjectId uuid.UUID
	Payload   any
}

type Stored struct {
	Seq       int64
	Type      string
	ActorKind string
	ActorId   *uuid.UUID
	SubjectId *uuid.UUID
	RunId     *string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// Append writes the event inside tx for the tenant and actor in ctx.
func Append(ctx context.Context, tx pgx.Tx, event Event) (int64, error) {
	seqs, err := AppendMany(ctx, tx, []Event{event})
	if err != nil {
		return 0, err
	}
	return seqs[0], nil
}

// AppendMany writes events in order, in one round trip after a transaction
// scoped advisory lock per tenant: the chain is computed in Go and the rows
// are sent as one batch.
func AppendMany(ctx context.Context, tx pgx.Tx, events []Event) ([]int64, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return nil, errors.New("events: no tenant in context")
	}
	a, ok := actor.From(ctx)
	if !ok {
		return nil, errors.New("events: no actor in context")
	}
	if len(events) == 0 {
		return nil, nil
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 7))`, tenantId); err != nil {
		return nil, err
	}
	// The time is the database's, read under the lock, so events from
	// several server instances are in order whatever their own clocks say.
	// If the database's clock is ever stepped back, the time holds at the
	// last event's instead of going back.
	var (
		lastSeq  *int64
		lastHash []byte
		now      time.Time
	)
	err := tx.QueryRow(ctx, `SELECT e.seq, e.hash, greatest(clock_timestamp(), e.created_at) FROM (SELECT 1) AS one
		LEFT JOIN LATERAL (SELECT seq, hash, created_at FROM mandate.events ORDER BY seq DESC LIMIT 1) AS e ON true`).Scan(&lastSeq, &lastHash, &now)
	if err != nil {
		return nil, err
	}
	var seq int64
	prevHash := genesisHash
	if lastSeq != nil {
		seq, prevHash = *lastSeq, lastHash
	}
	now = now.UTC().Truncate(time.Microsecond)
	var runId *string
	if run := actor.RunFrom(ctx); run != "" {
		runId = &run
	}
	batch := &pgx.Batch{}
	seqs := make([]int64, 0, len(events))
	for _, event := range events {
		payload, err := canonicalJson(event.Payload)
		if err != nil {
			return nil, fmt.Errorf("events: payload: %w", err)
		}
		seq++
		stored := Stored{Seq: seq, Type: event.Type, ActorKind: string(a.Kind), RunId: runId, Payload: payload, CreatedAt: now}
		if a.Id != uuid.Nil {
			stored.ActorId = &a.Id
		}
		if event.SubjectId != uuid.Nil {
			stored.SubjectId = &event.SubjectId
		}
		hash, err := chainHash(tenantId, prevHash, stored)
		if err != nil {
			return nil, err
		}
		batch.Queue(`INSERT INTO mandate.events
			(tenant_id, seq, type, actor_kind, actor_id, subject_id, run_id, payload, prev_hash, hash, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			tenantId, seq, stored.Type, stored.ActorKind, stored.ActorId, stored.SubjectId, stored.RunId, []byte(payload),
			prevHash, hash, stored.CreatedAt)
		prevHash = hash
		seqs = append(seqs, seq)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return nil, fmt.Errorf("events: insert: %w", err)
	}
	return seqs, nil
}

// ErrBroken is returned when the chain does not verify.
var ErrBroken = errors.New("event chain is broken")

// Verify recomputes the whole chain of the tenant in ctx and returns how
// many events it holds.
func Verify(ctx context.Context, tx pgx.Tx) (int64, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return 0, errors.New("events: no tenant in context")
	}
	rows, err := tx.Query(ctx, `SELECT seq, type, actor_kind, actor_id, subject_id, run_id, payload,
		prev_hash, hash, created_at FROM mandate.events ORDER BY seq`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	expectedPrev := genesisHash
	var count int64
	for rows.Next() {
		var stored Stored
		var prevHash, hash []byte
		if err := rows.Scan(&stored.Seq, &stored.Type, &stored.ActorKind, &stored.ActorId, &stored.SubjectId,
			&stored.RunId, &stored.Payload, &prevHash, &hash, &stored.CreatedAt); err != nil {
			return count, err
		}
		count++
		if stored.Seq != count || !bytes.Equal(prevHash, expectedPrev) {
			return count, fmt.Errorf("%w at seq %d", ErrBroken, stored.Seq)
		}
		if stored.Payload, err = canonicalJson(stored.Payload); err != nil {
			return count, err
		}
		want, err := chainHash(tenantId, prevHash, stored)
		if err != nil {
			return count, err
		}
		if !bytes.Equal(want, hash) {
			return count, fmt.Errorf("%w at seq %d", ErrBroken, stored.Seq)
		}
		expectedPrev = hash
	}
	return count, rows.Err()
}

// List returns the events about a subject, oldest first; with the nil
// subject, the newest events of the tenant, at most limit.
func List(ctx context.Context, tx pgx.Tx, subjectId *uuid.UUID, limit int) ([]Stored, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := tx.Query(ctx, `SELECT * FROM (
			SELECT seq, type, actor_kind, actor_id, subject_id, run_id, payload, created_at
			FROM mandate.events WHERE $1::uuid IS NULL OR subject_id = $1
			ORDER BY seq DESC LIMIT $2
		) latest ORDER BY seq`, subjectId, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stored, error) {
		var stored Stored
		err := row.Scan(&stored.Seq, &stored.Type, &stored.ActorKind, &stored.ActorId, &stored.SubjectId,
			&stored.RunId, &stored.Payload, &stored.CreatedAt)
		return stored, err
	})
}

// Last returns the newest event of a type about a subject, if any.
func Last(ctx context.Context, tx pgx.Tx, subjectId uuid.UUID, eventType string) (Stored, bool, error) {
	var stored Stored
	err := tx.QueryRow(ctx, `SELECT seq, type, actor_kind, actor_id, subject_id, run_id, payload, created_at
		FROM mandate.events WHERE subject_id = $1 AND type = $2 ORDER BY seq DESC LIMIT 1`, subjectId, eventType).
		Scan(&stored.Seq, &stored.Type, &stored.ActorKind, &stored.ActorId, &stored.SubjectId, &stored.RunId, &stored.Payload, &stored.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stored{}, false, nil
	}
	return stored, err == nil, err
}

func chainHash(tenantId uuid.UUID, prevHash []byte, stored Stored) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		TenantId  uuid.UUID       `json:"tenant"`
		Seq       int64           `json:"seq"`
		Type      string          `json:"type"`
		ActorKind string          `json:"actorKind"`
		ActorId   *uuid.UUID      `json:"actorId"`
		SubjectId *uuid.UUID      `json:"subjectId"`
		RunId     *string         `json:"runId"`
		Payload   json.RawMessage `json:"payload"`
		CreatedAt string          `json:"createdAt"`
	}{tenantId, stored.Seq, stored.Type, stored.ActorKind, stored.ActorId, stored.SubjectId, stored.RunId, stored.Payload,
		stored.CreatedAt.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(prevHash)
	h.Write(encoded)
	return h.Sum(nil), nil
}

// Canonical encodes value the way the chain hashes it.
func Canonical(value any) (json.RawMessage, error) {
	return canonicalJson(value)
}

// Digest is the hex SHA-256 of a value's canonical form. Events carry
// digests of what they are about, such as a payload, so the chain proves
// what was decided without keeping the text: the same JSON in any order
// or spacing gives the same digest, and so does the value after a round
// trip through jsonb.
func Digest(value any) (string, error) {
	canonical, err := canonicalJson(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// UrlDigest names an address in the chain without its text, which may
// carry credentials: the hex SHA-256 of the address exactly as written.
// Every event that names a connector or a notifier uses it, so an auditor
// can match the address a decision was made against to the one an action
// was defined with.
func UrlDigest(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// canonicalJson encodes value so that the result is stable across a jsonb
// round trip: object keys sorted, no insignificant whitespace, numbers in the
// plain form jsonb gives back, and no NUL characters (which jsonb refuses).
func canonicalJson(value any) (json.RawMessage, error) {
	raw, ok := value.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(value); err != nil {
			return nil, err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	return json.Marshal(normalise(generic))
}

func normalise(value any) any {
	switch v := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			result[withoutNul(key)] = normalise(item)
		}
		return result
	case []any:
		for i, item := range v {
			v[i] = normalise(item)
		}
	case string:
		return withoutNul(v)
	case json.Number:
		return json.Number(plainDecimal(string(v)))
	}
	return value
}

func withoutNul(text string) string {
	return strings.ReplaceAll(text, "\x00", "�")
}

// The most digits Postgres's numeric keeps before and after the point.
// Numbers beyond them cannot be stored in a payload anyway.
const (
	MaxIntegerDigits = 131072
	MaxDecimalDigits = 16383
)

// splitNumber splits a JSON number into its sign ("" or "-"), the digits
// before and after the point, and its exponent. ok is false when the
// exponent is not an integer or lies beyond any number Postgres can store;
// it is bounded here, so that sums over it cannot overflow.
func splitNumber(number string) (sign, whole, fraction string, exponent int, ok bool) {
	mantissa, exponentText, found := strings.Cut(strings.ToLower(number), "e")
	if rest, negative := strings.CutPrefix(mantissa, "-"); negative {
		sign, mantissa = "-", rest
	}
	if found {
		var err error
		if exponent, err = strconv.Atoi(exponentText); err != nil {
			return "", "", "", 0, false
		}
	}
	if exponent > MaxIntegerDigits || exponent < -(MaxIntegerDigits+MaxDecimalDigits) {
		return "", "", "", 0, false
	}
	whole, fraction, _ = strings.Cut(mantissa, ".")
	return sign, whole, fraction, exponent, true
}

// PlainLength is how many characters a JSON number takes once spelled out
// without an exponent, as the canonical form writes it, or -1 when it is
// not a number Postgres can store.
func PlainLength(number string) int {
	if !NumberFits(number) {
		return -1
	}
	_, whole, fraction, exponent, _ := splitNumber(number)
	digits := len(whole) + len(fraction)
	point := len(whole) + exponent
	switch {
	case point <= 0:
		return digits + 1 - point + 2
	case point > digits:
		return point + 1
	}
	return digits + 2
}

// NumberFits says whether a JSON number can be stored as Postgres keeps it.
func NumberFits(number string) bool {
	_, whole, fraction, exponent, ok := splitNumber(number)
	return ok && partsFit(whole, fraction, exponent)
}

// partsFit is NumberFits for a number already split.
func partsFit(whole, fraction string, exponent int) bool {
	point := len(strings.TrimLeft(whole, "0")) + exponent
	return point <= MaxIntegerDigits && len(fraction)-exponent <= MaxDecimalDigits
}

// plainDecimal writes a JSON number the way Postgres numeric does: without
// an exponent, with as many decimals as the number had after the exponent is
// applied (1.50e1 is 15.0), and never negative zero. The digits are moved,
// never converted through a float.
func plainDecimal(number string) string {
	// A number Postgres cannot store is left as written rather than spelled
	// out: an exponent chosen by a caller must not choose how much memory
	// this takes. NumberFits is the one test of that, the same one a payload
	// is refused by, so a number that fits is always spelled as jsonb gives
	// it back. A bound of its own here once differed from it by the zero
	// before the point, and 0.5e131072 kept its exponent.
	sign, whole, fraction, exponent, ok := splitNumber(number)
	if !ok || !partsFit(whole, fraction, exponent) {
		return number
	}
	digits := whole + fraction
	point := len(whole) + exponent
	switch {
	case point <= 0:
		digits = strings.Repeat("0", 1-point) + digits
		point = 1
	case point > len(digits):
		digits += strings.Repeat("0", point-len(digits))
	}
	integer := strings.TrimLeft(digits[:point], "0")
	if integer == "" {
		integer = "0"
	}
	plain := integer
	if decimals := digits[point:]; decimals != "" {
		plain += "." + decimals
	}
	if strings.Trim(plain, "0.") == "" {
		return plain
	}
	return sign + plain
}
