// Package helpdesk is the example host: a ticket system whose actions an
// agent may propose. It shows what a host provides: a migration whose
// tables name their proposal and are guarded by mandate.may_write_for, and
// actions with a schema, a risk and the code that applies them.
package helpdesk

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andreas-lindfalk/pgmandate"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

//go:embed schema/*.sql
var schemaFs embed.FS

// Migrations is the helpdesk's own schema. Apply it after mandate's.
var Migrations = db.Migrations{Name: "helpdesk", Fs: schemaFs, Dir: "schema"}

// LargeRefund is the amount from which a refund is high risk.
const LargeRefund = 100

type Ticket struct {
	Id             uuid.UUID
	LastProposalId uuid.UUID
	Subject        string
	Body           string
	Priority       string
	Status         string
	Resolution     string
	RefundAmount   *float64
	CreatedAt      time.Time
	ClosedAt       *time.Time
}

type createPayload struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Priority string `json:"priority"`
}

type closePayload struct {
	TicketId   uuid.UUID `json:"ticket_id"`
	Resolution string    `json:"resolution"`
}

type refundPayload struct {
	TicketId uuid.UUID `json:"ticket_id"`
	Amount   float64   `json:"amount"`
	Reason   string    `json:"reason"`
}

// Actions are what the helpdesk lets agents propose.
func Actions() []pgmandate.Action {
	return []pgmandate.Action{
		{
			Key: "tickets.create", Description: "Open a ticket.", Risk: pgmandate.RiskLow, MayExecute: true,
			Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["subject","body"],"properties":{
				"subject":{"type":"string","minLength":1,"maxLength":200},
				"body":{"type":"string","maxLength":5000},
				"priority":{"type":"string","enum":["low","normal","high"],"default":"normal"}}}`),
			Apply: createTicket,
		},
		{
			Key: "tickets.close", Description: "Close a ticket with a resolution.", Risk: pgmandate.RiskMedium, MayExecute: true,
			Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["ticket_id","resolution"],"properties":{
				"ticket_id":{"type":"string","format":"uuid"},
				"resolution":{"type":"string","minLength":1,"maxLength":2000}}}`),
			Apply: closeTicket,
		},
		{
			Key: "refunds.issue", Description: fmt.Sprintf("Refund the customer of a ticket. Medium risk below %d, high from %d: a person always decides large refunds.", LargeRefund, LargeRefund),
			Risk: pgmandate.RiskHigh, MayExecute: true,
			Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["ticket_id","amount","reason"],"properties":{
				"ticket_id":{"type":"string","format":"uuid"},
				"amount":{"type":"number","exclusiveMinimum":0},
				"reason":{"type":"string","minLength":1,"maxLength":1000}}}`),
			Assess: func(payload json.RawMessage) string {
				var p refundPayload
				if err := json.Unmarshal(payload, &p); err != nil || p.Amount >= LargeRefund {
					return pgmandate.RiskHigh
				}
				return pgmandate.RiskMedium
			},
			Apply: refund,
		},
	}
}

func createTicket(ctx context.Context, tx pgx.Tx, proposal pgmandate.Proposal) error {
	var p createPayload
	if err := json.Unmarshal(proposal.Payload, &p); err != nil {
		return err
	}
	if p.Priority == "" {
		p.Priority = "normal"
	}
	_, err := tx.Exec(ctx, `INSERT INTO helpdesk_tickets (id, tenant_id, last_proposal_id, subject, body, priority)
		VALUES ($1, mandate.current_tenant(), $2, $3, $4, $5)`, uuid.New(), proposal.Id, p.Subject, p.Body, p.Priority)
	return err
}

func closeTicket(ctx context.Context, tx pgx.Tx, proposal pgmandate.Proposal) error {
	var p closePayload
	if err := json.Unmarshal(proposal.Payload, &p); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE helpdesk_tickets SET status = 'closed', resolution = $2, closed_at = now(), last_proposal_id = $3
		WHERE id = $1 AND status = 'open'`, p.TicketId, p.Resolution, proposal.Id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fault.New(fault.NotFound, "no open ticket with that id")
	}
	return nil
}

func refund(ctx context.Context, tx pgx.Tx, proposal pgmandate.Proposal) error {
	var p refundPayload
	if err := json.Unmarshal(proposal.Payload, &p); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE helpdesk_tickets SET refund_amount = $2, last_proposal_id = $3
		WHERE id = $1 AND refund_amount IS NULL`, p.TicketId, p.Amount, proposal.Id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fault.New(fault.NotFound, "no ticket without a refund with that id")
	}
	return nil
}

// ListTickets returns the tickets of the tenant in scope, newest first.
func ListTickets(ctx context.Context, pool *pgxpool.Pool) ([]Ticket, error) {
	var result []Ticket
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, last_proposal_id, subject, body, priority, status, coalesce(resolution, ''),
			refund_amount, created_at, closed_at FROM helpdesk_tickets ORDER BY created_at DESC, id`)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ticket, error) {
			var t Ticket
			err := row.Scan(&t.Id, &t.LastProposalId, &t.Subject, &t.Body, &t.Priority, &t.Status, &t.Resolution, &t.RefundAmount, &t.CreatedAt, &t.ClosedAt)
			return t, err
		})
		return err
	})
	return result, err
}
