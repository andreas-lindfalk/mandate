// Package deliveries carries decided proposals to their receivers, under
// delivery contract v2 (docs/design-0.1.md, section 3).
//
// The decision is final in the database first; the delivery is a job
// inserted in the same transaction. Each attempt is opened in its own
// committed transaction before the call, so an attempt counts as possibly
// sent from the moment it opens until it is proven unsent. Only a
// receiver's explicit answer is final: done, refused or unknown. Anything
// else is retried until the attempts run out or the deadline comes near,
// and then the delivery ends unknown if an attempt was possibly sent, and
// not sent if none was. A person settles an unknown delivery once the work
// can no longer happen, and may send again one that was never sent.
package deliveries

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/andreas-lindfalk/pgmandate/modules/deliveries/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

// The states of a delivery.
const (
	StatusPending        = "pending"
	StatusDone           = "done"
	StatusRefused        = "refused"
	StatusNotSent        = "not_sent"
	StatusUnknown        = "unknown"
	StatusSettledDone    = "settled_done"
	StatusSettledNotDone = "settled_not_done"
)

const (
	// MaxAttempts is how many times a delivery is tried at most. River
	// backs off between attempts, never past the deadline.
	MaxAttempts = 8
	// RequestTimeout bounds one call to a receiver. The database holds the
	// same value (mandate.delivery_timeout).
	RequestTimeout = 15 * time.Second
	// Skew is the allowance for clocks the contract states (mandate.delivery_skew).
	Skew = 5 * time.Minute
	// staleAfter is how long an open attempt is taken to be still under way
	// (mandate.delivery_stale).
	staleAfter  = 45 * time.Second
	maxResponse = 4096
)

type Delivery struct {
	Id         uuid.UUID
	ProposalId uuid.UUID
	ActionKey  string
	Url        string
	Audience   string
	Status     string
	// NotAfter is when the delivery must no longer be carried out.
	NotAfter        time.Time
	AttemptsStarted int
	AttemptsUnsent  int
	AttemptOpenAt   *time.Time
	LastStatus      *int
	LastError       string
	// Result is the receiver's result, its refusal, or what it said when it
	// could not tell.
	Result     json.RawMessage
	CreatedAt  time.Time
	FinishedAt *time.Time
	SettledBy  *uuid.UUID
}

// PossiblySent says whether an attempt may have reached the receiver.
func (d Delivery) PossiblySent() bool {
	return d.AttemptsStarted > d.AttemptsUnsent || d.AttemptOpenAt != nil
}

// SettleAfter is the first moment a person may settle the delivery if it
// ends unknown: when the work can no longer happen.
func (d Delivery) SettleAfter() time.Time {
	return d.NotAfter.Add(RequestTimeout + Skew)
}

// SendAgainBefore is the last moment an attempt may open, and so the last
// moment a delivery never sent may be sent again.
func (d Delivery) SendAgainBefore() time.Time {
	return d.NotAfter.Add(-RequestTimeout)
}

// Payload is the body of a delivery, as docs/delivery-v2.schema.json
// describes it. Within one address and audience it is the same on every
// attempt: the time of an attempt is in the timestamp header.
type Payload struct {
	Version    int       `json:"version"`
	Kind       string    `json:"kind"`
	Audience   string    `json:"audience"`
	TenantId   uuid.UUID `json:"tenantId"`
	DeliveryId uuid.UUID `json:"deliveryId"`
	// CorrelationId follows one piece of work through every system: the
	// agent's run id when it gave one, otherwise the proposal's id.
	CorrelationId string `json:"correlationId"`
	// CreatedAt is when the delivery was recorded, in the transaction of
	// the decision.
	CreatedAt string `json:"createdAt"`
	// NotAfter is when the receiver must no longer carry it out.
	NotAfter string          `json:"notAfter"`
	Proposal PayloadProposal `json:"proposal"`
	Decision PayloadDecision `json:"decision"`
}

type PayloadProposal struct {
	Id      uuid.UUID       `json:"id"`
	Action  string          `json:"action"`
	Title   string          `json:"title"`
	Payload json.RawMessage `json:"payload"`
	Risk    string          `json:"risk"`
	AgentId uuid.UUID       `json:"agentId"`
	// RunId is the agent's own id for a chain of work, when it gave one.
	RunId      string `json:"runId,omitempty"`
	ProposedAt string `json:"proposedAt"`
}

type PayloadDecision struct {
	Status         string                    `json:"status"`
	DecidedAt      string                    `json:"decidedAt"`
	DecidedBy      *uuid.UUID                `json:"decidedBy,omitempty"`
	Note           string                    `json:"note,omitempty"`
	Edited         bool                      `json:"edited"`
	ExecutionBasis *proposals.ExecutionBasis `json:"executionBasis,omitempty"`
}

// timeFormat is RFC 3339 in UTC to the second, as a proposal's own times.
const timeFormat = "2006-01-02T15:04:05Z07:00"

// payloadOf builds the body for a delivery of a decided proposal.
func payloadOf(tenantId uuid.UUID, delivery Delivery, proposal proposals.Proposal) (Payload, error) {
	if proposal.DecidedAt == nil {
		return Payload{}, errors.New("deliveries: the proposal has no decision")
	}
	correlationId := proposal.RunId
	if correlationId == "" {
		correlationId = proposal.Id.String()
	}
	return Payload{
		Version: sign.Version, Kind: sign.KindDelivery, Audience: delivery.Audience, TenantId: tenantId, DeliveryId: delivery.Id,
		CorrelationId: correlationId, CreatedAt: delivery.CreatedAt.UTC().Format(timeFormat), NotAfter: delivery.NotAfter.UTC().Format(timeFormat),
		Proposal: PayloadProposal{Id: proposal.Id, Action: proposal.Action, Title: proposal.Title, Payload: proposal.Payload, Risk: proposal.Risk,
			AgentId: proposal.AgentId, RunId: proposal.RunId, ProposedAt: proposal.CreatedAt},
		Decision: PayloadDecision{Status: proposal.Status, DecidedAt: *proposal.DecidedAt, DecidedBy: proposal.DecidedBy, Note: proposal.DecisionNote,
			Edited: proposal.Edited, ExecutionBasis: proposal.ExecutionBasis},
	}, nil
}

type Service struct {
	pool         *pgxpool.Pool
	queue        *jobs.Queue
	allowPrivate bool
	proposals    *proposals.Service
	client       *http.Client
	signer       *sign.Signer
}

// Options tune the deliveries.
type Options struct {
	// AllowPrivate lets receivers live on loopback, private and link-local
	// addresses: for local development and for an on-premise install whose
	// receivers are internal. Off, such addresses are refused, so a tenant
	// cannot make the server call into its own network.
	AllowPrivate bool
	// Signer signs every delivery. Without one nothing is sent.
	Signer *sign.Signer
}

// errBlocked is returned by the dialer for an address a receiver may not have.
var errBlocked = errors.New("the receiver's address is private or local")

// errPlain is a receiver on a public address without TLS: the decision
// and its signature would cross the internet in the clear.
var errPlain = errors.New("the receiver is plain http on a public address; use https")

// newClient makes the client deliveries are sent with. It never follows a
// redirect, and it checks every address it connects to after DNS
// resolution, so a public name that points inward is refused too.
func newClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errBlocked
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return errBlocked
			}
			if blockedAddr(ip) {
				return errBlocked
			}
			return nil
		}
	}
	return &http.Client{
		Transport:     &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: RequestTimeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// blockedAddr says whether a receiver may not live at an address: this
// machine, a private or link-local network, and the ranges that are not
// public addresses or that wrap an IPv4 address inside an IPv6 one.
func blockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: an IPv4 address inside
	netip.MustParsePrefix("2002::/16"),      // 6to4: an IPv4 address inside
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fec0::/10"),      // site-local, deprecated but still routed in places
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64: an IPv4 address inside
	netip.MustParsePrefix("2001::/32"),      // Teredo: an IPv4 address inside
}

// New wires the service and, with a queue, its worker.
func New(pool *pgxpool.Pool, queue *jobs.Queue, proposalsService *proposals.Service, opts Options) *Service {
	s := &Service{pool: pool, queue: queue, proposals: proposalsService, client: newClient(opts.AllowPrivate),
		allowPrivate: opts.AllowPrivate, signer: opts.Signer}
	if queue != nil {
		river.AddWorker(queue.Workers(), &worker{service: s})
	}
	return s
}

// Enqueue records a delivery for a decided proposal in the caller's
// transaction and queues its job, so the delivery exists if and only if the
// decision was committed.
func (s *Service) Enqueue(ctx context.Context, tx pgx.Tx, proposal proposals.Proposal, url, audience string) error {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return errors.New("deliveries: no tenant in context")
	}
	id := uuid.New()
	notAfter, err := repo.New(tx).InsertDelivery(ctx, repo.InsertDeliveryParams{
		Id: id, TenantId: tenantId, ProposalId: proposal.Id, ActionKey: proposal.Action, Url: url, Audience: audience,
	})
	if err != nil {
		return err
	}
	if s.queue == nil {
		// Without a queue nothing sends it by itself: an embedded host
		// that runs no queue calls Deliver for the row.
		slog.WarnContext(ctx, "a delivery was recorded but there is no job queue to send it", "delivery", id, "proposal", proposal.Id)
		return nil
	}
	return s.queue.InsertTx(ctx, tx, DeliverArgs{TenantId: tenantId, DeliveryId: id, NotAfter: notAfter},
		&river.InsertOpts{Queue: jobs.QueueDeliveries, MaxAttempts: MaxAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight}})
}

// Retarget moves the action's waiting deliveries that were never possibly
// sent to its new address and audience, in the caller's transaction, which
// is the owner's change of the action. A delivery possibly sent stays
// where it went: sending it elsewhere could do the work twice.
func (s *Service) Retarget(ctx context.Context, tx pgx.Tx, actionKey, url, audience string) error {
	moved, err := repo.New(tx).RetargetWaiting(ctx, repo.RetargetWaitingParams{Url: url, Audience: audience, ActionKey: actionKey})
	if err != nil {
		return err
	}
	if len(moved) == 0 {
		return nil
	}
	list := make([]events.Event, 0, len(moved))
	for _, m := range moved {
		list = append(list, retargetEvent(actionKey, url, audience, m.Id, m.ProposalId))
	}
	_, err = events.AppendMany(ctx, tx, list)
	return err
}

func retargetEvent(actionKey, url, audience string, deliveryId, proposalId uuid.UUID) events.Event {
	return events.Event{
		Type: "delivery.retargeted", SubjectId: proposalId,
		Payload: map[string]any{"deliveryId": deliveryId, "action": actionKey, "connectorUrlSha256": urlDigest(url), "audience": audience},
	}
}

// urlDigest names a receiver's address in the chain without its text,
// which may carry credentials.
func urlDigest(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// DeliverArgs is the job that delivers one decision.
type DeliverArgs struct {
	TenantId   uuid.UUID `json:"tenantId"`
	DeliveryId uuid.UUID `json:"deliveryId"`
	// NotAfter caps when River retries: never past the last moment an
	// attempt may open.
	NotAfter time.Time `json:"notAfter"`
}

func (DeliverArgs) Kind() string { return "mandate.deliver" }

type worker struct {
	river.WorkerDefaults[DeliverArgs]
	service *Service
}

func (w *worker) Timeout(*river.Job[DeliverArgs]) time.Duration {
	return RequestTimeout + 15*time.Second
}

// NextRetry backs off as River does, never later than the last moment an
// attempt may open. A job that runs then finds the attempt refused and
// finishes the delivery.
func (w *worker) NextRetry(job *river.Job[DeliverArgs]) time.Time {
	next := (&river.DefaultClientRetryPolicy{}).NextRetry(job.JobRow)
	if last := job.Args.NotAfter.Add(-RequestTimeout); !job.Args.NotAfter.IsZero() && next.After(last) {
		return last
	}
	return next
}

func (w *worker) Work(ctx context.Context, job *river.Job[DeliverArgs]) error {
	return w.service.Deliver(ctx, job.Args.TenantId, job.Args.DeliveryId, job.Attempt >= job.MaxAttempts)
}

// errLater asks the job to try again: another attempt is still open.
var errLater = errors.New("deliveries: an attempt is still open")

// Deliver makes one attempt, as the system: it opens the attempt, sends
// the signed body, and records what the receiver said. lastAttempt says
// this is the job's last try; the delivery is then finished whatever the
// answer was.
func (s *Service) Deliver(ctx context.Context, tenantId, deliveryId uuid.UUID, lastAttempt bool) error {
	if s.signer == nil {
		return errors.New("deliveries: no signing key, so nothing is sent")
	}
	ctx = actor.AsSystem(ctx, tenantId)
	var (
		delivery Delivery
		opened   time.Time
		skip     bool
	)
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		row, err := q.GetDeliveryForUpdate(ctx, deliveryId)
		if errors.Is(err, pgx.ErrNoRows) {
			skip = true
			return nil
		}
		if err != nil {
			return err
		}
		delivery = toDelivery(row)
		if delivery.Status != StatusPending {
			skip = true
			return nil
		}
		now, err := q.DbNow(ctx)
		if err != nil {
			return err
		}
		if delivery.AttemptOpenAt != nil {
			if delivery.AttemptOpenAt.After(now.Add(-staleAfter)) {
				return errLater
			}
			// Left open by a crash: closed here, as the sweep would, and
			// still counted as possibly sent.
			slog.WarnContext(ctx, "an attempt was left open, as by a crash; it counts as possibly sent", "delivery", delivery.Id, "proposal", delivery.ProposalId)
			if _, err := q.CloseOpen(ctx, repo.CloseOpenParams{Id: delivery.Id, LastStatus: row.LastStatus, LastError: row.LastError}); err != nil {
				return err
			}
			delivery.AttemptOpenAt = nil
		}
		if !now.Before(delivery.SendAgainBefore()) {
			skip = true
			return s.finish(ctx, tx, delivery, "the deadline came before the delivery could be sent")
		}
		connector, err := q.ConnectorOf(ctx, delivery.ActionKey)
		if errors.Is(err, pgx.ErrNoRows) {
			skip = true
			return s.finish(ctx, tx, delivery, "the action no longer has a webhook connector")
		}
		if err != nil {
			return err
		}
		// A delivery left behind by a move goes to the action's address and
		// audience now, unless it may already have reached the old one.
		if (connector.Url != delivery.Url || connector.Audience != delivery.Audience) && !delivery.PossiblySent() {
			if err := q.MoveDelivery(ctx, repo.MoveDeliveryParams{Id: delivery.Id, Url: connector.Url, Audience: connector.Audience}); err != nil {
				return err
			}
			if _, err := events.Append(ctx, tx, retargetEvent(delivery.ActionKey, connector.Url, connector.Audience, delivery.Id, delivery.ProposalId)); err != nil {
				return err
			}
			delivery.Url, delivery.Audience = connector.Url, connector.Audience
		}
		opened, err = q.OpenAttempt(ctx, delivery.Id)
		return err
	})
	if errors.Is(err, errLater) {
		return err
	}
	if err != nil {
		return fault.FromDb(err)
	}
	if skip {
		return nil
	}

	proposal, err := s.proposals.Get(ctx, delivery.ProposalId)
	var body []byte
	if err == nil {
		var payload Payload
		if payload, err = payloadOf(tenantId, delivery, proposal); err == nil {
			body, err = json.Marshal(payload)
		}
	}
	if err != nil {
		// Nothing was sent: the body could not be built.
		return s.closed(ctx, delivery, attemptResult{err: err, message: "the delivery's body could not be built"}, lastAttempt, nil)
	}

	// The call leaves the database, so it happens outside any transaction.
	// Its deadline is measured on the database's clock, as the guard's is:
	// the attempt opened at least RequestTimeout before notAfter.
	deadline := min(RequestTimeout, delivery.NotAfter.Sub(opened))
	result := s.post(ctx, delivery.Url, delivery.Id, body, opened.Unix(), s.signer.Sign(opened.Unix(), body), deadline)
	return s.closed(ctx, delivery, result, lastAttempt, body)
}

// attemptResult is what one call learned.
type attemptResult struct {
	status int
	// sent says the request's headers were written: it may have reached
	// the receiver.
	sent bool
	// outcome is the receiver's explicit answer, if it gave one, and kept
	// is what is kept of it.
	outcome string
	kept    json.RawMessage
	err     error
	message string
}

// closed records how an attempt ended, and finishes the delivery when that
// is all there is to do.
func (s *Service) closed(ctx context.Context, delivery Delivery, r attemptResult, lastAttempt bool, body []byte) error {
	var lastStatus *int32
	if r.status != 0 {
		code := int32(r.status) //nolint:gosec // an HTTP status
		lastStatus = &code
	}
	permanent := errors.Is(r.err, errBlocked) || errors.Is(r.err, errPlain)
	finished, conflict := false, false
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if r.outcome != "" {
			rows, err := q.RecordAnswer(ctx, repo.RecordAnswerParams{Id: delivery.Id, Status: r.outcome, Result: r.kept, LastStatus: int32(r.status)}) //nolint:gosec // an HTTP status
			if err != nil {
				return err
			}
			if rows == 0 {
				// The attempt was closed meanwhile, by the sweep after it
				// stood open too long: the answer came too late to count.
				conflict = true
				return appendConflict(ctx, tx, delivery, r)
			}
			finished = true
			return appendOutcome(ctx, tx, delivery, r.outcome, outcomeFields(delivery, r, body))
		}
		message := r.message
		if message == "" {
			message = describe(r.err, r.status)
		}
		var rows int64
		var err error
		if !r.sent {
			rows, err = q.CloseUnsent(ctx, repo.CloseUnsentParams{Id: delivery.Id, LastStatus: lastStatus, LastError: &message})
		} else {
			rows, err = q.CloseOpen(ctx, repo.CloseOpenParams{Id: delivery.Id, LastStatus: lastStatus, LastError: &message})
		}
		if err != nil || rows == 0 {
			return err
		}
		if lastAttempt || permanent {
			after, err := q.GetDelivery(ctx, delivery.Id)
			if err != nil {
				return err
			}
			finished = true
			return s.finish(ctx, tx, toDelivery(after), message)
		}
		return nil
	})
	if err != nil {
		return errors.Join(r.err, fault.FromDb(err))
	}
	if finished {
		// Done with: the job completes, and nothing is retried.
		return nil
	}
	if conflict {
		return fmt.Errorf("deliver %s: the receiver answered %s after its attempt was closed; logged as a conflict", delivery.Id, r.outcome)
	}
	return fmt.Errorf("deliver %s: %s", delivery.Id, describe(r.err, r.status))
}

// finish ends a waiting delivery with no attempt open, in the caller's
// transaction: unknown if an attempt was possibly sent, not sent if none
// was. The database decides which from the counters.
func (s *Service) finish(ctx context.Context, tx pgx.Tx, delivery Delivery, reason string) error {
	status, err := repo.New(tx).Finish(ctx, repo.FinishParams{Id: delivery.Id, Reason: &reason})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == StatusUnknown {
		slog.WarnContext(ctx, "a delivery ended unknown: a person must settle it", "delivery", delivery.Id, "proposal", delivery.ProposalId, "reason", reason)
	}
	return appendOutcome(ctx, tx, delivery, status, map[string]any{
		"deliveryId": delivery.Id, "action": delivery.ActionKey, "attempts": delivery.AttemptsStarted, "unsent": delivery.AttemptsUnsent, "reason": reason,
	})
}

func outcomeFields(delivery Delivery, r attemptResult, body []byte) map[string]any {
	fields := map[string]any{
		"deliveryId": delivery.Id, "action": delivery.ActionKey, "status": r.status, "attempts": delivery.AttemptsStarted + 1,
		"connectorUrlSha256": urlDigest(delivery.Url), "audience": delivery.Audience,
	}
	if body != nil {
		// The exact bytes that were signed and sent.
		sum := sha256.Sum256(body)
		fields["bodySha256"] = hex.EncodeToString(sum[:])
	}
	if len(r.kept) > 0 {
		if digest, err := events.Digest(r.kept); err == nil {
			fields["resultSha256"] = digest
		}
	}
	return fields
}

func appendOutcome(ctx context.Context, tx pgx.Tx, delivery Delivery, status string, fields map[string]any) error {
	_, err := events.Append(ctx, tx, events.Event{Type: "delivery." + status, SubjectId: delivery.ProposalId, Payload: fields})
	return err
}

func appendConflict(ctx context.Context, tx pgx.Tx, delivery Delivery, r attemptResult) error {
	slog.WarnContext(ctx, "a receiver answered after its attempt was closed; a person should look", "delivery", delivery.Id, "outcome", r.outcome)
	fields := map[string]any{"deliveryId": delivery.Id, "action": delivery.ActionKey, "answered": r.outcome, "status": r.status}
	if len(r.kept) > 0 {
		if digest, err := events.Digest(r.kept); err == nil {
			fields["resultSha256"] = digest
		}
	}
	_, err := events.Append(ctx, tx, events.Event{Type: "delivery.conflict", SubjectId: delivery.ProposalId, Payload: fields})
	return err
}

// describe says what went wrong in words that are safe to keep and show:
// never the address, which may carry credentials, nor what the server could
// reach inside its own network.
func describe(err error, status int) string {
	var netErr net.Error
	switch {
	case errors.Is(err, errBlocked):
		return "blocked: " + errBlocked.Error()
	case errors.Is(err, errPlain):
		return "blocked: " + errPlain.Error()
	case status != 0:
		return fmt.Sprintf("the receiver answered %d without an outcome", status)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return "the receiver did not answer in time"
	case err == nil:
		return "the receiver gave no outcome"
	}
	return "the receiver could not be reached"
}

// post sends the signed body and reads the receiver's answer. Whether the
// request may have reached the receiver is taken from whether its headers
// were written: before that, it surely did not.
func (s *Service) post(ctx context.Context, url string, deliveryId uuid.UUID, body []byte, timestamp int64, signature string, deadline time.Duration) attemptResult {
	// Plain http is for local and private receivers, which only an
	// operator who allows them can use.
	if !s.allowPrivate && !strings.HasPrefix(strings.ToLower(url), "https://") {
		return attemptResult{err: errPlain}
	}
	ctx, cancel := context.WithTimeout(ctx, max(deadline, time.Second))
	defer cancel()
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return attemptResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pgmandate")
	req.Header.Set(sign.HeaderDeliveryId, deliveryId.String())
	// The timestamp is the database's clock, read when the attempt opened:
	// the one clock every deadline is measured by.
	req.Header.Set(sign.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	req.Header.Set(sign.HeaderSignature, signature)
	return s.send(req, &wrote)
}

func (s *Service) send(req *http.Request, wrote *atomic.Bool) attemptResult {
	resp, err := s.client.Do(req)
	if err != nil {
		return attemptResult{err: err, sent: wrote.Load()}
	}
	defer resp.Body.Close() //nolint:errcheck
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	outcome, kept := explicitAnswer(resp.StatusCode, answer)
	return attemptResult{status: resp.StatusCode, sent: true, outcome: outcome, kept: kept}
}

// explicitAnswer reads a receiver's answer: a JSON object of at most 4 kB
// that says what happened. Anything else says nothing final.
func explicitAnswer(status int, answer []byte) (string, json.RawMessage) {
	if len(answer) > maxResponse {
		return "", nil
	}
	var parsed struct {
		Outcome string          `json:"outcome"`
		Result  json.RawMessage `json:"result"`
		Refusal json.RawMessage `json:"refusal"`
	}
	if json.Unmarshal(answer, &parsed) != nil {
		return "", nil
	}
	ok := status >= 200 && status <= 299
	switch {
	case parsed.Outcome == StatusDone && ok:
		return StatusDone, objectOrNil(parsed.Result)
	case parsed.Outcome == StatusRefused:
		return StatusRefused, objectOrNil(parsed.Refusal)
	case parsed.Outcome == StatusUnknown && ok:
		// Kept whole: what the receiver said when it could not tell.
		return StatusUnknown, objectOrNil(answer)
	}
	return "", nil
}

func objectOrNil(raw json.RawMessage) json.RawMessage {
	var object map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &object) != nil || object == nil {
		return nil
	}
	return raw
}

// SendAgain sends a delivery that was never sent, as a person of the
// tenant, before its deadline: the same delivery, to the action's address
// and audience as they are now, with a new job.
func (s *Service) SendAgain(ctx context.Context, proposalId uuid.UUID) (Delivery, error) {
	a, ok := actor.From(ctx)
	if !ok || a.Kind != actor.Person {
		return Delivery{}, fault.New(fault.PermissionDenied, "only a person sends a delivery again")
	}
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Delivery{}, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	var result Delivery
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		row, err := q.GetDeliveryByProposalForUpdate(ctx, proposalId)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "the proposal has no delivery")
		}
		if err != nil {
			return err
		}
		delivery := toDelivery(row)
		if delivery.Status != StatusNotSent {
			return fault.New(fault.Conflict, "only a delivery that was never sent is sent again: this one is "+delivery.Status)
		}
		now, err := q.DbNow(ctx)
		if err != nil {
			return err
		}
		if !now.Before(delivery.SendAgainBefore()) {
			return fault.New(fault.Conflict, "too late to send it again: the deadline has passed")
		}
		connector, err := q.ConnectorOf(ctx, row.ActionKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.InvalidArgument, "the action has no webhook connector now")
		}
		if err != nil {
			return err
		}
		if err := q.SendAgain(ctx, repo.SendAgainParams{Id: row.Id, Url: connector.Url, Audience: connector.Audience}); err != nil {
			return err
		}
		if _, err := events.Append(ctx, tx, events.Event{
			Type: "delivery.retried", SubjectId: proposalId, Payload: map[string]any{"deliveryId": row.Id, "action": row.ActionKey, "audience": connector.Audience},
		}); err != nil {
			return err
		}
		after, err := q.GetDelivery(ctx, row.Id)
		if err != nil {
			return err
		}
		result = toDelivery(after)
		if s.queue == nil {
			return nil
		}
		// No uniqueness: the job that gave up may still be finishing in the
		// queue. Only a delivery never sent is sent again, so two of these
		// cannot both pass.
		return s.queue.InsertTx(ctx, tx, DeliverArgs{TenantId: tenantId, DeliveryId: row.Id, NotAfter: row.NotAfter},
			&river.InsertOpts{Queue: jobs.QueueDeliveries, MaxAttempts: MaxAttempts})
	})
	return result, fault.FromDb(err)
}

// Settle records, as a person, what happened to a delivery whose outcome
// is unknown: done or not done. Only once the work can no longer happen.
func (s *Service) Settle(ctx context.Context, proposalId uuid.UUID, done bool) (Delivery, error) {
	a, ok := actor.From(ctx)
	if !ok || a.Kind != actor.Person {
		return Delivery{}, fault.New(fault.PermissionDenied, "only a person settles a delivery")
	}
	status := StatusSettledNotDone
	if done {
		status = StatusSettledDone
	}
	var result Delivery
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		row, err := q.GetDeliveryByProposalForUpdate(ctx, proposalId)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "the proposal has no delivery")
		}
		if err != nil {
			return err
		}
		delivery := toDelivery(row)
		if delivery.Status != StatusUnknown {
			return fault.New(fault.Conflict, "only a delivery whose outcome is unknown is settled: this one is "+delivery.Status)
		}
		now, err := q.DbNow(ctx)
		if err != nil {
			return err
		}
		if now.Before(delivery.SettleAfter()) {
			return fault.New(fault.Conflict, "not yet: the receiver may still be carrying it out until "+delivery.SettleAfter().UTC().Format(time.RFC3339))
		}
		if err := q.Settle(ctx, repo.SettleParams{Id: row.Id, Status: status}); err != nil {
			return err
		}
		if _, err := events.Append(ctx, tx, events.Event{
			Type: "delivery.settled", SubjectId: proposalId, Payload: map[string]any{"deliveryId": row.Id, "action": row.ActionKey, "outcome": status},
		}); err != nil {
			return err
		}
		after, err := q.GetDelivery(ctx, row.Id)
		result = toDelivery(after)
		return err
	})
	return result, fault.FromDb(err)
}

// FinishDue does the sweep's work on the deliveries of the tenant in scope,
// as the system: it closes attempts left open by a crash, which stay
// counted as possibly sent, and finishes the deliveries past the last
// moment an attempt may open. It returns how many it finished.
func (s *Service) FinishDue(ctx context.Context) (int, error) {
	a, ok := actor.From(ctx)
	if !ok || a.Kind != actor.System {
		return 0, fault.New(fault.PermissionDenied, "only the system finishes deliveries")
	}
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		stale, err := repo.New(tx).CloseStale(ctx)
		for _, row := range stale {
			slog.WarnContext(ctx, "an attempt was left open, as by a crash; it counts as possibly sent", "delivery", row.Id, "proposal", row.ProposalId)
		}
		return err
	}); err != nil {
		return 0, fault.FromDb(err)
	}
	finished := 0
	for {
		done := false
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			q := repo.New(tx)
			ids, err := q.ListDueToFinish(ctx, 1)
			if err != nil || len(ids) == 0 {
				done = true
				return err
			}
			row, err := q.GetDelivery(ctx, ids[0])
			if err != nil {
				return err
			}
			return s.finish(ctx, tx, toDelivery(row), "the deadline came")
		})
		if err != nil {
			return finished, fault.FromDb(err)
		}
		if done {
			return finished, nil
		}
		finished++
	}
}

// ByProposals returns the deliveries of the proposals, by proposal id.
func (s *Service) ByProposals(ctx context.Context, proposalIds []uuid.UUID) (map[uuid.UUID]Delivery, error) {
	result := map[uuid.UUID]Delivery{}
	if len(proposalIds) == 0 {
		return result, nil
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListDeliveriesByProposals(ctx, proposalIds)
		for _, row := range rows {
			result[row.ProposalId] = toDelivery(row)
		}
		return err
	})
	return result, err
}

// Get returns the delivery of a proposal, if it has one.
func (s *Service) Get(ctx context.Context, proposalId uuid.UUID) (Delivery, bool, error) {
	var result Delivery
	found := false
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := repo.New(tx).GetDeliveryByProposal(ctx, proposalId)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result, found = toDelivery(row), true
		return nil
	})
	return result, found, err
}

func toDelivery(row repo.MandateDelivery) Delivery {
	d := Delivery{
		Id: row.Id, ProposalId: row.ProposalId, ActionKey: row.ActionKey, Url: row.Url, Audience: row.Audience, Status: row.Status,
		NotAfter: row.NotAfter, AttemptsStarted: int(row.AttemptsStarted), AttemptsUnsent: int(row.AttemptsUnsent), AttemptOpenAt: row.AttemptOpenAt,
		CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt, SettledBy: row.SettledBy,
	}
	if row.LastStatus != nil {
		status := int(*row.LastStatus)
		d.LastStatus = &status
	}
	if row.LastError != nil {
		d.LastError = *row.LastError
	}
	if len(row.Result) > 0 {
		d.Result = json.RawMessage(row.Result)
	}
	return d
}
