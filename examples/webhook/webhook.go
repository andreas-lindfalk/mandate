// Package webhook is a reference receiver of deliveries, delivery contract
// v2: an http.Handler to mount at the URL of a webhook action. It imports
// only the sign package, and does what the contract asks of a receiver, in
// this order:
//
//  1. verify the signature over the raw body, with a key it trusts,
//     fetching the published set again on a key id it does not know;
//  2. check the body's kind, audience and tenant are its own, and answer
//     one that is not without an outcome, so that it is retried: an action
//     just moved here may not have been configured yet;
//  3. claim the delivery id: a delivery it finished is answered as before,
//     one still in progress is answered 503, which is retried;
//  4. refuse, without acting, a delivery it never started that arrives
//     after notAfter;
//  5. check the facts and act (Handle), and answer with an explicit
//     outcome: done, refused or unknown.
//
// A real receiver keeps the claimed ids where a restart cannot lose them,
// at least until notAfter plus five minutes, and never commits work after
// notAfter by its own clock.
package webhook

import (
	"context"
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

// Payload is the body of a delivery, as docs/delivery-v2.schema.json
// describes it. Fields this receiver does not know are ignored.
type Payload struct {
	Version       int       `json:"version"`
	Kind          string    `json:"kind"`
	Audience      string    `json:"audience"`
	TenantId      uuid.UUID `json:"tenantId"`
	DeliveryId    uuid.UUID `json:"deliveryId"`
	CorrelationId string    `json:"correlationId"`
	CreatedAt     time.Time `json:"createdAt"`
	NotAfter      time.Time `json:"notAfter"`
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
			MandateId uuid.UUID `json:"mandateId"`
			PolicyRef string    `json:"policyRef"`
			MaxRisk   string    `json:"maxRisk"`
			Budgets   []struct {
				Kind  string `json:"kind"`
				Limit int64  `json:"limit"`
				Spent int64  `json:"spent"`
			} `json:"budgets"`
			Condition string `json:"condition"`
		} `json:"executionBasis"`
	} `json:"decision"`
}

type Received struct {
	DeliveryId uuid.UUID
	Payload    Payload
	// Body is the body exactly as it arrived and was signed.
	Body []byte
	At   time.Time
}

// Errors from Handle that say what happened.
var (
	// ErrRefused: the system refused on its own rules and did nothing.
	// Answered as the outcome refused, which is final.
	ErrRefused = errors.New("refused")
	// ErrUnknown: the system cannot tell whether it was done, as when its
	// own call timed out. Answered as the outcome unknown; a person settles
	// it.
	ErrUnknown = errors.New("outcome unknown")
)

type Receiver struct {
	mu       sync.Mutex
	keys     sign.KeySet
	trusted  []trust
	received []Received
	// answers are the deliveries finished, with what they were answered;
	// inProgress the ones claimed and still being carried out.
	answers    map[uuid.UUID]answer
	inProgress map[uuid.UUID]bool
	failures   int
	now        func() time.Time
	// refreshed is when Refresh last ran.
	refreshed time.Time
	// Refresh, when set, fetches the published key set again, as
	// sign.Fetch does. It is called on a key id the receiver does not know,
	// at most once a minute, and what it returns replaces the keys.
	Refresh func(context.Context) (sign.KeySet, error)
	// OnReceive, when set, is called for each new delivery, after it was
	// verified and claimed.
	OnReceive func(Received)
	// Handle, when set, carries a new delivery out. Its result is the
	// outcome done, kept by pgmandate as the result. An error that wraps
	// ErrRefused answers refused; one that wraps ErrUnknown answers
	// unknown; any other error, or a panic, is answered 500 and retried,
	// so Handle must have done nothing in that case, or be safe to run
	// twice (pass the delivery id on as an idempotency key).
	Handle func(Received) (any, error)
}

// trust is one audience this receiver accepts, with its tenant.
type trust struct {
	audience string
	tenantId string
}

// answer is what a delivery was answered, kept to answer it the same way
// again.
type answer struct {
	status int
	body   []byte
}

// NewReceiver makes a receiver that trusts deliveries signed by the keys.
// It accepts nothing until Trust names an audience.
func NewReceiver(keys sign.KeySet) *Receiver {
	return &Receiver{keys: keys, now: time.Now, answers: map[uuid.UUID]answer{}, inProgress: map[uuid.UUID]bool{}}
}

// SetKeys replaces the keys it trusts, as a receiver does when it fetches
// the published key set again.
func (r *Receiver) SetKeys(keys sign.KeySet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = keys
}

// Trust accepts deliveries for one audience of one tenant, as the action
// was defined with. A receiver that serves several actions trusts each.
func (r *Receiver) Trust(audience string, tenantId uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trusted = append(r.trusted, trust{audience: audience, tenantId: tenantId.String()})
}

// FailNext makes the next n deliveries answer 500, to see retries.
func (r *Receiver) FailNext(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = n
}

// Received lists what was carried out or is being carried out, oldest first.
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
	keys, trusted := r.keys, r.trusted
	fail := r.failures > 0
	if fail {
		r.failures--
	}
	r.mu.Unlock()

	// 1. The signature, over the raw bytes, before anything is parsed. A
	// key id it does not know may be a rotation: fetch the set again, once.
	verify := func(keys sign.KeySet) error {
		return sign.Verify(keys, req.Header.Get(sign.HeaderTimestamp), req.Header.Get(sign.HeaderSignature), body, r.now())
	}
	err = verify(keys)
	if errors.Is(err, sign.ErrUnknownKey) {
		if fresh, ok := r.refresh(req.Context()); ok {
			err = verify(fresh)
		}
	}
	if err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if fail {
		http.Error(w, "try again", http.StatusInternalServerError)
		return
	}
	// 2. The envelope: a version 2 delivery for one of its own audiences,
	// for that audience's tenant, with the header's delivery id.
	var envelope sign.Envelope
	accepted := false
	for _, t := range trusted {
		if envelope, err = sign.CheckEnvelope(body, req.Header.Get(sign.HeaderDeliveryId), t.audience, t.tenantId); err == nil {
			accepted = true
			break
		}
	}
	if !accepted {
		// Not for an audience it was configured with. It did nothing, but
		// answers without an outcome, so pgmandate tries again: an action
		// just moved here may not have been configured yet. Until then the
		// delivery cannot be settled as refused by this receiver.
		write(w, encode(http.StatusForbidden, map[string]any{"error": "not_for_this_receiver"}))
		return
	}
	var payload Payload
	if err := json.Unmarshal(body, &payload); err != nil || payload.DeliveryId.String() != envelope.DeliveryId {
		write(w, refusal(http.StatusBadRequest, "unreadable_payload"))
		return
	}
	if payload.Decision.Status != "approved" && payload.Decision.Status != "executed" {
		write(w, refusal(http.StatusForbidden, "not_a_decision"))
		return
	}

	// 3. Claim the delivery id, atomically.
	r.mu.Lock()
	if first, done := r.answers[payload.DeliveryId]; done {
		r.mu.Unlock()
		write(w, first)
		return
	}
	if r.inProgress[payload.DeliveryId] {
		r.mu.Unlock()
		http.Error(w, "in progress", http.StatusServiceUnavailable)
		return
	}
	// 4. Too late, and never started: refuse without acting.
	if !r.now().Before(envelope.NotAfter) {
		reply := refusal(http.StatusConflict, "too_late")
		r.answers[payload.DeliveryId] = reply
		r.mu.Unlock()
		write(w, reply)
		return
	}
	r.inProgress[payload.DeliveryId] = true
	received := Received{DeliveryId: payload.DeliveryId, Payload: payload, Body: body, At: r.now()}
	r.received = append(r.received, received)
	onReceive, handle := r.OnReceive, r.Handle
	r.mu.Unlock()
	if onReceive != nil {
		onReceive(received)
	}

	// 5. Act, and answer with an explicit outcome.
	var result any = map[string]any{"reference": fmt.Sprintf("REF-%s", payload.Proposal.Id.String()[:8]), "action": payload.Proposal.Action}
	if handle != nil {
		var panicked bool
		result, err, panicked = call(handle, received)
		if panicked || (err != nil && !errors.Is(err, ErrRefused) && !errors.Is(err, ErrUnknown)) {
			// Nothing is known to be done: let go of the claim, so that the
			// retry pgmandate makes after this 500 runs again.
			r.release(payload.DeliveryId)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	var reply answer
	switch {
	case errors.Is(err, ErrRefused):
		reply = refusal(http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrUnknown):
		reply = encode(http.StatusOK, map[string]any{"outcome": "unknown", "reason": err.Error()})
	default:
		reply = encode(http.StatusOK, map[string]any{"outcome": "done", "result": result})
	}
	r.mu.Lock()
	delete(r.inProgress, payload.DeliveryId)
	r.answers[payload.DeliveryId] = reply
	r.mu.Unlock()
	write(w, reply)
}

// refresh fetches the key set again, at most once a minute, and keeps it.
func (r *Receiver) refresh(ctx context.Context) (sign.KeySet, bool) {
	r.mu.Lock()
	fetch := r.Refresh
	if fetch == nil || r.now().Sub(r.refreshed) < time.Minute {
		r.mu.Unlock()
		return nil, false
	}
	r.refreshed = r.now()
	r.mu.Unlock()
	keys, err := fetch(ctx)
	if err != nil || len(keys) == 0 {
		return nil, false
	}
	r.SetKeys(keys)
	return keys, true
}

// release lets go of a claim on a delivery that was not carried out.
func (r *Receiver) release(deliveryId uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inProgress, deliveryId)
	for i := len(r.received) - 1; i >= 0; i-- {
		if r.received[i].DeliveryId == deliveryId {
			r.received = append(r.received[:i], r.received[i+1:]...)
			break
		}
	}
}

func refusal(status int, reason string) answer {
	return encode(status, map[string]any{"outcome": "refused", "refusal": map[string]any{"reason": reason}})
}

func encode(status int, value any) answer {
	body, err := json.Marshal(value)
	if err != nil {
		return answer{status: http.StatusInternalServerError, body: []byte(`{"error":"the answer could not be encoded"}`)}
	}
	return answer{status: status, body: body}
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
