// Package refunds is an example of the system on the other side of a
// mandate: it carries out a decided refund, and it shows what every
// receiver of an action that moves value should do.
//
// A proposal's payload is the agent's claim. A mandate's condition, such as
// payload.amountCents <= 5000 && payload.daysSinceDelivery <= 14, decides on
// that claim, and the database checks the condition on the payload as the
// agent wrote it. Neither knows whether the claim is true. So the receiver
// reads every fact the payload states from its own records before it acts,
// and refuses when one does not match: then nothing happens, the delivery
// ends refused, and a person and the agent see why. The refund is done once
// per proposal, however often it is delivered.
//
// It also refuses a refund once the delivery's notAfter has passed, even in
// the middle of its own work: a refund made later than that could be made
// twice, after a person settled the delivery as not done.
package refunds

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

// Action is the action key this receiver carries out.
const Action = "refunds.issue"

// Schema is the action's payload schema, to define the action with.
const Schema = `{
  "type": "object",
  "required": ["orderId", "amountCents", "currency", "daysSinceDelivery"],
  "additionalProperties": false,
  "properties": {
    "orderId": {"type": "string", "minLength": 1, "maxLength": 64, "description": "The order's id, as the shop shows it."},
    "amountCents": {"type": "integer", "minimum": 1, "description": "The amount to refund, in cents of the order's currency."},
    "currency": {"type": "string", "enum": ["EUR", "SEK", "USD"], "description": "The currency the order was paid in."},
    "daysSinceDelivery": {"type": "integer", "minimum": 0, "description": "Whole days from the delivery until now, in UTC, rounded down."}
  }
}`

// Request is the payload: what the agent proposes, and the facts it claims.
type Request struct {
	OrderId     string `json:"orderId"`
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
	// DaysSinceDelivery is a claim a condition may rest on; the receiver
	// works it out again from the order.
	DaysSinceDelivery int `json:"daysSinceDelivery"`
}

type Order struct {
	Id            string
	PaidCents     int64
	Currency      string
	DeliveredAt   time.Time
	RefundedCents int64
}

// Refund is the result the agent and the person get back.
type Refund struct {
	RefundId    string `json:"refundId"`
	OrderId     string `json:"orderId"`
	AmountCents int64  `json:"amountCents"`
}

// Store is the shop's own records.
type Store struct {
	mu      sync.Mutex
	orders  map[string]*Order
	refunds map[uuid.UUID]Refund
	now     func() time.Time
}

func NewStore(orders ...Order) *Store {
	s := &Store{orders: map[string]*Order{}, refunds: map[uuid.UUID]Refund{}, now: time.Now}
	for _, o := range orders {
		s.orders[o.Id] = &o
	}
	return s
}

// Order returns an order as it stands.
func (s *Store) Order(id string) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// ErrMismatch is a claim in the payload that the records do not bear out.
// It is a refusal: the outcome refused, final. Its text names the claim,
// never what the records hold, so an agent cannot read the records by
// trying claims.
var ErrMismatch = fmt.Errorf("%w: the payload does not match the records", webhook.ErrRefused)

// Receiver is the shop's endpoint for the refunds.issue action: it trusts
// pgmandate's keys and accepts the action's audience for the tenant.
func (s *Store) Receiver(keys sign.KeySet, audience string, tenantId uuid.UUID) *webhook.Receiver {
	r := webhook.NewReceiver(keys)
	r.Trust(audience, tenantId)
	r.Handle = s.Apply
	return r
}

// Apply carries out a decided refund once per proposal, after checking each
// claim of the payload against the records.
func (s *Store) Apply(delivery webhook.Received) (any, error) {
	p := delivery.Payload.Proposal
	if p.Action != Action {
		return nil, fmt.Errorf("%w: this receiver carries out %s only", webhook.ErrRefused, Action)
	}
	var request Request
	if err := json.Unmarshal(p.Payload, &request); err != nil {
		return nil, fmt.Errorf("%w: the payload does not read as a refund", webhook.ErrRefused)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if done, ok := s.refunds[p.Id]; ok {
		return done, nil
	}
	// Never commit after notAfter: this is the last check before the write,
	// made under the same lock.
	if !s.now().Before(delivery.Payload.NotAfter) {
		return nil, fmt.Errorf("%w: too late: the delivery's notAfter has passed", webhook.ErrRefused)
	}
	if request.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: the amount must be more than zero", ErrMismatch)
	}
	order, ok := s.orders[request.OrderId]
	if !ok {
		return nil, fmt.Errorf("%w: no such order", ErrMismatch)
	}
	// The claim was made when the agent proposed; a person may approve it,
	// or a retry deliver it, days later. It is checked as of then, and only
	// against the agent: a claim that makes the case look weaker than the
	// records do is no reason to refuse.
	days := int(p.ProposedAt.Sub(order.DeliveredAt).Hours() / 24)
	switch {
	case request.Currency != order.Currency:
		return nil, fmt.Errorf("%w: the currency is not the order's", ErrMismatch)
	case days > request.DaysSinceDelivery:
		return nil, fmt.Errorf("%w: the order was delivered longer ago than claimed", ErrMismatch)
	case request.AmountCents > order.PaidCents-order.RefundedCents:
		return nil, fmt.Errorf("%w: the amount is more than is left to refund on the order", ErrMismatch)
	}
	order.RefundedCents += request.AmountCents
	refund := Refund{RefundId: "RF-" + p.Id.String()[:8], OrderId: order.Id, AmountCents: request.AmountCents}
	s.refunds[p.Id] = refund
	return refund, nil
}
