package deliveries_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

type DeliveriesSuite struct {
	IntegrationSuite
}

func TestDeliveriesSuite(t *testing.T) {
	suite.Run(t, new(DeliveriesSuite))
}

type host struct {
	receiver *webhook.Receiver
	server   *httptest.Server
	secret   string
}

// defineWebhook defines tickets.create as a webhook to a fresh receiver.
func (s *DeliveriesSuite) defineWebhook(t testfix.Tenant, mayExecute bool) host {
	s.T().Helper()
	receiver := webhook.NewReceiver("")
	server := httptest.NewServer(receiver)
	s.T().Cleanup(server.Close)
	_, secret, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "tickets.create", Description: "Open a ticket", Risk: mandates.RiskLow, CostUnits: 1, MayExecute: mayExecute,
		Schema:    json.RawMessage(`{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`),
		Connector: actions.ConnectorWebhook, Url: server.URL + "/hooks",
	})
	s.Require().NoError(err)
	receiver.SetSecret(secret)
	return host{receiver: receiver, server: server, secret: secret}
}

func (s *DeliveriesSuite) propose(t testfix.Tenant, subject string) proposals.Proposal {
	s.T().Helper()
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: subject, Payload: json.RawMessage(`{"subject":"` + subject + `"}`)})
	s.Require().NoError(err)
	return p
}

func (s *DeliveriesSuite) Test_AnApprovalIsDelivered() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	p := s.propose(t, "Printer jams")
	_, found, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().False(found, "nothing is delivered before a decision")

	approved, err := s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{Note: "ok"})
	s.Require().NoError(err)
	delivery, found, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().True(found, "the delivery is recorded in the approval's transaction")
	s.Require().Equal(deliveries.StatusPending, delivery.Status)
	s.Require().Empty(h.receiver.Received())

	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))
	received := h.receiver.Received()
	s.Require().Len(received, 1)
	s.Require().Equal(delivery.Id, received[0].DeliveryId)
	s.Require().Equal(p.Id, received[0].Payload.Proposal.Id)
	s.Require().Equal("tickets.create", received[0].Payload.Proposal.Action)
	s.Require().JSONEq(`{"subject":"Printer jams"}`, string(received[0].Payload.Proposal.Payload))
	s.Require().Equal(proposals.StatusApproved, received[0].Payload.Decision.Status)
	s.Require().Equal(&t.ReviewerId, received[0].Payload.Decision.DecidedBy)
	s.Require().Equal("ok", received[0].Payload.Decision.Note)
	s.Require().Equal(t.Id, received[0].Payload.TenantId)
	s.Require().Equal(approved.Id, received[0].Payload.Proposal.Id)

	after, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusDelivered, after.Status)
	s.Require().Equal(1, after.Attempts)
	s.Require().NotNil(after.DeliveredAt)
	s.Require().JSONEq(`{"reference":"REF-`+p.Id.String()[:8]+`","action":"tickets.create"}`, string(after.Result), "the connector's answer is kept as the result")
	s.Require().Equal([]string{"proposal.created", "proposal.approved", "delivery.delivered"}, s.eventTypes(t.Owner, p.Id))
	history, err := s.W.Kernel.Proposals.History(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Contains(string(history[2].Payload), "resultSha256", "the chain holds the result's hash")
	s.Require().NotContains(string(history[2].Payload), "REF-", "and not the result")
	var deliveredEvent map[string]any
	s.Require().NoError(json.Unmarshal(history[2].Payload, &deliveredEvent))
	bodySum := sha256.Sum256(received[0].Body)
	s.Require().Equal(hex.EncodeToString(bodySum[:]), deliveredEvent["bodySha256"], "the chain names the exact body that was signed and sent")
	resultDigest, err := events.Digest(after.Result)
	s.Require().NoError(err)
	s.Require().Equal(resultDigest, deliveredEvent["resultSha256"])
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false), "a delivered delivery is left alone")
	s.Require().Len(h.receiver.Received(), 1)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, uuid.New(), false), "an unknown id is not a failure")
}

// bodies records the raw body of each delivery to a connector that accepts
// everything.
type bodies struct {
	mu       sync.Mutex
	list     [][]byte
	failNext bool
}

func (b *bodies) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	b.mu.Lock()
	b.list = append(b.list, body)
	fail := b.failNext
	b.failNext = false
	b.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *bodies) all() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.list)
}

// undeclared lists the fields of a value that a schema's properties do not
// name, so a field added to the body without the published schema fails.
func undeclared(path string, value, schema map[string]any) []string {
	var missing []string
	properties, _ := schema["properties"].(map[string]any)
	for key, inner := range value {
		declared, ok := properties[key].(map[string]any)
		if !ok {
			missing = append(missing, path+key)
			continue
		}
		if object, ok := inner.(map[string]any); ok && declared["properties"] != nil {
			missing = append(missing, undeclared(path+key+".", object, declared)...)
		}
	}
	return missing
}

func (s *DeliveriesSuite) Test_TheBodyIsThePublishedContract() {
	t := s.W.NewTenant(s.T(), "Acme")
	recorded := &bodies{}
	server := httptest.NewServer(recorded)
	defer server.Close()
	_, _, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "tickets.create", Risk: mandates.RiskLow, CostUnits: 2, MayExecute: true, Connector: actions.ConnectorWebhook, Url: server.URL,
		Schema: json.RawMessage(`{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`),
	})
	s.Require().NoError(err)
	raw, err := os.ReadFile("../../docs/delivery-v1.schema.json")
	s.Require().NoError(err)
	contract, err := actions.Compile(raw)
	s.Require().NoError(err)
	var contractDoc map[string]any
	s.Require().NoError(json.Unmarshal(raw, &contractDoc))
	// The times are the database's, whose clock may be a little off ours.
	started := time.Now().UTC().Add(-10 * time.Second)

	// An approval, with no run id: the proposal's id correlates.
	approved := s.propose(t, "Printer jams")
	_, err = s.W.Kernel.Proposals.Approve(t.Reviewer, approved.Id, proposals.Decision{Note: "ok"})
	s.Require().NoError(err)
	// An execution under a mandate, in a run the agent named.
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 5, PolicyRef: "P-1", Days: 30, Condition: `payload.subject != ""`})
	s.Require().NoError(err)
	inRun, _, err := s.W.Kernel.Proposals.Propose(actor.WithRun(t.Agent, "run-42"), proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: "Toner", Payload: json.RawMessage(`{"subject":"Toner"}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, inRun.Id))

	for _, id := range []uuid.UUID{approved.Id, inRun.Id} {
		delivery, found, err := s.W.Kernel.Deliveries.Get(t.Owner, id)
		s.Require().NoError(err)
		s.Require().True(found)
		s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))
	}
	got := recorded.all()
	s.Require().Len(got, 2)
	payloads := make([]webhook.Payload, len(got))
	for i, body := range got {
		var value map[string]any
		s.Require().NoError(json.Unmarshal(body, &value))
		s.Require().NoError(contract.Validate(value), string(body))
		s.Require().Empty(undeclared("", value, contractDoc), "every field of the body is in the published schema")
		s.Require().NoError(json.Unmarshal(body, &payloads[i]), "the reference receiver reads it")
	}

	first, second := payloads[0], payloads[1]
	s.Require().Equal(deliveries.PayloadVersion, first.Version)
	s.Require().Equal(webhook.Version, first.Version)
	s.Require().Equal(approved.Id.String(), first.CorrelationId, "without a run, the proposal's id correlates")
	s.Require().Empty(first.Proposal.RunId)
	s.Require().NotContains(string(got[0]), "runId", "no run id is left out, not sent empty")
	s.Require().Equal("run-42", second.CorrelationId, "the agent's run id correlates")
	s.Require().Equal("run-42", second.Proposal.RunId)
	s.Require().Equal(proposals.StatusApproved, first.Decision.Status)
	s.Require().Equal(&t.ReviewerId, first.Decision.DecidedBy)
	s.Require().Nil(first.Decision.ExecutionBasis)
	s.Require().Equal(proposals.StatusExecuted, second.Decision.Status)
	s.Require().Nil(second.Decision.DecidedBy)
	s.Require().NotNil(second.Decision.ExecutionBasis)
	s.Require().Equal("P-1", second.Decision.ExecutionBasis.PolicyRef)
	s.Require().Equal(`payload.subject != ""`, second.Decision.ExecutionBasis.Condition, "the receiver sees the condition that held")
	for _, p := range payloads {
		for name, at := range map[string]time.Time{"createdAt": p.CreatedAt, "proposedAt": p.Proposal.ProposedAt, "decidedAt": p.Decision.DecidedAt} {
			s.Require().False(at.Before(started), name)
			s.Require().WithinDuration(time.Now(), at, time.Minute, name)
			s.Require().Equal(time.UTC, at.Location(), name)
		}
		s.Require().False(p.Decision.DecidedAt.Before(p.Proposal.ProposedAt), "decided after it was proposed")
		s.Require().False(p.CreatedAt.Before(p.Decision.DecidedAt.Truncate(time.Second)), "recorded with the decision")
	}

	// The body is the same on every attempt: a retry repeats it.
	retried := s.propose(t, "Again")
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, retried.Id, proposals.Decision{})
	s.Require().NoError(err)
	again, _, err := s.W.Kernel.Deliveries.Get(t.Owner, retried.Id)
	s.Require().NoError(err)
	recorded.mu.Lock()
	recorded.failNext = true
	recorded.mu.Unlock()
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, again.Id, false), "the first attempt is refused")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, again.Id, false))
	got = recorded.all()
	s.Require().Len(got, 4)
	s.Require().Equal(string(got[2]), string(got[3]))
}

func (s *DeliveriesSuite) Test_TheReceiverRefusesWhatItShouldNot() {
	receiver := webhook.NewReceiver("mws_0123456789abcdef0123456789abcdef")
	server := httptest.NewServer(receiver)
	defer server.Close()
	post := func(body string) int {
		return s.postSigned(server.URL, "mws_0123456789abcdef0123456789abcdef", body)
	}
	s.Require().Equal(http.StatusOK, post(`{"version":1,"deliveryId":"ID","proposal":{"id":"ID","action":"a.b"},"decision":{"status":"approved"}}`))
	s.Require().Equal(http.StatusBadRequest, post(`{"version":2,"deliveryId":"ID","proposal":{"id":"ID","action":"a.b"},"decision":{"status":"approved"}}`))
	s.Require().Equal(http.StatusBadRequest, post(`{"deliveryId":"ID","proposal":{"id":"ID","action":"a.b"},"decision":{"status":"approved"}}`), "a body with no version is not version 1")
	s.Require().Equal(http.StatusForbidden, post(`{"version":1,"deliveryId":"ID","proposal":{"id":"ID","action":"a.b"},"decision":{"status":"pending"}}`), "only a decision is carried out")
	s.Require().Len(receiver.Received(), 1)

	// A secret bound to one tenant's action signs for nothing else.
	tenantId := uuid.New()
	bound := webhook.NewReceiver("")
	bound.AddSecretFor("mws_bound_secret_0123456789abcdef", tenantId, "tickets.create")
	boundServer := httptest.NewServer(bound)
	defer boundServer.Close()
	body := func(tenant uuid.UUID, action string) string {
		return `{"version":1,"deliveryId":"ID","tenantId":"` + tenant.String() + `","proposal":{"id":"ID","action":"` + action + `"},"decision":{"status":"executed"}}`
	}
	s.Require().Equal(http.StatusOK, s.postSigned(boundServer.URL, "mws_bound_secret_0123456789abcdef", body(tenantId, "tickets.create")))
	s.Require().Equal(http.StatusForbidden, s.postSigned(boundServer.URL, "mws_bound_secret_0123456789abcdef", body(tenantId, "refunds.issue")))
	s.Require().Equal(http.StatusForbidden, s.postSigned(boundServer.URL, "mws_bound_secret_0123456789abcdef", body(uuid.New(), "tickets.create")))
	s.Require().Len(bound.Received(), 1)
}

// postSigned posts a body signed with the secret under a fresh delivery id,
// which replaces every ID in the body, and returns the status.
func (s *DeliveriesSuite) postSigned(url, secret, body string) int {
	s.T().Helper()
	id := uuid.New()
	body = strings.ReplaceAll(body, "ID", id.String())
	now := time.Now().Unix()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	s.Require().NoError(err)
	req.Header.Set(sign.HeaderDeliveryId, id.String())
	req.Header.Set(sign.HeaderTimestamp, strconv.FormatInt(now, 10))
	req.Header.Set(sign.HeaderSignature, sign.Signature([]byte(secret), now, []byte(body)))
	res, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode
}

func (s *DeliveriesSuite) Test_AnExecutionIsDeliveredWithItsBasis() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, true)
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 5, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	p := s.propose(t, "Keyboard missing")
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, p.Id))
	delivery, found, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().True(found)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))
	received := h.receiver.Received()
	s.Require().Len(received, 1)
	s.Require().Equal(proposals.StatusExecuted, received[0].Payload.Decision.Status)
	s.Require().Nil(received[0].Payload.Decision.DecidedBy)
	s.Require().NotNil(received[0].Payload.Decision.ExecutionBasis)
	s.Require().Equal(m.Id, received[0].Payload.Decision.ExecutionBasis.MandateId)
	byProposal, err := s.W.Kernel.Deliveries.ByProposals(t.Owner, []uuid.UUID{p.Id, uuid.New()})
	s.Require().NoError(err)
	s.Require().Len(byProposal, 1)
}

func (s *DeliveriesSuite) Test_FailuresRetryAndFinallyFail() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	p := s.propose(t, "Monitor flickers")
	_, err := s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.Require().NoError(err)
	delivery, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)

	h.receiver.FailNext(1)
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))
	after, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusPending, after.Status)
	s.Require().Equal(1, after.Attempts)
	s.Require().Equal(500, *after.LastStatus)
	s.Require().Contains(after.LastError, "500")
	s.Require().Equal([]string{"proposal.created", "proposal.approved"}, s.eventTypes(t.Owner, p.Id), "a retryable failure is not in the chain")

	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false), "the retry succeeds")
	after, _, err = s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusDelivered, after.Status)
	s.Require().Equal(2, after.Attempts)

	// A wrong secret is refused by the receiver; the last attempt fails for good.
	other := s.propose(t, "Coffee machine")
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, other.Id, proposals.Decision{})
	s.Require().NoError(err)
	otherDelivery, _, err := s.W.Kernel.Deliveries.Get(t.Owner, other.Id)
	s.Require().NoError(err)
	// A 401 is retried, since a receiver answers it while a rotated secret
	// reaches it; it fails for good only on the last attempt.
	h.receiver.SetSecret("mws_somethingelse_somethingelse")
	err = s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, otherDelivery.Id, false)
	s.Require().Error(err)
	var notCancelled *river.JobCancelError
	s.Require().False(errors.As(err, &notCancelled), "a 401 leaves the job to retry")
	retrying, _, err := s.W.Kernel.Deliveries.Get(t.Owner, other.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusPending, retrying.Status)
	s.Require().Equal(401, *retrying.LastStatus)
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, otherDelivery.Id, true))
	failed, _, err := s.W.Kernel.Deliveries.Get(t.Owner, other.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusFailed, failed.Status)
	s.Require().Equal(401, *failed.LastStatus)
	s.Require().NotNil(failed.FailedAt)
	s.Require().Equal([]string{"proposal.created", "proposal.approved", "delivery.failed"}, s.eventTypes(t.Owner, other.Id))
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, otherDelivery.Id, false), "final is final")
	decided, err := s.W.Kernel.Proposals.Get(t.Owner, other.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusApproved, decided.Status, "the decision stands whatever the connector did")

	// A person sends the failed delivery again, once the receiver is right.
	_, err = s.W.Kernel.Deliveries.Redeliver(t.Agent, other.Id)
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Deliveries.Redeliver(t.Reviewer, p.Id)
	s.code(err, fault.Conflict)
	h.receiver.SetSecret(h.secret)
	again, err := s.W.Kernel.Deliveries.Redeliver(t.Reviewer, other.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusPending, again.Status)
	s.Require().Nil(again.FailedAt)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, otherDelivery.Id, false))
	resent, _, err := s.W.Kernel.Deliveries.Get(t.Owner, other.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusDelivered, resent.Status)
	s.Require().Equal(3, resent.Attempts, "the attempts keep counting")
	s.Require().Equal([]string{"proposal.created", "proposal.approved", "delivery.failed", "delivery.retried", "delivery.delivered"}, s.eventTypes(t.Owner, other.Id))

	// The receiver saw a delivery twice in the retry above and did it once.
	seen := 0
	for _, r := range h.receiver.Received() {
		if r.Payload.Proposal.Id == p.Id {
			seen++
		}
	}
	s.Require().Equal(1, seen)

	// A delivery that cannot even be prepared fails the same way: here the
	// action was redefined as builtin and lost its secret.
	stranded := s.propose(t, "Door squeaks")
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, stranded.Id, proposals.Decision{})
	s.Require().NoError(err)
	strandedDelivery, _, err := s.W.Kernel.Deliveries.Get(t.Owner, stranded.Id)
	s.Require().NoError(err)
	_, _, err = s.W.Kernel.Actions.Define(t.Owner, actions.Input{Key: "tickets.create", Risk: mandates.RiskLow, Connector: actions.ConnectorBuiltin})
	s.Require().NoError(err)
	err = s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, strandedDelivery.Id, false)
	var strandedCancel *river.JobCancelError
	s.Require().ErrorAs(err, &strandedCancel, "a connector that is gone fails for good at once, and the job leaves the queue")
	strandedAfter, _, err := s.W.Kernel.Deliveries.Get(t.Owner, stranded.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusFailed, strandedAfter.Status)
	s.Require().Contains(strandedAfter.LastError, "no longer has a webhook connector")
	s.Require().Equal([]string{"proposal.created", "proposal.approved", "delivery.failed"}, s.eventTypes(t.Owner, stranded.Id))
	_, _, err = s.W.Kernel.Actions.Define(t.Owner, actions.Input{Key: "tickets.create", Risk: mandates.RiskLow, Connector: actions.ConnectorWebhook, Url: h.server.URL + "/hooks", Schema: json.RawMessage(`{"type":"object"}`)})
	s.Require().NoError(err)

	// A final refusal takes the job out of the queue, so sending again is
	// not held up behind a retry with nothing left to do.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not for this action", http.StatusForbidden)
	}))
	defer refusing.Close()
	_, _, err = s.W.Kernel.Actions.Define(t.Owner, actions.Input{Key: "refused.always", Risk: mandates.RiskLow, Connector: actions.ConnectorWebhook, Url: refusing.URL, Schema: json.RawMessage(`{"type":"object"}`)})
	s.Require().NoError(err)
	final, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "refused.always", Title: "Refused for good", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, final.Id, proposals.Decision{})
	s.Require().NoError(err)
	finalDelivery, _, err := s.W.Kernel.Deliveries.Get(t.Owner, final.Id)
	s.Require().NoError(err)
	err = s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, finalDelivery.Id, false)
	var cancelled *river.JobCancelError
	s.Require().ErrorAs(err, &cancelled, "a 403 before the last attempt cancels the job")

	// An answer that is large as text but small as JSON text is kept; one
	// that cannot be kept still counts as delivered.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":[` + strings.TrimSuffix(strings.Repeat("1,", 2000), ",") + `]}`))
	}))
	defer big.Close()
	_, bigSecret, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{Key: "numbers.add", Risk: mandates.RiskLow, Connector: actions.ConnectorWebhook, Url: big.URL, Schema: json.RawMessage(`{"type":"object"}`)})
	s.Require().NoError(err)
	s.Require().NotEmpty(bigSecret)
	numbers, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "numbers.add", Title: "numbers", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, numbers.Id, proposals.Decision{})
	s.Require().NoError(err)
	numbersDelivery, _, err := s.W.Kernel.Deliveries.Get(t.Owner, numbers.Id)
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, numbersDelivery.Id, false))
	kept, _, err := s.W.Kernel.Deliveries.Get(t.Owner, numbers.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusDelivered, kept.Status)
	s.Require().NotEmpty(kept.Result, "four thousand bytes of numbers fit")

	// An unreachable connector is a failure without a status.
	h.server.Close()
	gone := s.propose(t, "Lamp broken")
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, gone.Id, proposals.Decision{})
	s.Require().NoError(err)
	goneDelivery, _, err := s.W.Kernel.Deliveries.Get(t.Owner, gone.Id)
	s.Require().NoError(err)
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, goneDelivery.Id, false))
	unreachable, _, err := s.W.Kernel.Deliveries.Get(t.Owner, gone.Id)
	s.Require().NoError(err)
	s.Require().Nil(unreachable.LastStatus)
	s.Require().NotEmpty(unreachable.LastError)
}

// A receiver's reason for refusing is kept with the failed delivery, named
// in the log by digest, for the person and the agent to read. A reason sent
// with an answer that will be retried is not kept: nothing is settled yet.
func (s *DeliveriesSuite) Test_ARefusalKeepsItsReason() {
	t := s.W.NewTenant(s.T(), "Acme")
	status := http.StatusUnprocessableEntity
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"refused":"refused: no such order"}`))
	}))
	defer refusing.Close()
	_, _, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{Key: "refunds.issue", Risk: mandates.RiskLow, Connector: actions.ConnectorWebhook, Url: refusing.URL, Schema: json.RawMessage(`{"type":"object"}`)})
	s.Require().NoError(err)
	deliver := func() deliveries.Delivery {
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "refunds.issue", Title: "Refund", Payload: json.RawMessage(`{}`)})
		s.Require().NoError(err)
		_, err = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
		s.Require().NoError(err)
		d, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
		s.Require().NoError(err)
		s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false))
		after, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
		s.Require().NoError(err)
		return after
	}
	refused := deliver()
	s.Require().Equal(deliveries.StatusFailed, refused.Status)
	s.Require().JSONEq(`{"refused":"refused: no such order"}`, string(refused.Result))
	var failed map[string]any
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		event, ok, err := events.Last(t.Owner, tx, refused.ProposalId, "delivery.failed")
		s.Require().True(ok)
		if err != nil {
			return err
		}
		return json.Unmarshal(event.Payload, &failed)
	}))
	digest, err := events.Digest(refused.Result)
	s.Require().NoError(err)
	s.Require().Equal(digest, failed["resultSha256"])
	s.Require().NotContains(fmt.Sprint(failed), "no such order", "the log holds the reason's digest, not its text")

	status = http.StatusServiceUnavailable
	retried := deliver()
	s.Require().Equal(deliveries.StatusPending, retried.Status)
	s.Require().Empty(retried.Result, "an answer to an attempt that will be retried settles nothing")
}

func (s *DeliveriesSuite) Test_DatabaseGuardsDeliveries() {
	a := s.W.NewTenant(s.T(), "A")
	b := s.W.NewTenant(s.T(), "B")
	s.defineWebhook(a, false)
	p := s.propose(a, "Printer jams")
	_, err := s.W.Kernel.Proposals.Approve(a.Owner, p.Id, proposals.Decision{})
	s.Require().NoError(err)
	delivery, _, err := s.W.Kernel.Deliveries.Get(a.Owner, p.Id)
	s.Require().NoError(err)
	s.refused(a.Owner, `UPDATE mandate.deliveries SET status = 'delivered' WHERE id = $1`, delivery.Id)
	s.refused(a.Agent, `UPDATE mandate.deliveries SET status = 'delivered' WHERE id = $1`, delivery.Id)
	s.refused(a.Agent, `INSERT INTO mandate.deliveries (id, tenant_id, proposal_id, action_key, url) VALUES (gen_random_uuid(), $1, $2, 'x.y', 'https://x')`, a.Id, p.Id)
	s.refused(a.Owner, `DELETE FROM mandate.deliveries WHERE id = $1`, delivery.Id)
	// A delivery is for a decided proposal, to its action's own connector.
	waiting := s.propose(a, "Still waiting")
	insert := `INSERT INTO mandate.deliveries (id, tenant_id, proposal_id, action_key, url) VALUES (gen_random_uuid(), $1, $2, 'tickets.create', $3)`
	s.refused(a.Owner, insert, a.Id, waiting.Id, delivery.Url)
	s.refused(a.System, insert, a.Id, waiting.Id, delivery.Url)
	_, err = s.W.Kernel.Proposals.Approve(a.Owner, waiting.Id, proposals.Decision{})
	s.Require().NoError(err)
	other := s.propose(a, "Somewhere else")
	_, err = s.W.Kernel.Proposals.Reject(a.Owner, other.Id, "no")
	s.Require().NoError(err)
	s.refused(a.Owner, insert, a.Id, other.Id, delivery.Url)
	// Decided, but to somewhere that is not its action's connector.
	second := s.propose(a, "Decided, wrong address")
	_, err = s.W.Kernel.Proposals.Reject(a.Owner, second.Id, "set up")
	s.Require().NoError(err)
	s.refused(a.Owner, insert, a.Id, waiting.Id, "https://attacker.example/x")
	s.refused(a.System, insert, a.Id, waiting.Id, "https://attacker.example/x")
	s.refused(a.System, `UPDATE mandate.deliveries SET url = 'https://attacker.example/x' WHERE id = $1`, delivery.Id)
	s.refused(a.Owner, `UPDATE mandate.deliveries SET status = 'pending' WHERE id = $1`, delivery.Id)
	_, found, err := s.W.Kernel.Deliveries.Get(b.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().False(found)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), b.Id, delivery.Id, false), "B's system cannot see A's delivery")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), a.Id, delivery.Id, false))
	done, _, err := s.W.Kernel.Deliveries.Get(a.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusDelivered, done.Status)
	s.refused(a.System, `UPDATE mandate.deliveries SET status = 'failed' WHERE id = $1`, delivery.Id)
	s.code(s.W.Kernel.Actions.Remove(b.Owner, "tickets.create"), fault.NotFound)
}

// A delivery still waiting when its action moves goes to the new address,
// signed with the new secret: the old receiver no longer holds it.
func (s *DeliveriesSuite) Test_WaitingDeliveriesMoveWithTheirAction() {
	t := s.W.NewTenant(s.T(), "Acme")
	old := s.defineWebhook(t, false)
	p := s.propose(t, "Printer jams")
	_, err := s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.Require().NoError(err)
	waiting, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(old.server.URL+"/hooks", waiting.Url)

	// A reviewer cannot point a delivery anywhere, nor an owner at an
	// address that is not the action's.
	s.refused(t.Reviewer, `UPDATE mandate.deliveries SET url = 'https://elsewhere.example/hooks' WHERE id = $1`, waiting.Id)
	s.refused(t.Owner, `UPDATE mandate.deliveries SET url = 'https://elsewhere.example/hooks' WHERE id = $1`, waiting.Id)

	moved := webhook.NewReceiver("")
	movedServer := httptest.NewServer(moved)
	defer movedServer.Close()
	_, secret, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "tickets.create", Description: "Open a ticket", Risk: mandates.RiskLow, CostUnits: 1,
		Schema:    json.RawMessage(`{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`),
		Connector: actions.ConnectorWebhook, Url: movedServer.URL + "/hooks",
	})
	s.Require().NoError(err)
	s.Require().NotEmpty(secret, "a moved connector gets a new secret")
	moved.SetSecret(secret)

	after, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(movedServer.URL+"/hooks", after.Url, "the waiting delivery moved with its action")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, waiting.Id, false))
	s.Require().Len(moved.Received(), 1, "it arrives at the new receiver, which verifies the new secret")
	s.Require().Empty(old.receiver.Received())
	s.Require().Equal([]string{"proposal.created", "proposal.approved", "delivery.retargeted", "delivery.delivered"}, s.eventTypes(t.Owner, p.Id))
	history, err := s.W.Kernel.Proposals.History(t.Owner, p.Id)
	s.Require().NoError(err)
	var approved map[string]any
	s.Require().NoError(json.Unmarshal(history[1].Payload, &approved))
	sum := sha256.Sum256([]byte(old.server.URL + "/hooks"))
	s.Require().Equal(hex.EncodeToString(sum[:]), approved["connectorUrlSha256"], "the approval names the connector it was made against")
}

// A delivery recorded at the old address while the owner moved the action
// is found behind when it is sent: it goes to the new address, signed with
// the new secret, and the chain says so.
func (s *DeliveriesSuite) Test_ADeliveryLeftBehindIsSentWhereTheActionIsNow() {
	t := s.W.NewTenant(s.T(), "Acme")
	old := s.defineWebhook(t, false)
	p := s.propose(t, "Printer jams")
	_, err := s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.Require().NoError(err)
	waiting, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)

	// The move as the race leaves it: the action points elsewhere with a new
	// secret, and the delivery was not moved with it.
	moved := webhook.NewReceiver("mws_moved_secret_0123456789abcdef")
	movedServer := httptest.NewServer(moved)
	defer movedServer.Close()
	s.Require().NoError(s.exec(t.Owner, `UPDATE mandate.actions SET connector_url = $1, connector_secret = 'mws_moved_secret_0123456789abcdef'::bytea WHERE key = 'tickets.create'`, movedServer.URL+"/hooks"))
	still, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(old.server.URL+"/hooks", still.Url)

	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, waiting.Id, false))
	s.Require().Empty(old.receiver.Received(), "nothing goes to the old address")
	s.Require().Len(moved.Received(), 1, "the new receiver verifies the new secret")
	after, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusDelivered, after.Status)
	s.Require().Equal(movedServer.URL+"/hooks", after.Url)
	s.Require().Equal([]string{"proposal.created", "proposal.approved", "delivery.retargeted", "delivery.delivered"}, s.eventTypes(t.Owner, p.Id))
	history, err := s.W.Kernel.Proposals.History(t.Owner, p.Id)
	s.Require().NoError(err)
	var delivered map[string]any
	s.Require().NoError(json.Unmarshal(history[3].Payload, &delivered))
	sum := sha256.Sum256([]byte(movedServer.URL + "/hooks"))
	s.Require().Equal(hex.EncodeToString(sum[:]), delivered["connectorUrlSha256"], "the chain names the address it was sent to")
}

// Sending a failed delivery again always gets a job of its own, even while
// the job that failed is still finishing in the queue: a duplicate check
// would drop it and leave the delivery waiting with nothing to send it.
func (s *DeliveriesSuite) Test_SendingAgainIsNeverDroppedAsADuplicate() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	p := s.propose(t, "Printer jams")
	_, err := s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.Require().NoError(err)
	delivery, _, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	h.receiver.FailNext(1)
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, true))

	// A queue that is set up but not run: the failed attempt's job is
	// still there, as it is while River finishes it.
	queue := jobs.NewQueue(s.W.Pg.App)
	service := deliveries.New(s.W.Pg.App, queue, s.W.Kernel.Proposals, deliveries.Options{AllowPrivate: true})
	s.Require().NoError(queue.Init(1))
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		return queue.InsertTx(t.System, tx, deliveries.DeliverArgs{TenantId: t.Id, DeliveryId: delivery.Id},
			&river.InsertOpts{Queue: jobs.QueueDeliveries, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight}})
	}))
	_, err = service.Redeliver(t.Owner, p.Id)
	s.Require().NoError(err)
	var count int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT count(*) FROM river_job WHERE args->>'deliveryId' = $1`, delivery.Id.String()).Scan(&count))
	s.Require().Equal(2, count, "the resend has its own job")
}
