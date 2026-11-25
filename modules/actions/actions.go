// Package actions holds what agents may propose, as owners define it: a
// key, a payload schema, a default risk, a cost, whether a mandate may ever
// let it run without a person, and the connector that carries it out.
package actions

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/andreas-lindfalk/pgmandate/modules/actions/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

const (
	ConnectorWebhook = "webhook"
	ConnectorBuiltin = "builtin"

	// SecretPrefix marks connector secrets, so they are recognizable in
	// config files and by secret scanners.
	SecretPrefix = "mws_"
	secretBytes  = 24
)

type Action struct {
	Id            uuid.UUID
	Key           string
	Description   string
	Risk          string
	CostUnits     int64
	MayExecute    bool
	Schema        json.RawMessage
	ConnectorKind string
	ConnectorUrl  string
	Department    string
	CreatedBy     *uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
	RemovedAt     *time.Time
}

// HasSecret says whether the action has a signing secret: every webhook
// action does. The secret itself is shown once, when it is issued, and
// never read back by the application.
func (e Action) HasSecret() bool {
	return e.ConnectorKind == ConnectorWebhook
}

type Input struct {
	Key         string
	Description string
	Risk        string
	CostUnits   int64
	MayExecute  bool
	Schema      json.RawMessage
	// Connector is webhook or builtin.
	Connector string
	// Url is the webhook's address; empty for builtin.
	Url string
	// RotateSecret makes a new signing secret for an existing webhook.
	RotateSecret bool
	Department   string
}

type Service struct {
	pool    *pgxpool.Pool
	onMoved OnMoved
}

// OnMoved runs in the transaction that moves a webhook action to a new
// URL. The kernel uses it to move the action's waiting deliveries along.
type OnMoved func(ctx context.Context, tx pgx.Tx, actionKey, url string) error

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// SetOnMoved connects what must follow a connector that moves.
func (s *Service) SetOnMoved(onMoved OnMoved) {
	s.onMoved = onMoved
}

// Define creates or updates the action with the input's key, as an owner,
// which the database enforces. A new webhook gets a signing secret, which
// is returned once; so does a rotation. Defining a removed action again
// restores it.
func (s *Service) Define(ctx context.Context, input Input) (Action, string, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Action{}, "", fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	input.Description = strings.TrimSpace(input.Description)
	input.Url = strings.TrimSpace(input.Url)
	input.Department = strings.TrimSpace(input.Department)
	switch {
	case !mandates.ValidKey(input.Key):
		return Action{}, "", fault.New(fault.InvalidArgument, "the key is dotted lower case, such as tickets.create")
	case utf8.RuneCountInString(input.Description) > 1000:
		return Action{}, "", fault.New(fault.InvalidArgument, "the description can be at most 1000 characters")
	case mandates.RiskRank(input.Risk) < 0:
		return Action{}, "", fault.New(fault.InvalidArgument, "the risk is low, medium or high")
	case input.CostUnits < 0 || input.CostUnits > 1_000_000_000:
		return Action{}, "", fault.New(fault.InvalidArgument, "the cost is 0 or more units")
	case input.Connector != ConnectorWebhook && input.Connector != ConnectorBuiltin:
		return Action{}, "", fault.New(fault.InvalidArgument, "the connector is webhook or builtin")
	case input.Connector == ConnectorWebhook && !validUrl(input.Url):
		return Action{}, "", fault.New(fault.InvalidArgument, "a webhook needs an http(s) URL of at most 2000 characters")
	case input.Connector == ConnectorWebhook && input.MayExecute && input.Risk == mandates.RiskHigh:
		// A mandate never allows high risk, and a webhook action's
		// proposals are never below its risk: the flag could not take
		// effect, and would read as if it did.
		return Action{}, "", fault.New(fault.InvalidArgument, "a high risk webhook action is always decided by a person, so it cannot be one a mandate may allow")
	case input.Connector == ConnectorWebhook && len(input.Schema) == 0:
		// What reaches another system is held to a shape someone wrote
		// down. Accepting anything is possible, and then it is stated.
		return Action{}, "", fault.New(fault.InvalidArgument, "a webhook action needs a payload schema")
	case input.Connector == ConnectorBuiltin && input.Url != "":
		return Action{}, "", fault.New(fault.InvalidArgument, "a builtin action has no URL")
	case utf8.RuneCountInString(input.Department) > 100:
		return Action{}, "", fault.New(fault.InvalidArgument, "the department can be at most 100 characters")
	}
	if len(input.Schema) > 0 {
		if _, err := Compile(input.Schema); err != nil {
			return Action{}, "", fault.Wrap(fault.InvalidArgument, "the schema is not valid: "+err.Error(), err)
		}
	}
	var result Action
	secret := ""
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		existing, err := q.GetActionByKey(ctx, input.Key)
		created := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !created {
			return err
		}
		var secretBytes []byte
		// A new URL gets a new secret: the receiver at the old one holds
		// the old secret and must not be able to sign for the new one.
		moved := !created && (existing.ConnectorUrl == nil || *existing.ConnectorUrl != input.Url)
		// A webhook always has a secret (the table requires it), so one that
		// was a webhook before and stays at its address keeps its secret.
		if input.Connector == ConnectorWebhook && (created || moved || input.RotateSecret || existing.ConnectorKind != ConnectorWebhook) {
			if secret, err = newSecret(); err != nil {
				return err
			}
			secretBytes = []byte(secret)
		}
		var url, department *string
		if input.Url != "" {
			url = &input.Url
		}
		if input.Department != "" {
			department = &input.Department
		}
		var schema []byte
		if len(input.Schema) > 0 {
			schema = input.Schema
		}
		id := uuid.New()
		eventType := "action.defined"
		if created {
			if err := q.InsertAction(ctx, repo.InsertActionParams{
				Id: id, TenantId: tenantId, Key: input.Key, Description: input.Description, Risk: input.Risk, CostUnits: input.CostUnits,
				MayExecute: input.MayExecute, Schema: schema, ConnectorKind: input.Connector, ConnectorUrl: url, ConnectorSecret: secretBytes, Department: department,
			}); err != nil {
				return err
			}
		} else {
			id = existing.Id
			eventType = "action.changed"
			if input.Connector == ConnectorBuiltin {
				// A builtin action has no secret; a stale one must not linger.
				secretBytes = nil
			}
			// The secret is written when one is issued, and cleared for a
			// builtin action; it is never read back.
			if secretBytes != nil || input.Connector == ConnectorBuiltin {
				err = q.UpdateActionWithSecret(ctx, repo.UpdateActionWithSecretParams{
					Id: id, Description: input.Description, Risk: input.Risk, CostUnits: input.CostUnits, MayExecute: input.MayExecute,
					Schema: schema, ConnectorKind: input.Connector, ConnectorUrl: url, ConnectorSecret: secretBytes, Department: department,
				})
			} else {
				err = q.UpdateAction(ctx, repo.UpdateActionParams{
					Id: id, Description: input.Description, Risk: input.Risk, CostUnits: input.CostUnits, MayExecute: input.MayExecute,
					Schema: schema, ConnectorKind: input.Connector, ConnectorUrl: url, Department: department,
				})
			}
			if err != nil {
				return err
			}
			// Deliveries still waiting would go to the old address, signed
			// with a secret the receiver there no longer holds.
			if moved && input.Connector == ConnectorWebhook && s.onMoved != nil {
				if err := s.onMoved(ctx, tx, input.Key, input.Url); err != nil {
					return err
				}
			}
		}
		row, err := q.GetActionByKey(ctx, input.Key)
		if err != nil {
			return err
		}
		result = toAction(row)
		// Where an action delivers and what it accepts are what an auditor
		// most needs to see change; the chain holds their digests.
		payload := map[string]any{
			"key": input.Key, "risk": input.Risk, "costUnits": input.CostUnits, "mayExecute": input.MayExecute,
			"connector": input.Connector, "secretRotated": secret != "", "department": department,
		}
		if input.Url != "" {
			sum := sha256.Sum256([]byte(input.Url))
			payload["connectorUrlSha256"] = hex.EncodeToString(sum[:])
		}
		if len(input.Schema) > 0 {
			if payload["schemaSha256"], err = events.Digest(input.Schema); err != nil {
				return err
			}
		}
		_, err = events.Append(ctx, tx, events.Event{Type: eventType, SubjectId: id, Payload: payload})
		return err
	})
	if err != nil {
		return Action{}, "", fault.FromDb(err)
	}
	return result, secret, nil
}

// Remove hides an action: agents can no longer propose it, old proposals
// still read. Defining it again restores it. Only an owner may; an action
// removed already is a Conflict.
func (s *Service) Remove(ctx context.Context, key string) error {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		row, err := q.GetActionByKey(ctx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "no such action")
		}
		if err != nil {
			return err
		}
		changed, err := q.RemoveAction(ctx, key)
		if err != nil {
			return err
		}
		if changed == 0 {
			return fault.New(fault.Conflict, "the action is already removed")
		}
		_, err = events.Append(ctx, tx, events.Event{Type: "action.removed", SubjectId: row.Id, Payload: map[string]any{"key": key}})
		return err
	})
	return fault.FromDb(err)
}

// Get returns the action with the key, removed or not.
func (s *Service) Get(ctx context.Context, key string) (Action, error) {
	var result Action
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		result, err = s.GetIn(ctx, tx, key)
		return err
	})
	return result, err
}

// GetIn is Get inside the caller's transaction, for code that already
// holds one and must not wait for a second connection.
func (s *Service) GetIn(ctx context.Context, tx pgx.Tx, key string) (Action, error) {
	row, err := repo.New(tx).GetActionByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, fault.New(fault.NotFound, "no such action")
	}
	if err != nil {
		return Action{}, err
	}
	return toAction(row), nil
}

func (s *Service) List(ctx context.Context, includeRemoved bool) ([]Action, error) {
	var result []Action
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListActions(ctx, includeRemoved)
		for _, row := range rows {
			result = append(result, toAction(repo.GetActionByKeyRow(row)))
		}
		return err
	})
	return result, err
}

// Compile compiles a payload schema, asserting formats such as uuid.
func Compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("schema.json", doc); err != nil {
		return nil, err
	}
	return compiler.Compile("schema.json")
}

func validUrl(url string) bool {
	return len(url) <= 2000 && (strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")) && len(url) > 8
}

func newSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func toAction(row repo.GetActionByKeyRow) Action {
	e := Action{
		Id: row.Id, Key: row.Key, Description: row.Description, Risk: row.Risk, CostUnits: row.CostUnits, MayExecute: row.MayExecute,
		ConnectorKind: row.ConnectorKind, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, RemovedAt: row.RemovedAt,
	}
	if len(row.Schema) > 0 {
		e.Schema = json.RawMessage(row.Schema)
	}
	if row.ConnectorUrl != nil {
		e.ConnectorUrl = *row.ConnectorUrl
	}
	if row.Department != nil {
		e.Department = *row.Department
	}
	return e
}
