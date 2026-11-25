package actions_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type ActionsSuite struct {
	IntegrationSuite
}

func TestActionsSuite(t *testing.T) {
	suite.Run(t, new(ActionsSuite))
}

var ticketSchema = json.RawMessage(`{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`)

func webhook(key string) actions.Input {
	return actions.Input{Key: key, Description: "Open a ticket", Risk: mandates.RiskLow, MayExecute: true,
		Schema: ticketSchema, Connector: actions.ConnectorWebhook, Url: "https://helpdesk.example/hooks/mandate"}
}

func (s *ActionsSuite) Test_OwnerDefinesWebhookActions() {
	t := s.W.NewTenant(s.T(), "Acme")
	defined, err := s.W.Kernel.Actions.Define(t.Owner, webhook("tickets.create"))
	s.Require().NoError(err)
	audience := defined.ConnectorAudience
	s.Require().Regexp(`^pgm:`+t.Id.String()+`:tickets\.create:[0-9a-f]{16}$`, audience, "the database draws the audience")
	s.Require().Equal(&t.OwnerId, defined.CreatedBy)
	s.Require().Equal("https://helpdesk.example/hooks/mandate", defined.ConnectorUrl)
	s.Require().JSONEq(string(ticketSchema), string(defined.Schema))
	s.Require().Equal([]string{"action.defined"}, s.eventTypes(t.Owner, defined.Id))

	// Defining again at the same address changes it and keeps the audience.
	changed := webhook("tickets.create")
	changed.WaitLimit = 48 * time.Hour
	changed.DeliverWithin = 10 * time.Minute
	again, err := s.W.Kernel.Actions.Define(t.Owner, changed)
	s.Require().NoError(err)
	s.Require().Equal(defined.Id, again.Id)
	s.Require().Equal(48*time.Hour, again.WaitLimit)
	s.Require().Equal(10*time.Minute, again.DeliverWithin)
	s.Require().Equal(audience, again.ConnectorAudience, "defining it again at the same address keeps the audience")
	// A new address gets a new audience, so the old receiver accepts
	// nothing meant for the new one.
	moved := changed
	moved.Url = "https://helpdesk.example/hooks/elsewhere"
	movedAction, err := s.W.Kernel.Actions.Define(t.Owner, moved)
	s.Require().NoError(err)
	s.Require().NotEqual(audience, movedAction.ConnectorAudience, "moving the connector draws a new audience")
	s.Require().Regexp(`^pgm:`+t.Id.String()+`:tickets\.create:[0-9a-f]{16}$`, movedAction.ConnectorAudience)
	s.Require().Equal("https://helpdesk.example/hooks/elsewhere", movedAction.ConnectorUrl)
	s.Require().Equal([]string{"action.defined", "action.changed", "action.changed"}, s.eventTypes(t.Owner, defined.Id))

	// The chain shows where it delivers, to whom and what it accepts.
	var logged []events.Stored
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		var err error
		logged, err = events.List(t.Owner, tx, &defined.Id, 0)
		return err
	}))
	urlDigest := func(url string) string {
		sum := sha256.Sum256([]byte(url))
		return hex.EncodeToString(sum[:])
	}
	schemaDigest, err := events.Digest(ticketSchema)
	s.Require().NoError(err)
	var first, third map[string]any
	s.Require().NoError(json.Unmarshal(logged[0].Payload, &first))
	s.Require().NoError(json.Unmarshal(logged[2].Payload, &third))
	s.Require().Equal(urlDigest("https://helpdesk.example/hooks/mandate"), first["connectorUrlSha256"])
	s.Require().Equal(schemaDigest, first["schemaSha256"])
	s.Require().Equal(audience, first["audience"])
	s.Require().Equal(urlDigest("https://helpdesk.example/hooks/elsewhere"), third["connectorUrlSha256"], "a moved connector shows in the chain")
	s.Require().Equal(movedAction.ConnectorAudience, third["audience"])

	list, err := s.W.Kernel.Actions.List(t.Reviewer, false)
	s.Require().NoError(err)
	s.Require().Len(list, 1)

	s.Require().NoError(s.W.Kernel.Actions.Remove(t.Owner, "tickets.create"))
	s.code(s.W.Kernel.Actions.Remove(t.Owner, "tickets.create"), fault.Conflict)
	s.code(s.W.Kernel.Actions.Remove(t.Reviewer, "tickets.create"), fault.PermissionDenied)
	list, err = s.W.Kernel.Actions.List(t.Owner, false)
	s.Require().NoError(err)
	s.Require().Empty(list)
	all, err := s.W.Kernel.Actions.List(t.Owner, true)
	s.Require().NoError(err)
	s.Require().Len(all, 1)
	s.Require().NotNil(all[0].RemovedAt)
	restored, err := s.W.Kernel.Actions.Define(t.Owner, moved)
	s.Require().NoError(err)
	s.Require().Nil(restored.RemovedAt, "defining a removed action restores it")
	s.Require().Equal(moved.Url, restored.ConnectorUrl)
	s.Require().NotEqual(movedAction.ConnectorAudience, restored.ConnectorAudience, "an action defined anew after removal gets a new audience, even at the same address")
	s.code(s.W.Kernel.Actions.Remove(t.Owner, "nope.nothing"), fault.NotFound)

	// A builtin definition has no URL and no audience.
	builtin := actions.Input{Key: "test.low", Description: "", Risk: mandates.RiskMedium, Connector: actions.ConnectorBuiltin}
	b, err := s.W.Kernel.Actions.Define(t.Owner, builtin)
	s.Require().NoError(err)
	s.Require().Empty(b.ConnectorAudience)
	// Turning a webhook into a builtin drops its audience, and back into a
	// webhook at the same address draws a new one.
	asBuiltin := builtin
	asBuiltin.Key = "tickets.create"
	b, err = s.W.Kernel.Actions.Define(t.Owner, asBuiltin)
	s.Require().NoError(err)
	s.Require().Empty(b.ConnectorAudience)
	s.Require().Empty(b.ConnectorUrl)
	back, err := s.W.Kernel.Actions.Define(t.Owner, moved)
	s.Require().NoError(err)
	s.Require().NotEmpty(back.ConnectorAudience)
	s.Require().NotEqual(restored.ConnectorAudience, back.ConnectorAudience)
}

// audienceOf reads an action's audience as it is stored.
func (s *ActionsSuite) audienceOf(t testfix.Tenant, key string) *string {
	s.T().Helper()
	var audience *string
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Owner, `SELECT connector_audience FROM mandate.actions WHERE key = $1`, key).Scan(&audience)
	}))
	return audience
}

// The audience is the database's to draw: whatever an owner writes in SQL,
// the trigger draws it or keeps it, and a receiver can rely on it never
// repeating.
func (s *ActionsSuite) Test_NoOneChoosesTheAudience() {
	t := s.W.NewTenant(s.T(), "Acme")
	defined, err := s.W.Kernel.Actions.Define(t.Owner, webhook("tickets.create"))
	s.Require().NoError(err)
	audience := defined.ConnectorAudience
	const chosen = "pgm:00000000-0000-0000-0000-000000000000:tickets.create:0123456789abcdef"

	// Set directly, it is kept as it was.
	s.Require().NoError(s.exec(t.Owner, `UPDATE mandate.actions SET connector_audience = $1 WHERE key = 'tickets.create'`, chosen))
	s.Require().Equal(audience, *s.audienceOf(t, "tickets.create"))
	// Set along with a move, a new one is drawn, not the one chosen.
	s.Require().NoError(s.exec(t.Owner, `UPDATE mandate.actions SET connector_url = 'https://elsewhere.example/hooks', connector_audience = $1 WHERE key = 'tickets.create'`, chosen))
	movedTo := *s.audienceOf(t, "tickets.create")
	s.Require().NotEqual(chosen, movedTo)
	s.Require().NotEqual(audience, movedTo)
	// Set on insert, it is drawn anyway.
	s.Require().NoError(s.exec(t.Owner, `INSERT INTO mandate.actions (id, tenant_id, key, description, risk, connector_kind, connector_url, connector_audience, schema)
		VALUES (gen_random_uuid(), $1, 'x.y', '', 'low', 'webhook', 'https://x.example/hooks', $2, '{"type":"object"}')`, t.Id, chosen))
	inserted := *s.audienceOf(t, "x.y")
	s.Require().NotEqual(chosen, inserted)
	s.Require().Regexp(`^pgm:`+t.Id.String()+`:x\.y:[0-9a-f]{16}$`, inserted)
	// Set on a builtin, it is cleared.
	s.Require().NoError(s.exec(t.Owner, `UPDATE mandate.actions SET connector_kind = 'builtin', connector_url = NULL, connector_audience = $1 WHERE key = 'x.y'`, chosen))
	s.Require().Nil(s.audienceOf(t, "x.y"))
	// Only an owner writes the table at all.
	for name, ctx := range map[string]context.Context{"reviewer": t.Reviewer, "agent": t.Agent} {
		s.refused(ctx, `UPDATE mandate.actions SET connector_url = 'https://evil.example/hooks' WHERE key = 'tickets.create'`)
		s.T().Log(name, "cannot move a connector")
	}
}

func (s *ActionsSuite) Test_DefineValidation() {
	t := s.W.NewTenant(s.T(), "Acme")
	for name, change := range map[string]func(*actions.Input){
		"bad key":        func(i *actions.Input) { i.Key = "Tickets.Create" },
		"bad risk":       func(i *actions.Input) { i.Risk = "severe" },
		"wait too short": func(i *actions.Input) { i.WaitLimit = 30 * time.Second },
		"wait too long":  func(i *actions.Input) { i.WaitLimit = 31 * 24 * time.Hour },
		"wait fraction":  func(i *actions.Input) { i.WaitLimit = 90*time.Second + time.Millisecond },
		"deliver short":  func(i *actions.Input) { i.DeliverWithin = time.Minute },
		"deliver long":   func(i *actions.Input) { i.DeliverWithin = 8 * 24 * time.Hour },
		"bad connector":  func(i *actions.Input) { i.Connector = "smtp" },
		"no url":         func(i *actions.Input) { i.Url = "" },
		"bad url":        func(i *actions.Input) { i.Url = "ftp://x" },
		"url on builtin": func(i *actions.Input) { i.Connector = actions.ConnectorBuiltin },
		"bad schema":     func(i *actions.Input) { i.Schema = json.RawMessage(`{"type":`) },
		"no schema":      func(i *actions.Input) { i.Schema = nil },
		"high delegable": func(i *actions.Input) { i.Risk = mandates.RiskHigh; i.MayExecute = true },
		"long desc":      func(i *actions.Input) { i.Description = strings.Repeat("x", 1001) },
	} {
		input := webhook("tickets.create")
		change(&input)
		_, err := s.W.Kernel.Actions.Define(t.Owner, input)
		s.code(err, fault.InvalidArgument)
		s.T().Log(name, "refused")
	}
}

func (s *ActionsSuite) Test_OnlyOwnersAndTheDatabaseAgrees() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Actions.Define(t.Reviewer, webhook("tickets.create"))
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Actions.Define(t.Agent, webhook("tickets.create"))
	s.code(err, fault.PermissionDenied)
	defined, err := s.W.Kernel.Actions.Define(t.Owner, webhook("tickets.create"))
	s.Require().NoError(err)
	s.code(s.W.Kernel.Actions.Remove(t.Reviewer, "tickets.create"), fault.PermissionDenied)
	s.refused(t.Owner, `UPDATE mandate.actions SET key = 'tickets.other' WHERE id = $1`, defined.Id)
	s.refused(t.Owner, `DELETE FROM mandate.actions WHERE id = $1`, defined.Id)
	s.refused(t.Reviewer, `UPDATE mandate.actions SET wait_limit = interval '1 day' WHERE id = $1`, defined.Id)
	s.invalid(t.Owner, `UPDATE mandate.actions SET wait_limit = interval '31 days' WHERE id = $1`, defined.Id)
	s.invalid(t.Owner, `UPDATE mandate.actions SET deliver_within = interval '1 minute' WHERE id = $1`, defined.Id)
	s.refused(t.Reviewer, `INSERT INTO mandate.actions (id, tenant_id, key, description, risk, connector_kind) VALUES (gen_random_uuid(), $1, 'x.y', '', 'low', 'builtin')`, t.Id)
	s.invalid(t.Owner, `INSERT INTO mandate.actions (id, tenant_id, key, description, risk, connector_kind) VALUES (gen_random_uuid(), $1, 'x.y', '', 'low', 'webhook')`, t.Id)
	// A webhook action without a payload schema is refused by the table
	// itself, and the same row with one is accepted.
	const insertWebhook = `INSERT INTO mandate.actions (id, tenant_id, key, description, risk, connector_kind, connector_url, schema)
		VALUES (gen_random_uuid(), $1, 'x.y', '', 'low', 'webhook', 'https://x.example/hooks', $2::jsonb)`
	s.invalid(t.Owner, insertWebhook, t.Id, nil)
	s.invalid(t.Owner, insertWebhook, t.Id, `null`)
	s.invalid(t.Owner, insertWebhook, t.Id, `[]`)
	s.Require().NoError(s.exec(t.Owner, insertWebhook, t.Id, `{"type":"object"}`))
	s.invalid(t.Owner, `UPDATE mandate.actions SET schema = NULL WHERE tenant_id = $1 AND key = 'x.y'`, t.Id)
}

func (s *ActionsSuite) Test_AnotherTenantSeesNoActions() {
	a := s.W.NewTenant(s.T(), "A")
	b := s.W.NewTenant(s.T(), "B")
	defined, err := s.W.Kernel.Actions.Define(a.Owner, webhook("tickets.create"))
	s.Require().NoError(err)
	list, err := s.W.Kernel.Actions.List(b.Owner, true)
	s.Require().NoError(err)
	s.Require().Empty(list)
	_, err = s.W.Kernel.Actions.Get(b.Owner, "tickets.create")
	s.code(err, fault.NotFound)
	s.code(s.W.Kernel.Actions.Remove(b.Owner, "tickets.create"), fault.NotFound)
	s.Require().NoError(s.exec(b.Owner, `UPDATE mandate.actions SET removed_at = now() WHERE id = $1`, defined.Id), "invisible rows are not updated")
	still, err := s.W.Kernel.Actions.Get(a.Owner, "tickets.create")
	s.Require().NoError(err)
	s.Require().Nil(still.RemovedAt)
	theirs, err := s.W.Kernel.Actions.Define(b.Owner, webhook("tickets.create"))
	s.Require().NoError(err)
	s.Require().NotEqual(defined.Id, theirs.Id, "the same key in two tenants is two actions")
}
