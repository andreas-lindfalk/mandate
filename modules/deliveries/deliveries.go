// Package deliveries carries decided proposals to their webhook
// connectors. The decision is final in the database first; the delivery is a
// job inserted in the same transaction, retried with backoff, and recorded
// in the chain when it is delivered or when it finally fails. The
// proposal's status never depends on it.
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
	"net/netip"
	"strconv"
	"strings"
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

const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"

	// MaxAttempts is how many times a delivery is tried before it fails.
	// River backs off between attempts.
	MaxAttempts = 8
	// requestTimeout bounds one call to a connector.
	requestTimeout = 15 * time.Second
	maxResponse    = 4096
)

type Delivery struct {
	Id         uuid.UUID
	ProposalId uuid.UUID
	ActionKey  string
	Url        string
	Status     string
	Attempts   int
	LastStatus *int
	LastError  string
	// Result is what the connector answered, when it answered with a JSON object.
	Result      json.RawMessage
	CreatedAt   time.Time
	DeliveredAt *time.Time
	FailedAt    *time.Time
}

// PayloadVersion is the version of the delivery body. It changes when a
// field is removed or changes meaning, never when one is added, so a
// receiver refuses a version it does not know and ignores fields it does
// not know. The body is described by docs/delivery-v1.schema.json.
const PayloadVersion = 1

// Payload is the body of a delivery. It is the same on every attempt: the
// time of an attempt is in the timestamp header.
type Payload struct {
	Version    int       `json:"version"`
	DeliveryId uuid.UUID `json:"deliveryId"`
	// CorrelationId follows one piece of work through every system: the
	// agent's run id when it gave one, otherwise the proposal's id. The
	// agent knows it either way. It is a value to join logs on; what
	// identifies the work is the proposal's id and the agent's.
	CorrelationId string    `json:"correlationId"`
	TenantId      uuid.UUID `json:"tenantId"`
	// CreatedAt is when the delivery was recorded, in the transaction of
	// the decision.
	CreatedAt string          `json:"createdAt"`
	Proposal  PayloadProposal `json:"proposal"`
	Decision  PayloadDecision `json:"decision"`
}

type PayloadProposal struct {
	Id      uuid.UUID       `json:"id"`
	Action  string          `json:"action"`
	Title   string          `json:"title"`
	Payload json.RawMessage `json:"payload"`
	Risk    string          `json:"risk"`
	AgentId uuid.UUID       `json:"agentId"`
	// RunId is the agent's own id for a chain of work, when it gave one.
	// It is the agent's word: a label to group by, not an identity.
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
	decidedAt := *proposal.DecidedAt
	return Payload{
		Version: PayloadVersion, DeliveryId: delivery.Id, CorrelationId: correlationId, TenantId: tenantId,
		CreatedAt: delivery.CreatedAt.UTC().Format(timeFormat),
		Proposal: PayloadProposal{Id: proposal.Id, Action: proposal.Action, Title: proposal.Title, Payload: proposal.Payload, Risk: proposal.Risk,
			AgentId: proposal.AgentId, RunId: proposal.RunId, ProposedAt: proposal.CreatedAt},
		Decision: PayloadDecision{Status: proposal.Status, DecidedAt: decidedAt, DecidedBy: proposal.DecidedBy, Note: proposal.DecisionNote,
			Edited: proposal.Edited, ExecutionBasis: proposal.ExecutionBasis},
	}, nil
}

type Service struct {
	pool         *pgxpool.Pool
	queue        *jobs.Queue
	allowPrivate bool
	proposals    *proposals.Service
	client       *http.Client
}

// Options tune the deliveries.
type Options struct {
	// AllowPrivate lets connectors live on loopback, private and link-local
	// addresses: for local development and for an on-premise install whose
	// connectors are internal. Off, such addresses are refused, so a tenant
	// cannot make the server call into its own network.
	AllowPrivate bool
}

// errBlocked is returned by the dialer for an address a connector may not have.
var errBlocked = errors.New("the connector address is private or local")

// errPlain is a connector on a public address without TLS: the decision
// and its signature would cross the internet in the clear.
var errPlain = errors.New("the connector is plain http on a public address; use https")

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
		Timeout:       requestTimeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: requestTimeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// blockedAddr says whether a connector may not live at an address: this
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
	s := &Service{pool: pool, queue: queue, proposals: proposalsService, client: newClient(opts.AllowPrivate), allowPrivate: opts.AllowPrivate}
	if queue != nil {
		river.AddWorker(queue.Workers(), &worker{service: s})
	}
	return s
}

// Enqueue records a delivery for a decided proposal in the caller's
// transaction and queues its job, so the delivery exists if and only if the
// decision was committed.
func (s *Service) Enqueue(ctx context.Context, tx pgx.Tx, proposal proposals.Proposal, url string) error {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return errors.New("deliveries: no tenant in context")
	}
	id := uuid.New()
	if err := repo.New(tx).InsertDelivery(ctx, repo.InsertDeliveryParams{
		Id: id, TenantId: tenantId, ProposalId: proposal.Id, ActionKey: proposal.Action, Url: url,
	}); err != nil {
		return err
	}
	if s.queue == nil {
		// Without a queue nothing sends it by itself: an embedded host
		// that runs no queue calls Deliver for the row.
		slog.WarnContext(ctx, "a delivery was recorded but there is no job queue to send it", "delivery", id, "proposal", proposal.Id)
		return nil
	}
	return s.queue.InsertTx(ctx, tx, DeliverArgs{TenantId: tenantId, DeliveryId: id},
		&river.InsertOpts{Queue: jobs.QueueDeliveries, MaxAttempts: MaxAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight}})
}

// Retarget moves the action's waiting deliveries to its new connector URL,
// in the caller's transaction, which is the owner's change of the action.
// Each move is written to the chain under its proposal.
func (s *Service) Retarget(ctx context.Context, tx pgx.Tx, actionKey, url string) error {
	moved, err := repo.New(tx).RetargetPending(ctx, repo.RetargetPendingParams{Url: url, ActionKey: actionKey})
	if err != nil {
		return err
	}
	if len(moved) == 0 {
		return nil
	}
	list := make([]events.Event, 0, len(moved))
	for _, m := range moved {
		list = append(list, retargetEvent(actionKey, url, m.Id, m.ProposalId))
	}
	_, err = events.AppendMany(ctx, tx, list)
	return err
}

// retargeted records that one delivery now goes to the action's new URL.
func retargeted(ctx context.Context, tx pgx.Tx, actionKey, url string, deliveryId, proposalId uuid.UUID) error {
	_, err := events.Append(ctx, tx, retargetEvent(actionKey, url, deliveryId, proposalId))
	return err
}

func retargetEvent(actionKey, url string, deliveryId, proposalId uuid.UUID) events.Event {
	return events.Event{
		Type: "delivery.retargeted", SubjectId: proposalId,
		Payload: map[string]any{"deliveryId": deliveryId, "action": actionKey, "connectorUrlSha256": urlDigest(url)},
	}
}

// urlDigest names a connector address in the chain without its text,
// which may carry credentials.
func urlDigest(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// DeliverArgs is the job that delivers one decision.
type DeliverArgs struct {
	TenantId   uuid.UUID `json:"tenantId"`
	DeliveryId uuid.UUID `json:"deliveryId"`
}

func (DeliverArgs) Kind() string { return "mandate.deliver" }

type worker struct {
	river.WorkerDefaults[DeliverArgs]
	service *Service
}

func (w *worker) Timeout(*river.Job[DeliverArgs]) time.Duration {
	return requestTimeout + 10*time.Second
}

func (w *worker) Work(ctx context.Context, job *river.Job[DeliverArgs]) error {
	return w.service.Deliver(ctx, job.Args.TenantId, job.Args.DeliveryId, job.Attempt >= job.MaxAttempts)
}

// Deliver makes one attempt, as the system. A 2xx answer marks the
// delivery delivered and keeps the answer as the result. Anything else is
// returned as an error for the job to retry; on the last attempt, on a 4xx
// that a retry cannot change, or on an address that may not be called, the
// delivery is marked failed. Delivered and failed are written to the chain
// under the proposal.
func (s *Service) Deliver(ctx context.Context, tenantId, deliveryId uuid.UUID, lastAttempt bool) error {
	ctx = actor.AsSystem(ctx, tenantId)
	// The delivery, the action's address and its secret are read together:
	// the body goes where the action points now, signed with the secret
	// that belongs to that address. A delivery left behind by a move, say
	// one recorded while the owner was moving the action, is moved here.
	var (
		delivery Delivery
		secret   []byte
		lost     error
	)
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		row, err := q.GetDeliveryForUpdate(ctx, deliveryId)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "delivery not found")
		}
		if err != nil {
			return err
		}
		delivery = toDelivery(row)
		if delivery.Status != StatusPending {
			return nil
		}
		connector, err := q.ConnectorOf(ctx, delivery.ActionKey)
		if errors.Is(err, pgx.ErrNoRows) {
			// The action is no longer a webhook, or is removed.
			lost = errors.New("the action no longer has a webhook connector")
			return nil
		}
		if err != nil {
			return err
		}
		secret = connector.Secret
		if connector.Url != delivery.Url {
			if err := q.MoveDelivery(ctx, repo.MoveDeliveryParams{Id: delivery.Id, Url: connector.Url}); err != nil {
				return err
			}
			if err := retargeted(ctx, tx, delivery.ActionKey, connector.Url, delivery.Id, delivery.ProposalId); err != nil {
				return err
			}
			delivery.Url = connector.Url
		}
		return nil
	}); err != nil {
		if fault.CodeOf(err) == fault.NotFound {
			return nil
		}
		return err
	}
	if delivery.Status != StatusPending {
		return nil
	}
	if lost != nil {
		// Final: no retry can change it. The job leaves the queue now, so a
		// later "send again" is the only thing that sends it.
		err := s.failed(ctx, delivery, lost, "the action no longer has a webhook connector", 0, true)
		if !lastAttempt {
			return river.JobCancel(err)
		}
		return err
	}
	proposal, err := s.proposals.Get(ctx, delivery.ProposalId)
	if err != nil {
		return s.failed(ctx, delivery, err, "the proposal could not be read", 0, lastAttempt)
	}
	payload, err := payloadOf(tenantId, delivery, proposal)
	if err != nil {
		// Nothing is sent that the published body does not describe.
		return s.failed(ctx, delivery, err, "the proposal has no decision to deliver", 0, lastAttempt)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// The call leaves the database, so it happens outside any transaction.
	status, result, callErr := s.post(ctx, delivery.Url, secret, delivery.Id, body)
	if callErr == nil {
		err := s.delivered(ctx, delivery, status, body, result)
		if err != nil && len(result) > 0 {
			// The connector did its part. If its answer cannot be kept,
			// the delivery is still a delivery, without a result.
			slog.WarnContext(ctx, "a connector's answer could not be kept as the result", "delivery", delivery.Id, "error", err)
			err = s.delivered(ctx, delivery, status, body, nil)
		}
		return err
	}
	// A connector that says the request itself is wrong will say so again,
	// and an address that may not be called stays that way: only a timeout
	// or a rate limit among the 4xx answers is worth a retry.
	// A 401 is retried too: after a secret is rotated, the receiver answers
	// 401 until it has the new one, and the delivery should survive that.
	final := lastAttempt || errors.Is(callErr, errBlocked) || errors.Is(callErr, errPlain) ||
		(status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests && status != http.StatusUnauthorized)
	err = s.failed(ctx, delivery, callErr, describe(callErr), status, final)
	if final && !lastAttempt {
		// The job must leave the queue now, or sending it again would wait
		// behind a retry that has nothing left to do.
		return river.JobCancel(err)
	}
	return err
}

// delivered records an accepted delivery and, when there is one, its result.
func (s *Service) delivered(ctx context.Context, delivery Delivery, status int, body []byte, result json.RawMessage) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := repo.New(tx).MarkDelivered(ctx, repo.MarkDeliveredParams{Id: delivery.Id, LastStatus: int32(status), Result: result}); err != nil { //nolint:gosec // an HTTP status
			return err
		}
		// The chain holds digests, not text: of the body that was signed
		// and sent (the exact bytes), and of what the connector answered
		// (its canonical form, as kept in the table).
		bodySum := sha256.Sum256(body)
		payload := map[string]any{
			"deliveryId": delivery.Id, "action": delivery.ActionKey, "status": status, "attempts": delivery.Attempts + 1,
			"bodySha256": hex.EncodeToString(bodySum[:]), "connectorUrlSha256": urlDigest(delivery.Url),
		}
		if len(result) > 0 {
			digest, err := events.Digest(result)
			if err != nil {
				return err
			}
			payload["resultSha256"] = digest
		}
		_, err := events.Append(ctx, tx, events.Event{Type: "delivery.delivered", SubjectId: delivery.ProposalId, Payload: payload})
		return err
	})
}

// describe says what went wrong in words that are safe to keep and show:
// never the address, which may carry credentials, nor what the server could
// reach inside its own network.
func describe(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, errBlocked):
		return "blocked: " + errBlocked.Error()
	case errors.Is(err, errPlain):
		return "blocked: " + errPlain.Error()
	case errors.As(err, new(*statusError)):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return "the connector did not answer in time"
	}
	return "the connector could not be reached"
}

// statusError is a connector's answer outside 2xx.
type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("connector answered %d", e.status) }

// Redeliver sends a failed delivery again, as a person of the tenant: back
// to pending, to the action's connector as it is defined now, with a new
// job. The attempts keep counting.
func (s *Service) Redeliver(ctx context.Context, proposalId uuid.UUID) (Delivery, error) {
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
		row, err := q.GetDeliveryByProposal(ctx, proposalId)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "the proposal has no delivery")
		}
		if err != nil {
			return err
		}
		if row.Status != StatusFailed {
			return fault.New(fault.Conflict, "only a failed delivery is sent again")
		}
		url, err := q.ConnectorUrl(ctx, row.ActionKey)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && url == nil) {
			return fault.New(fault.InvalidArgument, "the action has no webhook connector now")
		}
		if err != nil {
			return err
		}
		if err := q.RetryDelivery(ctx, repo.RetryDeliveryParams{Id: row.Id, Url: *url}); err != nil {
			return err
		}
		if _, err := events.Append(ctx, tx, events.Event{
			Type: "delivery.retried", SubjectId: proposalId, Payload: map[string]any{"deliveryId": row.Id, "action": row.ActionKey},
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
		// No uniqueness here: the job that failed may still be finishing in
		// the queue, and a duplicate check would drop this one and leave the
		// delivery waiting with nothing to send it. Only a failed delivery
		// can be sent again, so two of these cannot both pass.
		return s.queue.InsertTx(ctx, tx, DeliverArgs{TenantId: tenantId, DeliveryId: row.Id},
			&river.InsertOpts{Queue: jobs.QueueDeliveries, MaxAttempts: MaxAttempts})
	})
	return result, fault.FromDb(err)
}

// failed records an attempt that did not deliver: as one more attempt, or
// on the last attempt as the delivery's final failure, in the chain.
func (s *Service) failed(ctx context.Context, delivery Delivery, callErr error, message string, status int, lastAttempt bool) error {
	var lastStatus *int32
	if status != 0 {
		code := int32(status) //nolint:gosec // an HTTP status
		lastStatus = &code
	}
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if !lastAttempt {
			return q.MarkAttempt(ctx, repo.MarkAttemptParams{Id: delivery.Id, LastStatus: lastStatus, LastError: &message})
		}
		if err := q.MarkFailed(ctx, repo.MarkFailedParams{Id: delivery.Id, LastStatus: lastStatus, LastError: &message}); err != nil {
			return err
		}
		_, err := events.Append(ctx, tx, events.Event{
			Type: "delivery.failed", SubjectId: delivery.ProposalId,
			Payload: map[string]any{"deliveryId": delivery.Id, "action": delivery.ActionKey, "status": status, "attempts": delivery.Attempts + 1, "connectorUrlSha256": urlDigest(delivery.Url)},
		})
		return err
	}); err != nil {
		return errors.Join(callErr, err)
	}
	if lastAttempt {
		slog.ErrorContext(ctx, "delivery failed for good", "delivery", delivery.Id, "proposal", delivery.ProposalId, "error", callErr)
	}
	return fmt.Errorf("deliver %s: %w", delivery.Id, callErr)
}

// post signs and sends the body. It returns the HTTP status (0 when no
// answer came) and nil when the connector accepted the delivery.
func (s *Service) post(ctx context.Context, url string, secret []byte, deliveryId uuid.UUID, body []byte) (int, json.RawMessage, error) {
	// Plain http is for local and private connectors, which only an
	// operator who allows them can use.
	if !s.allowPrivate && !strings.HasPrefix(strings.ToLower(url), "https://") {
		return 0, nil, errPlain
	}
	timestamp := time.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pgmandate")
	req.Header.Set(sign.HeaderDeliveryId, deliveryId.String())
	req.Header.Set(sign.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	req.Header.Set(sign.HeaderSignature, sign.Signature(secret, timestamp, body))
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, nil, &statusError{status: resp.StatusCode}
	}
	// The answer is kept as the result when it is a JSON object that fits;
	// anything else is a delivery without a result.
	var object map[string]any
	if len(answer) > maxResponse || json.Unmarshal(answer, &object) != nil || object == nil {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, answer, nil
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
		Id: row.Id, ProposalId: row.ProposalId, ActionKey: row.ActionKey, Url: row.Url, Status: row.Status, Attempts: int(row.Attempts),
		CreatedAt: row.CreatedAt, DeliveredAt: row.DeliveredAt, FailedAt: row.FailedAt,
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
