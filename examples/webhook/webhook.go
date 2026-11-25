// Package webhook is a reference receiver of deliveries: an http.Handler to
// mount at the URL of a webhook action. It verifies the signature, drops
// a delivery it has already seen, decodes the payload, and answers with a
// JSON object, which mandate keeps as the proposal's result and shows to
// the agent. A real receiver does its work where this one appends to a
// list. It imports only the sign package.
package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/andreas-lindfalk/pgmandate/sign"
)

// maxBody bounds a delivery's body.
const maxBody = 1 << 20

// Payload is the body of a delivery, as docs/delivery-v1.schema.json
// describes it. Fields this receiver does not know are ignored.
type Payload struct {
	Version       int       `json:"version"`
	DeliveryId    uuid.UUID `json:"deliveryId"`
	CorrelationId string    `json:"correlationId"`
	TenantId      uuid.UUID `json:"tenantId"`
	CreatedAt     time.Time `json:"createdAt"`
	Proposal      struct {
		Id         uuid.UUID       `json:"id"`
		Action     string          `json:"action"`
		Title      string          `json:"title"`
		Payload    json.RawMessage `json:"payload"`
		Risk       string          `json:"risk"`
		AgentId    uuid.UUID       `json:"agentId"`
		RunId      string          `json:"runId"`
		ProposedAt time.Time       `json:"proposedAt"`
	} `json:"proposal"`
	Decision struct {
		Status         string     `json:"status"`
		DecidedAt      time.Time  `json:"decidedAt"`
		DecidedBy      *uuid.UUID `json:"decidedBy"`
		Note           string     `json:"note"`
		Edited         bool       `json:"edited"`
		ExecutionBasis *struct {
			MandateId   uuid.UUID `json:"mandateId"`
			PolicyRef   string    `json:"policyRef"`
			MaxRisk     string    `json:"maxRisk"`
			BudgetUnits int64     `json:"budgetUnits"`
			SpentUnits  int64     `json:"spentUnits"`
			CostUnits   int64     `json:"costUnits"`
			Condition   string    `json:"condition"`
		} `json:"executionBasis"`
	} `json:"decision"`
}

// Version is the version of the delivery body this receiver understands.
const Version = 1

type Received struct {
	DeliveryId uuid.UUID
	Payload    Payload
	// Body is the body exactly as it arrived and was signed.
	Body []byte
	At   time.Time
}

type Receiver struct {
	mu       sync.Mutex
	secrets  []trusted
	received []Received
	seen     map[uuid.UUID]bool
	failures int
	now      func() time.Time
	// OnReceive, when set, is called for each new delivery, after it was
	// verified and recorded.
	OnReceive func(Received)
	// Handle, when set, carries a new delivery out. Its result is the
	// answer, which pgmandate keeps as the proposal's result. An error that
	// wraps ErrRefused is a refusal on the system's own rules, answered 422
	// with its text, which pgmandate does not retry and shows to the person
	// and the agent; any other error is answered 500 and retried. A delivery
	// that arrives again after it was carried out gets the first answer; one
	// that was refused or failed runs again, so a person who fixed the
	// records can send it again. Handle must do its work in one step or
	// be safe to run twice: a panic or a 500 after part of the work runs
	// again on the retry (pass the delivery id on as an idempotency key).
	Handle  func(Received) (any, error)
	answers map[uuid.UUID]answer
}

// ErrRefused marks an error from Handle as a refusal on the system's own
// rules: answered 422, final.
var ErrRefused = errors.New("refused")

// answer is what a delivery was answered, kept to answer it the same way
// again.
type answer struct {
	status int
	body   []byte
}

// trusted is a secret and, when known, the one tenant and action it signs
// for. Each action has its own secret, so a receiver that serves several
// should bind each secret to its action: otherwise a delivery signed for
// one action is accepted as another.
type trusted struct {
	secret   []byte
	tenantId uuid.UUID
	action   string
}

// NewReceiver makes a receiver that trusts deliveries signed with the
// secret; an empty secret trusts nothing until AddSecret.
func NewReceiver(secret string) *Receiver {
	r := &Receiver{now: time.Now, seen: map[uuid.UUID]bool{}, answers: map[uuid.UUID]answer{}}
	if secret != "" {
		r.secrets = []trusted{{secret: []byte(secret)}}
	}
	return r
}

// SetSecret replaces every secret, after a rotation.
func (r *Receiver) SetSecret(secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = []trusted{{secret: []byte(secret)}}
}

// AddSecret trusts one more secret for any action, as the local receiver
// command does.
func (r *Receiver) AddSecret(secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, trusted{secret: []byte(secret)})
}

// AddSecretFor trusts a secret only for the one tenant and action it was
// issued for, which is what a real receiver serving several actions does.
func (r *Receiver) AddSecretFor(secret string, tenantId uuid.UUID, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, trusted{secret: []byte(secret), tenantId: tenantId, action: action})
}

// FailNext makes the next n deliveries answer 500, to see retries.
func (r *Receiver) FailNext(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = n
}

// Received lists what was accepted, oldest first.
func (r *Receiver) Received() []Received {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Received, len(r.received))
	copy(out, r.received)
	return out
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBody))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	secrets := r.secrets
	fail := r.failures > 0
	if fail {
		r.failures--
	}
	r.mu.Unlock()
	var signer *trusted
	for i := range secrets {
		if sign.Verify(secrets[i].secret, req.Header.Get(sign.HeaderTimestamp), req.Header.Get(sign.HeaderSignature), body, r.now()) == nil {
			signer = &secrets[i]
			break
		}
	}
	if signer == nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if fail {
		http.Error(w, "try again", http.StatusInternalServerError)
		return
	}
	var payload Payload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	// A new version means a field went away or changed meaning: refusing it
	// is safer than acting on a body read wrongly. A 4xx is not retried.
	if payload.Version != Version {
		http.Error(w, fmt.Sprintf("unknown payload version %d", payload.Version), http.StatusBadRequest)
		return
	}
	// The secret signs for one tenant's action, and only a decision is
	// ever delivered. A 403 is not retried.
	if signer.action != "" && (payload.TenantId != signer.tenantId || payload.Proposal.Action != signer.action) {
		http.Error(w, "this secret does not sign for that tenant and action", http.StatusForbidden)
		return
	}
	if payload.Decision.Status != "approved" && payload.Decision.Status != "executed" {
		http.Error(w, "only an approved or executed proposal is carried out", http.StatusForbidden)
		return
	}
	deliveryId, err := uuid.Parse(req.Header.Get(sign.HeaderDeliveryId))
	if err != nil || deliveryId != payload.DeliveryId {
		http.Error(w, "bad delivery id", http.StatusBadRequest)
		return
	}
	// Delivery is at least once: the same delivery can arrive again after a
	// timeout. It is done once and answered the same way every time. The
	// ids are kept in memory here; a real receiver keeps them where a
	// restart cannot lose them, or passes the id on as the idempotency key
	// of the call it makes.
	r.mu.Lock()
	duplicate := r.seen[deliveryId]
	first, answered := r.answers[deliveryId]
	received := Received{DeliveryId: deliveryId, Payload: payload, Body: body, At: r.now()}
	if !duplicate {
		r.seen[deliveryId] = true
		r.received = append(r.received, received)
	}
	onReceive, handle := r.OnReceive, r.Handle
	r.mu.Unlock()
	if !duplicate && onReceive != nil {
		onReceive(received)
	}
	if handle != nil {
		switch {
		case answered:
			write(w, first)
		case duplicate:
			// The first copy is still being carried out: ask for a retry.
			http.Error(w, "in progress", http.StatusServiceUnavailable)
		default:
			result, err, panicked := call(handle, received)
			if panicked {
				// Nothing is known to be done: forget the delivery, so that
				// the retry pgmandate makes after this 500 runs again.
				r.mu.Lock()
				r.forget(deliveryId)
				r.mu.Unlock()
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			reply := answer{status: http.StatusOK}
			switch {
			case errors.Is(err, ErrRefused):
				reply.status = http.StatusUnprocessableEntity
				result = map[string]any{"refused": err.Error()}
			case err != nil:
				reply.status = http.StatusInternalServerError
				result = map[string]any{"error": "the system could not carry it out now"}
			}
			body, encodeErr := json.Marshal(result)
			if encodeErr != nil {
				reply.status, body = http.StatusInternalServerError, []byte(`{"error":"the result could not be encoded"}`)
			}
			reply.body = body
			r.mu.Lock()
			if reply.status == http.StatusOK {
				r.answers[deliveryId] = reply
			} else {
				// Only what was done is remembered: a refusal or a failure
				// runs again when the delivery is sent again.
				r.forget(deliveryId)
			}
			r.mu.Unlock()
			write(w, reply)
		}
		return
	}
	// The answer is the result the agent gets back: here a reference made
	// from the proposal, where a real system would return its own id.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"reference": fmt.Sprintf("REF-%s", payload.Proposal.Id.String()[:8]), "action": payload.Proposal.Action})
}

func write(w http.ResponseWriter, a answer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// call runs handle and reports a panic instead of passing it on.
func call(handle func(Received) (any, error), received Received) (result any, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			result, err, panicked = nil, nil, true
		}
	}()
	result, err = handle(received)
	return result, err, false
}

// forget takes back a delivery that was not carried out, so that the next
// copy of it runs again and is recorded once. The caller holds r.mu.
func (r *Receiver) forget(deliveryId uuid.UUID) {
	delete(r.seen, deliveryId)
	for i := len(r.received) - 1; i >= 0; i-- {
		if r.received[i].DeliveryId == deliveryId {
			r.received = append(r.received[:i], r.received[i+1:]...)
			break
		}
	}
}
