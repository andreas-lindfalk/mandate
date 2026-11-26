// Package actions holds what agents may propose, as owners define it: a
// key, a payload schema, a default risk, how long its proposals may wait, whether a mandate may ever
// let it run without a person, and the connector that carries it out.
package actions

import (
	"bytes"
	"context"
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
)

// How long a proposal may wait for a decision, and how long after a decision
// its delivery may be carried out: an action's defaults and their bounds,
// which the database holds too.
const (
	DefaultWaitLimit     = 7 * 24 * time.Hour
	MinWaitLimit         = time.Minute
	MaxWaitLimit         = 30 * 24 * time.Hour
	DefaultDeliverWithin = time.Hour
	MinDeliverWithin     = 2 * time.Minute
	MaxDeliverWithin     = 7 * 24 * time.Hour
)

type Action struct {
	Id          uuid.UUID
	Key         string
	Description string
	Risk        string
	// WaitLimit is how long a proposal for it may wait for a decision, and
	// DeliverWithin how long after a decision its delivery may be carried
	// out. Both are fixed on each proposal when it is made.
	WaitLimit     time.Duration
	DeliverWithin time.Duration
	MayExecute    bool
	Schema        json.RawMessage
	ConnectorKind string
	ConnectorUrl  string
	// ConnectorAudience is who a webhook's deliveries are for. The receiver
	// is configured with it.
	ConnectorAudience string
	Department        string
	CreatedBy         *uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
	RemovedAt         *time.Time
}

type Input struct {
	Key         string
	Description string
	Risk        string
	// WaitLimit and DeliverWithin default to DefaultWaitLimit and
	// DefaultDeliverWithin when zero.
	WaitLimit     time.Duration
	DeliverWithin time.Duration
	MayExecute    bool
	Schema        json.RawMessage
	// Connector is webhook or builtin.
	Connector string
	// Url is the webhook's address; empty for builtin.
	Url        string
	Department string
}

type Service struct {
	pool    *pgxpool.Pool
	onMoved OnMoved
}

// OnMoved runs in the transaction that moves a webhook action to a new
// URL. The kernel uses it to move the action's waiting deliveries along.
type OnMoved func(ctx context.Context, tx pgx.Tx, actionKey, url, audience string) error

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// SetOnMoved connects what must follow a connector that moves.
func (s *Service) SetOnMoved(onMoved OnMoved) {
	s.onMoved = onMoved
}

// Define creates or updates the action with the input's key, as an owner,
// which the database enforces. A webhook gets an audience, drawn by the
// database: a new one when the webhook is new, moves, or is defined again
// after removal. Defining a removed action again restores it.
func (s *Service) Define(ctx context.Context, input Input) (Action, error) {
	if input.WaitLimit == 0 {
		input.WaitLimit = DefaultWaitLimit
	}
	if input.DeliverWithin == 0 {
		input.DeliverWithin = DefaultDeliverWithin
	}
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Action{}, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	input.Description = strings.TrimSpace(input.Description)
	input.Url = strings.TrimSpace(input.Url)
	input.Department = strings.TrimSpace(input.Department)
	switch {
	case !mandates.ValidKey(input.Key):
		return Action{}, fault.New(fault.InvalidArgument, "the key is dotted lower case, such as tickets.create")
	case utf8.RuneCountInString(input.Description) > 1000:
		return Action{}, fault.New(fault.InvalidArgument, "the description can be at most 1000 characters")
	case mandates.RiskRank(input.Risk) < 0:
		return Action{}, fault.New(fault.InvalidArgument, "the risk is low, medium or high")
	case input.WaitLimit < MinWaitLimit || input.WaitLimit > MaxWaitLimit || input.WaitLimit%time.Second != 0:
		return Action{}, fault.New(fault.InvalidArgument, "a proposal may wait between 1 minute and 30 days, in whole seconds")
	case input.DeliverWithin < MinDeliverWithin || input.DeliverWithin > MaxDeliverWithin || input.DeliverWithin%time.Second != 0:
		return Action{}, fault.New(fault.InvalidArgument, "a decision may be delivered within 2 minutes to 7 days, in whole seconds")
	case input.Connector != ConnectorWebhook && input.Connector != ConnectorBuiltin:
		return Action{}, fault.New(fault.InvalidArgument, "the connector is webhook or builtin")
	case input.Connector == ConnectorWebhook && !validUrl(input.Url):
		return Action{}, fault.New(fault.InvalidArgument, "a webhook needs an http(s) URL of at most 2000 characters")
	case input.Connector == ConnectorWebhook && input.MayExecute && input.Risk == mandates.RiskHigh:
		// A mandate never allows high risk, and a webhook action's
		// proposals are never below its risk: the flag could not take
		// effect, and would read as if it did.
		return Action{}, fault.New(fault.InvalidArgument, "a high risk webhook action is always decided by a person, so it cannot be one a mandate may allow")
	case input.Connector == ConnectorWebhook && len(input.Schema) == 0:
		// What reaches another system is held to a shape someone wrote
		// down. Accepting anything is possible, and then it is stated.
		return Action{}, fault.New(fault.InvalidArgument, "a webhook action needs a payload schema")
	case input.Connector == ConnectorBuiltin && input.Url != "":
		return Action{}, fault.New(fault.InvalidArgument, "a builtin action has no URL")
	case utf8.RuneCountInString(input.Department) > 100:
		return Action{}, fault.New(fault.InvalidArgument, "the department can be at most 100 characters")
	}
	if len(input.Schema) > 0 {
		if _, err := Compile(input.Schema); errors.Is(err, ErrExternalRef) {
			return Action{}, fault.New(fault.InvalidArgument, "the schema is not valid: "+ErrExternalRef.Error())
		} else if err != nil {
			return Action{}, fault.Wrap(fault.InvalidArgument, "the schema is not valid: "+err.Error(), err)
		}
	}
	var result Action
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		existing, err := q.GetActionByKey(ctx, input.Key)
		created := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !created {
			return err
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
				Id: id, TenantId: tenantId, Key: input.Key, Description: input.Description, Risk: input.Risk,
				WaitSeconds: int64(input.WaitLimit / time.Second), DeliverSeconds: int64(input.DeliverWithin / time.Second),
				MayExecute: input.MayExecute, Schema: schema, ConnectorKind: input.Connector, ConnectorUrl: url, Department: department,
			}); err != nil {
				return err
			}
		} else {
			id = existing.Id
			eventType = "action.changed"
			if err := q.UpdateAction(ctx, repo.UpdateActionParams{
				Id: id, Description: input.Description, Risk: input.Risk, MayExecute: input.MayExecute,
				WaitSeconds: int64(input.WaitLimit / time.Second), DeliverSeconds: int64(input.DeliverWithin / time.Second),
				Schema: schema, ConnectorKind: input.Connector, ConnectorUrl: url, Department: department,
			}); err != nil {
				return err
			}
		}
		row, err := q.GetActionByKey(ctx, input.Key)
		if err != nil {
			return err
		}
		result = toAction(row)
		// A new audience means the receiver of the old one must get nothing
		// more: deliveries still waiting, and never sent, move along.
		if !created && result.ConnectorKind == ConnectorWebhook && s.onMoved != nil &&
			(existing.ConnectorAudience == nil || *existing.ConnectorAudience != result.ConnectorAudience) {
			if err := s.onMoved(ctx, tx, input.Key, result.ConnectorUrl, result.ConnectorAudience); err != nil {
				return err
			}
		}
		// Where an action delivers and what it accepts are what an auditor
		// most needs to see change; the chain holds their digests.
		payload := map[string]any{
			"key": input.Key, "risk": input.Risk, "waitSeconds": int64(input.WaitLimit / time.Second),
			"deliverSeconds": int64(input.DeliverWithin / time.Second), "mayExecute": input.MayExecute,
			"connector": input.Connector, "audience": result.ConnectorAudience, "department": department,
		}
		if input.Url != "" {
			payload["connectorUrlSha256"] = events.UrlDigest(input.Url)
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
		return Action{}, fault.FromDb(err)
	}
	return result, nil
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

// ErrExternalRef is what Compile returns for a schema that refers to
// anything but itself.
var ErrExternalRef = errors.New("a schema may refer only to itself, such as #/$defs/name")

// noLoader refuses every URL. The library's default loader reads files,
// so a $ref in a schema an owner writes would open paths on the server,
// and the schema would mean what those files say on that replica.
// Meta-schemas named by $schema are embedded and never reach a loader.
type noLoader struct{}

// schemaId is the id a payload schema compiles under. It is not a file
// URL, so errors do not name the server's directory, and it has a path,
// so a relative ref resolves to another document that noLoader refuses
// rather than (as under a URN) to the schema itself.
const schemaId = "pgmandate:///action-schema.json"

func (noLoader) Load(string) (any, error) {
	return nil, ErrExternalRef
}

// Compile compiles a payload schema, asserting formats such as uuid. A
// schema refers only to itself: any other $ref, or a $schema that is not
// a known draft, is ErrExternalRef.
func Compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	compiler.UseLoader(noLoader{})
	// A relative id would resolve against the server's working directory,
	// which errors would then name and relative refs would point into.
	if err := compiler.AddResource(schemaId, doc); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(schemaId)
	// The library's error names the URL it could not load.
	if loadErr := (*jsonschema.LoadURLError)(nil); errors.As(err, &loadErr) {
		return nil, ErrExternalRef
	}
	return compiled, err
}

func validUrl(url string) bool {
	return len(url) <= 2000 && (strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")) && len(url) > 8
}

func toAction(row repo.GetActionByKeyRow) Action {
	e := Action{
		Id: row.Id, Key: row.Key, Description: row.Description, Risk: row.Risk, MayExecute: row.MayExecute,
		WaitLimit: time.Duration(row.WaitSeconds) * time.Second, DeliverWithin: time.Duration(row.DeliverSeconds) * time.Second,
		ConnectorKind: row.ConnectorKind, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, RemovedAt: row.RemovedAt,
	}
	if len(row.Schema) > 0 {
		e.Schema = json.RawMessage(row.Schema)
	}
	if row.ConnectorUrl != nil {
		e.ConnectorUrl = *row.ConnectorUrl
	}
	if row.ConnectorAudience != nil {
		e.ConnectorAudience = *row.ConnectorAudience
	}
	if row.Department != nil {
		e.Department = *row.Department
	}
	return e
}
