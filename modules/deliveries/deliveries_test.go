package deliveries_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
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

var ticketSchema = json.RawMessage(`{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`)

type host struct {
	receiver *webhook.Receiver
	server   *httptest.Server
	audience string
}

// defineWebhook defines tickets.create as a webhook to a fresh reference
// receiver that trusts the fixture's key and the action's audience.
func (s *DeliveriesSuite) defineWebhook(t testfix.Tenant, mayExecute bool) host {
	s.T().Helper()
	receiver := webhook.NewReceiver(testfix.Keys)
	server := httptest.NewServer(receiver)
	s.T().Cleanup(server.Close)
	defined, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "tickets.create", Description: "Open a ticket", Risk: mandates.RiskLow, MayExecute: mayExecute,
		Schema: ticketSchema, Connector: actions.ConnectorWebhook, Url: server.URL + "/hooks",
	})
	s.Require().NoError(err)
	receiver.Trust(defined.ConnectorAudience, t.Id)
	return host{receiver: receiver, server: server, audience: defined.ConnectorAudience}
}

// defineAt defines tickets.create as a webhook to any address.
func (s *DeliveriesSuite) defineAt(t testfix.Tenant, url string) actions.Action {
	s.T().Helper()
	defined, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "tickets.create", Risk: mandates.RiskLow, Schema: ticketSchema, Connector: actions.ConnectorWebhook, Url: url,
	})
	s.Require().NoError(err)
	return defined
}

// closedUrl is an address where nothing listens.
func closedUrl() string {
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	return url
}

func (s *DeliveriesSuite) propose(t testfix.Tenant, subject string) proposals.Proposal {
	s.T().Helper()
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: subject, Payload: json.RawMessage(`{"subject":"` + subject + `"}`)})
	s.Require().NoError(err)
	return p
}

// approved proposes and approves a ticket, and returns its delivery.
func (s *DeliveriesSuite) approved(t testfix.Tenant, subject string) deliveries.Delivery {
	s.T().Helper()
	p := s.propose(t, subject)
	_, err := s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{Note: "ok"})
	s.Require().NoError(err)
	return s.delivery(t, p.Id)
}

func (s *DeliveriesSuite) delivery(t testfix.Tenant, proposalId uuid.UUID) deliveries.Delivery {
	s.T().Helper()
	d, found, err := s.W.Kernel.Deliveries.Get(t.Owner, proposalId)
	s.Require().NoError(err)
	s.Require().True(found)
	return d
}

// backdate moves a delivery's times by the owner, with the triggers set
// aside, so a test need not wait for a deadline.
func (s *DeliveriesSuite) backdate(id uuid.UUID, set string) {
	s.T().Helper()
	_, err := s.W.Pg.Owner.Exec(context.Background(), `
		BEGIN;
		SET LOCAL session_replication_role = replica;
		UPDATE mandate.deliveries SET `+set+` WHERE id = '`+id.String()+`';
		COMMIT;`)
	s.Require().NoError(err)
}

// openAttempt opens an attempt as the worker does, and leaves it open, as a
// worker that crashed would.
func (s *DeliveriesSuite) openAttempt(t testfix.Tenant, id uuid.UUID) {
	s.T().Helper()
	s.Require().NoError(s.exec(t.System, `UPDATE mandate.deliveries SET attempts_started = attempts_started + 1 WHERE id = $1`, id))
}

func (s *DeliveriesSuite) eventPayload(t testfix.Tenant, subject uuid.UUID, eventType string) map[string]any {
	s.T().Helper()
	var raw []byte
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(),
		`SELECT payload::text FROM mandate.events WHERE tenant_id = $1 AND subject_id = $2 AND type = $3 ORDER BY seq DESC LIMIT 1`,
		t.Id, subject, eventType).Scan(&raw))
	var payload map[string]any
	s.Require().NoError(json.Unmarshal(raw, &payload))
	return payload
}

func (s *DeliveriesSuite) Test_AnApprovalIsDone() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	p := s.propose(t, "Printer jams")
	_, found, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().False(found, "nothing is delivered before a decision")

	approved, err := s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{Note: "ok"})
	s.Require().NoError(err)
	delivery := s.delivery(t, p.Id)
	s.Require().Equal(deliveries.StatusPending, delivery.Status)
	s.Require().Equal(h.audience, delivery.Audience)
	decidedAt, err := time.Parse(time.RFC3339, *approved.DecidedAt)
	s.Require().NoError(err)
	s.Require().WithinDuration(decidedAt.Add(time.Hour), delivery.NotAfter, time.Second, "the decision's time plus the action's window")
	s.Require().Zero(delivery.AttemptsStarted)

	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))
	received := h.receiver.Received()
	s.Require().Len(received, 1)
	s.Require().Equal(delivery.Id, received[0].DeliveryId)
	s.Require().Equal(p.Id, received[0].Payload.Proposal.Id)
	s.Require().Equal(h.audience, received[0].Payload.Audience)
	s.Require().Equal(t.Id, received[0].Payload.TenantId)
	s.Require().Equal(&t.ReviewerId, received[0].Payload.Decision.DecidedBy)

	after := s.delivery(t, p.Id)
	s.Require().Equal(deliveries.StatusDone, after.Status)
	s.Require().Equal(1, after.AttemptsStarted)
	s.Require().Zero(after.AttemptsUnsent)
	s.Require().Nil(after.AttemptOpenAt)
	s.Require().NotNil(after.FinishedAt)
	var result map[string]any
	s.Require().NoError(json.Unmarshal(after.Result, &result))
	s.Require().Equal("REF-"+p.Id.String()[:8], result["reference"], "the receiver's result is kept")
	s.Require().Equal([]string{"proposal.created", "proposal.approved", "delivery.done"}, s.eventTypes(t.Owner, p.Id))
	sum := sha256.Sum256(received[0].Body)
	done := s.eventPayload(t, p.Id, "delivery.done")
	s.Require().Equal(hex.EncodeToString(sum[:]), done["bodySha256"], "the log names the exact body sent")
	s.Require().Equal(h.audience, done["audience"])

	// Once done, nothing more is sent.
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))
	s.Require().Len(h.receiver.Received(), 1)
}

// An approver's edit is what the receiver gets: the edited title and
// payload, not the agent's, with the decision marked edited and its note.
func (s *DeliveriesSuite) Test_AnEditedApprovalDeliversTheEdit() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	p := s.propose(t, "Printer jams")

	approved, err := s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{
		Title: "Printer jams on floor 3", Payload: json.RawMessage(`{"subject":"Printer jams on 3"}`), Note: "checked",
	})
	s.Require().NoError(err)
	s.Require().True(approved.Edited)
	delivery := s.delivery(t, p.Id)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))

	received := h.receiver.Received()
	s.Require().Len(received, 1)
	s.Require().JSONEq(`{"subject":"Printer jams on 3"}`, string(received[0].Payload.Proposal.Payload), "the payload as approved, not as proposed")
	s.Require().Equal("Printer jams on floor 3", received[0].Payload.Proposal.Title)
	s.Require().True(received[0].Payload.Decision.Edited)
	s.Require().Equal("checked", received[0].Payload.Decision.Note)
	s.Require().Equal(deliveries.StatusDone, s.delivery(t, p.Id).Status)
	sum := sha256.Sum256(received[0].Body)
	s.Require().Equal(hex.EncodeToString(sum[:]), s.eventPayload(t, p.Id, "delivery.done")["bodySha256"], "the log names the edited body sent")
}

// recorder records the raw body and headers of each delivery, and answers
// as it is told.
type recorder struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	answer  func() (int, string)
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.headers = append(r.headers, req.Header.Clone())
	answer := r.answer
	r.mu.Unlock()
	status, text := http.StatusOK, `{"outcome":"done","result":{}}`
	if answer != nil {
		status, text = answer()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

func (r *recorder) all() ([][]byte, []http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.bodies), slices.Clone(r.headers)
}

func (r *recorder) setAnswer(answer func() (int, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = answer
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
	recorded := &recorder{}
	server := httptest.NewServer(recorded)
	defer server.Close()
	defined, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "tickets.create", Risk: mandates.RiskLow, MayExecute: true, Connector: actions.ConnectorWebhook, Url: server.URL, Schema: ticketSchema,
	})
	s.Require().NoError(err)
	raw, err := os.ReadFile("../../docs/delivery-v2.schema.json")
	s.Require().NoError(err)
	contract, err := actions.Compile(raw)
	s.Require().NoError(err)
	var contractDoc map[string]any
	s.Require().NoError(json.Unmarshal(raw, &contractDoc))

	approved := s.approved(t, "Printer jams")
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 5, PolicyRef: "P-1", Days: 30, Condition: `payload.subject != ""`})
	s.Require().NoError(err)
	inRun, _, err := s.W.Kernel.Proposals.Propose(actor.WithRun(t.Agent, "run-42"), proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: "Toner", Payload: json.RawMessage(`{"subject":"Toner"}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, inRun.Id))
	executed := s.delivery(t, inRun.Id)
	for _, d := range []deliveries.Delivery{approved, executed} {
		s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false))
	}

	got, headers := recorded.all()
	s.Require().Len(got, 2)
	payloads := make([]webhook.Payload, len(got))
	for i, body := range got {
		var value map[string]any
		s.Require().NoError(json.Unmarshal(body, &value))
		s.Require().NoError(contract.Validate(value), string(body))
		s.Require().Empty(undeclared("", value, contractDoc), "every field of the body is in the published schema")
		s.Require().NoError(json.Unmarshal(body, &payloads[i]), "the reference receiver reads it")
		// Signed by the fixture's key, over the raw body, and for this
		// action's audience and tenant.
		s.Require().NoError(sign.Verify(testfix.Keys, headers[i].Get(sign.HeaderTimestamp), headers[i].Get(sign.HeaderSignature), body, time.Now()))
		_, err := sign.CheckEnvelope(body, headers[i].Get(sign.HeaderDeliveryId), defined.ConnectorAudience, t.Id.String())
		s.Require().NoError(err)
	}
	first, second := payloads[0], payloads[1]
	s.Require().Equal(2, first.Version)
	s.Require().Equal("delivery", first.Kind)
	s.Require().Equal(approved.Id, first.DeliveryId)
	s.Require().WithinDuration(approved.NotAfter, first.NotAfter, time.Second)
	s.Require().Equal(first.Proposal.Id.String(), first.CorrelationId, "without a run, the proposal's id correlates")
	s.Require().NotContains(string(got[0]), "runId", "no run id is left out, not sent empty")
	s.Require().Equal("run-42", second.CorrelationId)
	s.Require().Nil(first.Decision.ExecutionBasis)
	s.Require().NotNil(second.Decision.ExecutionBasis)
	s.Require().Equal(`payload.subject != ""`, second.Decision.ExecutionBasis.Condition, "the receiver sees the condition that held")
	s.Require().Len(second.Decision.ExecutionBasis.Budgets, 1)

	// Within one audience the body is the same on every attempt; only the
	// timestamp and the signature change.
	again := s.approved(t, "Again")
	calls := 0
	recorded.setAnswer(func() (int, string) {
		calls++
		if calls == 1 {
			return http.StatusServiceUnavailable, ``
		}
		return http.StatusOK, `{"outcome":"done"}`
	})
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, again.Id, false))
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, again.Id, false))
	got, _ = recorded.all()
	s.Require().Len(got, 4)
	s.Require().Equal(string(got[2]), string(got[3]))
}

// A delivery for another audience does nothing at the receiver, which says
// so; it is never acted on by a receiver it was not meant for.
func (s *DeliveriesSuite) Test_AReceiverAcceptsOnlyItsOwn() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	stale := webhook.NewReceiver(testfix.Keys)
	stale.Trust(h.audience, t.Id)
	staleServer := httptest.NewServer(stale)
	defer staleServer.Close()
	// The action now points at a receiver still configured with the old
	// audience: it acts on nothing, and the delivery is retried rather than
	// refused, since the receiver may simply not be told the new one yet.
	moved := s.defineAt(t, staleServer.URL)
	s.Require().NotEqual(h.audience, moved.ConnectorAudience, "a move draws a new audience")
	d := s.approved(t, "Printer jams")
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false), "no outcome: tried again")
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusPending, after.Status)
	s.Require().Contains(after.LastError, "403 without an outcome")
	s.Require().Empty(stale.Received())

	// Once told the new audience, it accepts the same delivery.
	stale.Trust(moved.ConnectorAudience, t.Id)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false))
	after = s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusDone, after.Status)
	s.Require().Len(stale.Received(), 1)
	s.Require().Equal(moved.ConnectorAudience, stale.Received()[0].Payload.Audience)
}

// Only an explicit answer is final. Without one the delivery is retried,
// and when the attempts run out it ends unknown if an attempt may have
// reached the receiver, and not sent if none did.
func (s *DeliveriesSuite) Test_WithoutAnAnswerItEndsUnknownOrNotSent() {
	t := s.W.NewTenant(s.T(), "Acme")
	recorded := &recorder{answer: func() (int, string) { return http.StatusNotFound, `<html>a proxy</html>` }}
	server := httptest.NewServer(recorded)
	defer server.Close()
	first := s.defineAt(t, server.URL)
	d := s.approved(t, "Printer jams")
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false), "a 404 without an outcome is retried, never read as refused")
	s.Require().Equal(deliveries.StatusPending, s.delivery(t, d.ProposalId).Status)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, true))
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusUnknown, after.Status, "it reached something that may have acted")
	s.Require().Equal(2, after.AttemptsStarted)
	s.Require().Contains(s.eventTypes(t.Owner, d.ProposalId), "delivery.unknown")
	unknown := s.eventPayload(t, d.ProposalId, "delivery.unknown")
	firstSum := sha256.Sum256([]byte(server.URL))
	s.Require().Equal(hex.EncodeToString(firstSum[:]), unknown["connectorUrlSha256"], "the log names where the possibly sent attempts went")
	s.Require().Equal(first.ConnectorAudience, unknown["audience"])

	// Nothing listening: proven never sent, on every attempt.
	closed := closedUrl()
	second := s.defineAt(t, closed)
	never := s.approved(t, "Toner")
	s.Require().Error(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, never.Id, false))
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, never.Id, true))
	after = s.delivery(t, never.ProposalId)
	s.Require().Equal(deliveries.StatusNotSent, after.Status)
	s.Require().Equal(2, after.AttemptsStarted)
	s.Require().Equal(2, after.AttemptsUnsent)
	s.Require().Contains(s.eventTypes(t.Owner, never.ProposalId), "delivery.not_sent")
	notSent := s.eventPayload(t, never.ProposalId, "delivery.not_sent")
	closedSum := sha256.Sum256([]byte(closed))
	s.Require().Equal(hex.EncodeToString(closedSum[:]), notSent["connectorUrlSha256"])
	s.Require().Equal(second.ConnectorAudience, notSent["audience"])
}

// A receiver the server may not call, plain http on a public address or
// an address inside its own network, ends the delivery not sent on the
// first attempt: no later attempt could be allowed either.
func (s *DeliveriesSuite) Test_ABlockedReceiverEndsAtOnce() {
	t := s.W.NewTenant(s.T(), "Acme")
	strict := deliveries.New(s.W.Pg.App, nil, s.W.Kernel.Proposals, deliveries.Options{AllowPrivate: false, Signer: testfix.Signer})
	for _, c := range []struct{ url, reason string }{
		{"http://connector.example/hooks", "use https"},
		{"https://127.0.0.1:1/hooks", "private or local"},
	} {
		s.defineAt(t, c.url)
		d := s.approved(t, "Printer jams "+c.url)
		s.Require().NoError(strict.Deliver(context.Background(), t.Id, d.Id, false), "%s: not retried", c.url)
		after := s.delivery(t, d.ProposalId)
		s.Require().Equal(deliveries.StatusNotSent, after.Status, c.url)
		s.Require().Equal(1, after.AttemptsStarted, c.url)
		s.Require().Equal(1, after.AttemptsUnsent, c.url)
		s.Require().Contains(after.LastError, c.reason, c.url)
		s.Require().Contains(s.eventTypes(t.Owner, d.ProposalId), "delivery.not_sent", c.url)
	}
}

// A body that cannot be built is our failure, not the receiver's: the job's
// error and the delivery's last error both say so, and nothing is sent.
func (s *DeliveriesSuite) Test_ABodyThatCannotBeBuiltIsNotBlamedOnTheReceiver() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	d := s.approved(t, "Printer jams")
	// A decided proposal that has lost its decision time has no body the
	// contract describes.
	_, err := s.W.Pg.Owner.Exec(context.Background(), `
		BEGIN;
		SET LOCAL session_replication_role = replica;
		UPDATE mandate.proposals SET decided_at = NULL WHERE id = '`+d.ProposalId.String()+`';
		COMMIT;`)
	s.Require().NoError(err)

	err = s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false)
	s.Require().Error(err, "retried")
	s.Require().Contains(err.Error(), "the delivery's body could not be built")
	s.Require().NotContains(err.Error(), "the receiver could not be reached")
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusPending, after.Status)
	s.Require().Equal("the delivery's body could not be built", after.LastError)
	s.Require().Equal(1, after.AttemptsUnsent)

	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, true), "the last attempt finishes it")
	after = s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusNotSent, after.Status)
	s.Require().Equal("the delivery's body could not be built", after.LastError)
	s.Require().Empty(h.receiver.Received())
}

// A refusal is final: nothing was done, and it is never sent again.
func (s *DeliveriesSuite) Test_ARefusalIsFinal() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	h.receiver.Handle = func(webhook.Received) (any, error) {
		return nil, fmt.Errorf("%w: the ticket queue is closed", webhook.ErrRefused)
	}
	d := s.approved(t, "Printer jams")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false))
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusRefused, after.Status)
	s.Require().JSONEq(`{"reason":"refused: the ticket queue is closed"}`, string(after.Result))
	_, err := s.W.Kernel.Deliveries.SendAgain(t.Owner, d.ProposalId)
	s.code(err, fault.Conflict)
	s.refused(t.Owner, `UPDATE mandate.deliveries SET status = 'pending' WHERE id = $1`, d.Id)
}

// A receiver's final answer is kept even when its result is more than the
// database stores as written: a NUL character is replaced, and a number
// Postgres numeric cannot hold, or one too long to spell out, drops the
// result, never the answer.
func (s *DeliveriesSuite) Test_AnAnswerTheDatabaseCannotStoreAsWrittenIsStillFinal() {
	for name, c := range map[string]struct {
		status int
		answer string
		want   string
		result string
	}{
		"done with a nul":                                      {http.StatusOK, `{"outcome":"done","result":{"note":"a\u0000b"}}`, deliveries.StatusDone, `{"note":"a\ufffdb"}`},
		"done with a nul in a key":                             {http.StatusOK, `{"outcome":"done","result":{"a\u0000":[1e2]}}`, deliveries.StatusDone, `{"a\ufffd":[100]}`},
		"done with a huge number":                              {http.StatusOK, `{"outcome":"done","result":{"n":1e999999}}`, deliveries.StatusDone, ``},
		"done with a tiny number":                              {http.StatusOK, `{"outcome":"done","result":{"n":[1e-999999]}}`, deliveries.StatusDone, ``},
		"refused with a nul":                                   {http.StatusUnprocessableEntity, `{"outcome":"refused","refusal":{"reason":"no\u0000"}}`, deliveries.StatusRefused, `{"reason":"no\ufffd"}`},
		"refused with a huge number":                           {http.StatusUnprocessableEntity, `{"outcome":"refused","refusal":{"n":1e999999}}`, deliveries.StatusRefused, ``},
		"unknown with a nul":                                   {http.StatusOK, `{"outcome":"unknown","reason":"\u0000"}`, deliveries.StatusUnknown, `{"outcome":"unknown","reason":"\ufffd"}`},
		"unknown with a huge number":                           {http.StatusOK, `{"outcome":"unknown","n":1e999999}`, deliveries.StatusUnknown, `{}`},
		"unknown with a tiny number":                           {http.StatusOK, `{"outcome":"unknown","n":1e-999999}`, deliveries.StatusUnknown, `{}`},
		"done with a long number":                              {http.StatusOK, `{"outcome":"done","result":{"n":1e1000}}`, deliveries.StatusDone, ``},
		"done with too many numbers":                           {http.StatusOK, `{"outcome":"done","result":{"n":[` + strings.Repeat(`1e900,`, 80) + `1]}}`, deliveries.StatusDone, ``},
		"done too long for the column":                         {http.StatusOK, `{"outcome":"done","result":{"n":[` + strings.Repeat(`1e900,`, 20) + `1]}}`, deliveries.StatusDone, ``},
		"done with numbers and a note too long for the column": {http.StatusOK, `{"outcome":"done","result":{"note":"` + strings.Repeat(`a`, 200) + `","n":[` + strings.Repeat(`1e900,`, 18) + `1]}}`, deliveries.StatusDone, ``},
		"done with text jsonb writes shorter":                  {http.StatusOK, `{"outcome":"done","result":{"html":"` + strings.Repeat(`<&>`, 1000) + `"}}`, deliveries.StatusDone, `{"html":"` + strings.Repeat(`<&>`, 1000) + `"}`},
		"done that just fits the column":                       {http.StatusOK, `{"outcome":"done","result":{"n":[` + strings.Repeat(`1e900,`, 18) + `1]}}`, deliveries.StatusDone, `{"n":[` + strings.Repeat(`1`+strings.Repeat(`0`, 900)+`,`, 18) + `1]}`},
		"done with a number that fits":                         {http.StatusOK, `{"outcome":"done","result":{"n":[1e2,0.5]}}`, deliveries.StatusDone, `{"n":[100,0.5]}`},
	} {
		t := s.W.NewTenant(s.T(), "Acme "+name)
		answer := c
		recorded := &recorder{answer: func() (int, string) { return answer.status, answer.answer }}
		server := httptest.NewServer(recorded)
		defer server.Close()
		s.defineAt(t, server.URL)
		d := s.approved(t, "Printer jams")
		s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false), name)
		after := s.delivery(t, d.ProposalId)
		s.Require().Equal(c.want, after.Status, name)
		s.Require().Equal(1, after.AttemptsStarted, name+": final on the first attempt")
		s.Require().Nil(after.AttemptOpenAt, name)
		bodies, _ := recorded.all()
		s.Require().Len(bodies, 1, name)
		outcome := s.eventPayload(t, d.ProposalId, "delivery."+c.want)
		// The chain digests what the receiver answered, stored or not.
		var parts struct{ Result, Refusal json.RawMessage }
		s.Require().NoError(json.Unmarshal([]byte(c.answer), &parts), name)
		answered := map[string]json.RawMessage{deliveries.StatusDone: parts.Result, deliveries.StatusRefused: parts.Refusal, deliveries.StatusUnknown: json.RawMessage(c.answer)}[c.want]
		answeredDigest, err := events.Digest(answered)
		s.Require().NoError(err, name)
		s.Require().Equal(answeredDigest, outcome["resultSha256"], name+": the chain names what the receiver answered")
		if c.result == "" || c.result == `{}` {
			if c.result == "" {
				s.Require().Nil(after.Result, name)
			}
			s.Require().Equal(false, outcome["resultKept"], name+": the chain says the answer is not stored")
			continue
		}
		s.Require().NotContains(outcome, "resultKept", name)
		// Compared canonically: the numbers may be longer than float64 holds.
		want, err := events.Canonical(json.RawMessage(c.result))
		s.Require().NoError(err, name)
		stored, err := events.Canonical(after.Result)
		s.Require().NoError(err, name)
		s.Require().Equal(string(want), string(stored), name)
		digest, err := events.Digest(after.Result)
		s.Require().NoError(err)
		s.Require().Equal(digest, outcome["resultSha256"], name+": what is stored has the same digest")
	}
}

// A delivery never sent may be sent again by a person, the same delivery,
// before its deadline; after the deadline, not.
func (s *DeliveriesSuite) Test_ANeverSentDeliveryIsSentAgainBeforeItsDeadline() {
	t := s.W.NewTenant(s.T(), "Acme")
	url := closedUrl()
	s.defineAt(t, url)
	d := s.approved(t, "Printer jams")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, true))
	s.Require().Equal(deliveries.StatusNotSent, s.delivery(t, d.ProposalId).Status)

	_, err := s.W.Kernel.Deliveries.SendAgain(t.Agent, d.ProposalId)
	s.code(err, fault.PermissionDenied)
	h := s.defineWebhook(t, false)
	again, err := s.W.Kernel.Deliveries.SendAgain(t.Reviewer, d.ProposalId)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusPending, again.Status)
	s.Require().Equal(d.Id, again.Id, "the same delivery")
	s.Require().Equal(h.audience, again.Audience, "to the action's address and audience as they are now")
	retried := s.eventPayload(t, d.ProposalId, "delivery.retried")
	movedSum := sha256.Sum256([]byte(h.server.URL + "/hooks"))
	s.Require().Equal(hex.EncodeToString(movedSum[:]), retried["connectorUrlSha256"], "the log names the new address, which the approver never saw")
	s.Require().Equal(h.audience, retried["audience"])
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false))
	s.Require().Equal(deliveries.StatusDone, s.delivery(t, d.ProposalId).Status)

	// Past the last moment an attempt may open, it cannot be sent again.
	s.defineAt(t, url)
	late := s.approved(t, "Toner")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, late.Id, true))
	s.backdate(late.Id, `not_after = now() + interval '10 seconds'`)
	_, err = s.W.Kernel.Deliveries.SendAgain(t.Reviewer, late.ProposalId)
	s.code(err, fault.Conflict)
	s.refused(t.Owner, `UPDATE mandate.deliveries SET status = 'pending' WHERE id = $1`, late.Id)
}

// An unknown delivery is settled by a person, and only once the work can
// no longer happen.
func (s *DeliveriesSuite) Test_AnUnknownDeliveryIsSettledOnlyWhenTheWorkCannotHappen() {
	t := s.W.NewTenant(s.T(), "Acme")
	recorded := &recorder{answer: func() (int, string) { return http.StatusOK, `{"outcome":"unknown","reason":"the bank did not answer"}` }}
	server := httptest.NewServer(recorded)
	defer server.Close()
	defined := s.defineAt(t, server.URL)
	d := s.approved(t, "Refund")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false))
	unknown := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusUnknown, unknown.Status)
	s.Require().Contains(string(unknown.Result), "the bank did not answer")

	_, err := s.W.Kernel.Deliveries.Settle(t.Reviewer, d.ProposalId, false)
	s.code(err, fault.Conflict)
	s.Require().Contains(err.Error(), "not yet")
	s.refused(t.Owner, `UPDATE mandate.deliveries SET status = 'settled_not_done' WHERE id = $1`, d.Id)

	// The work can no longer happen once notAfter, the request timeout and
	// the allowance for clocks have passed.
	s.backdate(d.Id, `not_after = now() - interval '5 minutes 20 seconds'`)
	_, err = s.W.Kernel.Deliveries.Settle(t.Agent, d.ProposalId, false)
	s.code(err, fault.PermissionDenied)
	s.refused(t.System, `UPDATE mandate.deliveries SET status = 'settled_not_done' WHERE id = $1`, d.Id)
	settled, err := s.W.Kernel.Deliveries.Settle(t.Reviewer, d.ProposalId, false)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusSettledNotDone, settled.Status)
	s.Require().Equal(&t.ReviewerId, settled.SettledBy)
	types := s.eventTypes(t.Owner, d.ProposalId)
	s.Require().Equal("delivery.settled", types[len(types)-1])
	settledEvent := s.eventPayload(t, d.ProposalId, "delivery.settled")
	sum := sha256.Sum256([]byte(server.URL))
	s.Require().Equal(hex.EncodeToString(sum[:]), settledEvent["connectorUrlSha256"], "the log names where the settled delivery went")
	s.Require().Equal(defined.ConnectorAudience, settledEvent["audience"])
	s.Require().Equal(deliveries.StatusSettledNotDone, settledEvent["outcome"])
	_, err = s.W.Kernel.Deliveries.Settle(t.Owner, d.ProposalId, true)
	s.code(err, fault.Conflict)
	s.refused(t.Owner, `UPDATE mandate.deliveries SET status = 'settled_done' WHERE id = $1`, d.Id)
}

// No attempt opens too late: a delivery whose deadline is near ends before
// anything is sent.
func (s *DeliveriesSuite) Test_NoAttemptOpensTooLate() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	d := s.approved(t, "Printer jams")
	s.backdate(d.Id, `not_after = now() + interval '10 seconds'`)
	s.refused(t.System, `UPDATE mandate.deliveries SET attempts_started = attempts_started + 1 WHERE id = $1`, d.Id)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false))
	s.Require().Empty(h.receiver.Received())
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusNotSent, after.Status)
	s.Require().Zero(after.AttemptsStarted)
}

// An attempt left open by a crash counts as possibly sent: a retry that
// then fails to connect still ends unknown, never not sent.
func (s *DeliveriesSuite) Test_ACrashLeavesAnAttemptPossiblySent() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.defineAt(t, closedUrl())
	d := s.approved(t, "Printer jams")
	s.openAttempt(t, d.Id)
	s.Require().ErrorContains(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, true), "still open", "a young open attempt is waited for")
	s.backdate(d.Id, `attempt_open_at = now() - interval '1 minute'`)
	finished, err := s.W.Kernel.Deliveries.FinishDue(t.System)
	s.Require().NoError(err)
	s.Require().Zero(finished, "the sweep closes the attempt, but the deadline is not near")
	s.Require().Nil(s.delivery(t, d.ProposalId).AttemptOpenAt)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, true))
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusUnknown, after.Status, "the crashed attempt may have reached the receiver")
	s.Require().Equal(2, after.AttemptsStarted)
	s.Require().Equal(1, after.AttemptsUnsent)
}

// A job that finds an attempt left open by a crash closes it itself, still
// counted as possibly sent, and goes on: past the deadline it finishes the
// delivery at once rather than failing until its attempts run out.
func (s *DeliveriesSuite) Test_AJobClosesAStaleAttemptItself() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.defineAt(t, closedUrl())

	// Before the deadline: closed, and a new attempt is made.
	d := s.approved(t, "Printer jams")
	s.openAttempt(t, d.Id)
	s.backdate(d.Id, `attempt_open_at = now() - interval '1 minute'`)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, true))
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusUnknown, after.Status, "the crashed attempt may have reached the receiver")
	s.Require().Equal(2, after.AttemptsStarted)
	s.Require().Equal(1, after.AttemptsUnsent)

	// Past the last moment an attempt may open: finished at once, with no
	// new attempt and no error for the queue to retry.
	late := s.approved(t, "Keyboard")
	s.openAttempt(t, late.Id)
	s.backdate(late.Id, `attempt_open_at = now() - interval '1 minute', not_after = now() + interval '5 seconds'`)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, late.Id, false))
	after = s.delivery(t, late.ProposalId)
	s.Require().Equal(deliveries.StatusUnknown, after.Status)
	s.Require().Equal(1, after.AttemptsStarted, "no attempt opens too late")
	s.Require().Nil(after.AttemptOpenAt)

	// Its action removed: finished at once too.
	gone := s.approved(t, "Monitor")
	s.openAttempt(t, gone.Id)
	s.backdate(gone.Id, `attempt_open_at = now() - interval '1 minute'`)
	s.Require().NoError(s.W.Kernel.Actions.Remove(t.Owner, "tickets.create"))
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, gone.Id, false))
	after = s.delivery(t, gone.ProposalId)
	s.Require().Equal(deliveries.StatusUnknown, after.Status)
	s.Require().Equal(1, after.AttemptsStarted)
}

// The kernel's sweep visits a tenant whose only due work is an attempt left
// open by a crash, closes it, and, past the last moment an attempt may
// open, finishes the delivery unknown, since that attempt was possibly sent.
// Each case is a tenant of its own, so nothing else lists it for the sweep.
func (s *DeliveriesSuite) Test_TheKernelSweepClosesAnAttemptLeftOpenByACrash() {
	// Near the deadline: closed and finished unknown.
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	d := s.approved(t, "Printer jams")
	s.openAttempt(t, d.Id)
	s.backdate(d.Id, `attempt_open_at = now() - interval '1 minute', not_after = now() + interval '5 seconds'`)
	_, err := s.W.Kernel.FinishDue(context.Background())
	s.Require().NoError(err)
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusUnknown, after.Status, "the crashed attempt may have reached the receiver")
	s.Require().Nil(after.AttemptOpenAt)
	s.Require().Equal(1, after.AttemptsStarted, "no attempt opens too late")
	s.Require().Zero(after.AttemptsUnsent)
	s.Require().Empty(h.receiver.Received(), "the sweep sends nothing")
	unknown := s.eventPayload(t, d.ProposalId, "delivery.unknown")
	sum := sha256.Sum256([]byte(h.server.URL + "/hooks"))
	s.Require().Equal(hex.EncodeToString(sum[:]), unknown["connectorUrlSha256"], "the log names where the possibly sent attempt went")
	s.Require().Equal(h.audience, unknown["audience"])

	// Far from the deadline: closed, and the delivery waits for its job.
	far := s.W.NewTenant(s.T(), "Bolt")
	s.defineWebhook(far, false)
	waiting := s.approved(far, "Toner")
	s.openAttempt(far, waiting.Id)
	s.backdate(waiting.Id, `attempt_open_at = now() - interval '1 minute'`)
	_, err = s.W.Kernel.FinishDue(context.Background())
	s.Require().NoError(err)
	after = s.delivery(far, waiting.ProposalId)
	s.Require().Equal(deliveries.StatusPending, after.Status, "the deadline is not near")
	s.Require().Nil(after.AttemptOpenAt, "the sweep closed the attempt")
	s.Require().Equal(1, after.AttemptsStarted)
	s.Require().Zero(after.AttemptsUnsent, "still counted as possibly sent")
}

// A receiver that answers after its attempt was closed does not change the
// delivery: the answer is recorded as a conflict for a person.
func (s *DeliveriesSuite) Test_ALateAnswerIsAConflict() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	started, release := make(chan struct{}), make(chan struct{})
	var body []byte
	h.receiver.Handle = func(r webhook.Received) (any, error) {
		body = r.Body
		close(started)
		<-release
		return map[string]any{"done": true}, nil
	}
	d := s.approved(t, "Printer jams")
	result := make(chan error, 1)
	go func() { result <- s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, false) }()
	select {
	case <-started:
	case err := <-result:
		close(release)
		s.Require().FailNow("the delivery ended before the receiver was called", "%v", err)
	case <-time.After(10 * time.Second):
		close(release)
		s.Require().FailNow("the receiver was never called")
	}
	s.backdate(d.Id, `attempt_open_at = now() - interval '1 minute'`)
	_, err := s.W.Kernel.Deliveries.FinishDue(t.System)
	s.Require().NoError(err)
	close(release)
	s.Require().ErrorContains(<-result, "answered done after its attempt was closed", "the job says what happened, not that no outcome came")
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusPending, after.Status, "the late answer changes nothing")
	s.Require().Contains(s.eventTypes(t.Owner, d.ProposalId), "delivery.conflict")
	conflict := s.eventPayload(t, d.ProposalId, "delivery.conflict")
	s.Require().Equal(deliveries.StatusDone, conflict["answered"])
	urlSum := sha256.Sum256([]byte(h.server.URL + "/hooks"))
	s.Require().Equal(hex.EncodeToString(urlSum[:]), conflict["connectorUrlSha256"], "the log names where the answered attempt went")
	s.Require().Equal(h.audience, conflict["audience"])
	s.Require().NotEmpty(body)
	bodySum := sha256.Sum256(body)
	s.Require().Equal(hex.EncodeToString(bodySum[:]), conflict["bodySha256"], "the log names the exact body the receiver answered")
	s.Require().NotEmpty(conflict["resultSha256"])
}

// An answer that came is recorded even when the job's context ends right
// after it, as River's per-job timeout or a shutdown would end it: the
// receiver said done, so nothing is sent again and no person settles it.
func (s *DeliveriesSuite) Test_AnAnswerIsRecordedAfterTheJobsContextEnds() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, false)
	d := s.approved(t, "Printer jams")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := deliveries.New(s.W.Pg.App, nil, s.W.Kernel.Proposals, deliveries.Options{AllowPrivate: true, Signer: testfix.Signer})
	deliveries.WrapTransport(service, func(inner http.RoundTripper) http.RoundTripper {
		return roundTrip(func(req *http.Request) (*http.Response, error) {
			resp, err := inner.RoundTrip(req)
			if err != nil {
				return nil, err
			}
			// The whole answer is in hand before the context ends.
			answer, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return nil, err
			}
			resp.Body = io.NopCloser(bytes.NewReader(answer))
			cancel()
			return resp, nil
		})
	})

	s.Require().NoError(service.Deliver(ctx, t.Id, d.Id, false))
	s.Require().ErrorIs(ctx.Err(), context.Canceled, "the job's context ended before the answer was recorded")
	s.Require().Len(h.receiver.Received(), 1)
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusDone, after.Status)
	s.Require().Nil(after.AttemptOpenAt)
	s.Require().Equal(1, after.AttemptsStarted)
	s.Require().Contains(s.eventTypes(t.Owner, d.ProposalId), "delivery.done")
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// The sweep finishes deliveries past the last moment an attempt may open,
// and never one with an attempt open.
func (s *DeliveriesSuite) Test_TheSweepFinishesWhatIsDue() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.defineWebhook(t, false)
	due := s.approved(t, "Due")
	open := s.approved(t, "Open")
	s.openAttempt(t, open.Id)
	waiting := s.approved(t, "Waiting")
	s.backdate(due.Id, `not_after = now() + interval '5 seconds'`)
	s.backdate(open.Id, `not_after = now() + interval '5 seconds'`)

	_, err := s.W.Kernel.Deliveries.FinishDue(t.Owner)
	s.code(err, fault.PermissionDenied)
	finished, err := s.W.Kernel.Deliveries.FinishDue(t.System)
	s.Require().NoError(err)
	s.Require().Equal(1, finished)
	s.Require().Equal(deliveries.StatusNotSent, s.delivery(t, due.ProposalId).Status)
	s.Require().Equal(deliveries.StatusPending, s.delivery(t, open.ProposalId).Status, "an attempt is open")
	s.Require().Equal(deliveries.StatusPending, s.delivery(t, waiting.ProposalId).Status)

	// Across tenants, through the kernel.
	other := s.W.NewTenant(s.T(), "Bolt")
	s.defineWebhook(other, false)
	theirs := s.approved(other, "Theirs")
	s.backdate(theirs.Id, `not_after = now() + interval '5 seconds'`)
	total, err := s.W.Kernel.FinishDue(context.Background())
	s.Require().NoError(err)
	s.Require().Equal(1, total)
	s.Require().Equal(deliveries.StatusNotSent, s.delivery(other, theirs.ProposalId).Status)
}

// The sweep seeks what is due in the deadline index rather than reading
// every waiting delivery of every tenant: the deadline is compared with the
// indexed column as it is, so it is an index condition and not a filter.
func (s *DeliveriesSuite) Test_TheSweepSeeksWhatIsDueInTheIndex() {
	source, err := os.ReadFile("internal/repo/deliveries.sql.go")
	s.Require().NoError(err)
	query := regexp.MustCompile("(?s)const listDueToFinish = `(.*?)`").FindSubmatch(source)
	s.Require().NotNil(query, "the generated ListDueToFinish")
	t := s.W.NewTenant(s.T(), "Acme")
	var plan []string
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		// A test's table is so small that reading it all would be cheapest;
		// the plan in question is the one for a large table.
		if _, err := tx.Exec(t.System, `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		rows, err := tx.Query(t.System, "EXPLAIN "+string(query[1]), int32(1))
		if err != nil {
			return err
		}
		plan, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}))
	text := strings.Join(plan, "\n")
	s.Require().Contains(text, "deliveries_due_idx", text)
	s.Require().Regexp(`Index Cond: \(not_after <= `, text)
}

// A waiting delivery moves with its action, to the new address and
// audience, only while nothing was possibly sent.
func (s *DeliveriesSuite) Test_AWaitingDeliveryMovesOnlyBeforeAnythingWasSent() {
	t := s.W.NewTenant(s.T(), "Acme")
	old := s.defineWebhook(t, false)
	fresh := s.approved(t, "Fresh")
	sent := s.approved(t, "Sent")
	s.openAttempt(t, sent.Id)
	s.Require().NoError(s.exec(t.System, `UPDATE mandate.deliveries SET attempt_open_at = NULL WHERE id = $1`, sent.Id))

	moved := s.defineWebhook(t, false)
	s.Require().NotEqual(old.audience, moved.audience)
	got := s.delivery(t, fresh.ProposalId)
	s.Require().Equal(moved.server.URL+"/hooks", got.Url)
	s.Require().Equal(moved.audience, got.Audience)
	s.Require().Contains(s.eventTypes(t.Owner, fresh.ProposalId), "delivery.retargeted")
	stayed := s.delivery(t, sent.ProposalId)
	s.Require().Equal(old.server.URL+"/hooks", stayed.Url, "possibly sent: it stays where it went")
	s.Require().Equal(old.audience, stayed.Audience)

	// The database holds it too.
	s.refused(t.Owner, `UPDATE mandate.deliveries SET url = $2, audience = $3 WHERE id = $1`, sent.Id, moved.server.URL+"/hooks", moved.audience)
	s.refused(t.Reviewer, `UPDATE mandate.deliveries SET url = 'https://elsewhere.example' WHERE id = $1`, fresh.Id)
}

func (s *DeliveriesSuite) Test_TheDatabaseHoldsTheTransitions() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.defineWebhook(t, false)
	d := s.approved(t, "Printer jams")
	for name, c := range map[string]struct {
		ctx       context.Context
		statement string
	}{
		"a person marks it done":               {t.Owner, `UPDATE mandate.deliveries SET status = 'done' WHERE id = $1`},
		"done without an open attempt":         {t.System, `UPDATE mandate.deliveries SET status = 'done' WHERE id = $1`},
		"two attempts at once":                 {t.System, `UPDATE mandate.deliveries SET attempts_started = attempts_started + 2 WHERE id = $1`},
		"unsent without an open attempt":       {t.System, `UPDATE mandate.deliveries SET attempts_unsent = attempts_unsent + 1 WHERE id = $1`},
		"a result while it waits":              {t.System, `UPDATE mandate.deliveries SET result = '{"x":1}' WHERE id = $1`},
		"another deadline":                     {t.System, `UPDATE mandate.deliveries SET not_after = now() + interval '1 year' WHERE id = $1`},
		"unknown with nothing possibly sent":   {t.System, `UPDATE mandate.deliveries SET status = 'unknown' WHERE id = $1`},
		"settled while it waits":               {t.Owner, `UPDATE mandate.deliveries SET status = 'settled_done' WHERE id = $1`},
		"to an address that is not the action": {t.System, `UPDATE mandate.deliveries SET url = 'https://elsewhere.example' WHERE id = $1`},
	} {
		err := s.exec(c.ctx, c.statement, d.Id)
		s.Require().Error(err, name)
		s.Require().Equal(fault.Name(fault.PermissionDenied), fault.Name(fault.CodeOf(fault.FromDb(err))), name+": "+err.Error())
	}
	// Opened: no second attempt over it, no not_sent with it possibly sent,
	// and an answer closes it.
	s.openAttempt(t, d.Id)
	s.refused(t.System, `UPDATE mandate.deliveries SET attempts_started = attempts_started + 1 WHERE id = $1`, d.Id)
	s.refused(t.System, `UPDATE mandate.deliveries SET status = 'not_sent', attempt_open_at = NULL WHERE id = $1`, d.Id)
	s.Require().NoError(s.exec(t.System, `UPDATE mandate.deliveries SET status = 'done', attempt_open_at = NULL WHERE id = $1`, d.Id))
	s.refused(t.System, `UPDATE mandate.deliveries SET status = 'pending' WHERE id = $1`, d.Id)
	s.refused(t.Owner, `DELETE FROM mandate.deliveries WHERE id = $1`, d.Id)

	// A delivery is recorded only in the transaction that decides, to the
	// action's own address and audience.
	p := s.propose(t, "Undecided")
	s.refused(t.Owner, `INSERT INTO mandate.deliveries (id, tenant_id, proposal_id, action_key, url, audience, not_after)
		VALUES (gen_random_uuid(), $1, $2, 'tickets.create', 'https://x.example', 'pgm:x', now())`, t.Id, p.Id)
}

// An attempt opens only by counting it, which the guard refuses too late:
// the system setting attempt_open_at itself, past the last moment an
// attempt may open, is refused, so no answer can be recorded as final.
func (s *DeliveriesSuite) Test_AnAttemptOpensOnlyByCountingIt() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.defineWebhook(t, false)
	d := s.approved(t, "Printer jams")
	s.refused(t.System, `UPDATE mandate.deliveries SET attempt_open_at = now() WHERE id = $1`, d.Id)
	s.backdate(d.Id, `not_after = now() + interval '10 seconds'`)
	s.refused(t.System, `UPDATE mandate.deliveries SET attempt_open_at = now() WHERE id = $1`, d.Id)
	s.refused(t.System, `UPDATE mandate.deliveries SET attempt_open_at = now() + interval '1 year' WHERE id = $1`, d.Id)

	// RecordAnswer's own statement then finds no attempt to answer.
	var answered int64
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		tag, err := tx.Exec(t.System, `UPDATE mandate.deliveries
			SET status = 'done', result = '{"done":true}', attempt_open_at = NULL, last_status = 200, last_error = NULL
			WHERE id = $1 AND status = 'pending' AND attempt_open_at IS NOT NULL`, d.Id)
		answered = tag.RowsAffected()
		return err
	}))
	s.Require().Zero(answered)
	after := s.delivery(t, d.ProposalId)
	s.Require().Equal(deliveries.StatusPending, after.Status)
	s.Require().Nil(after.AttemptOpenAt)
	s.Require().Zero(after.AttemptsStarted)

	// An attempt opened by counting it keeps its time: it is not moved on
	// to escape the stale sweep.
	open := s.approved(t, "Keyboard")
	s.openAttempt(t, open.Id)
	s.refused(t.System, `UPDATE mandate.deliveries SET attempt_open_at = now() + interval '1 year' WHERE id = $1`, open.Id)
}

// Sending again is never dropped as a duplicate of the job that gave up.
func (s *DeliveriesSuite) Test_SendingAgainIsNeverDroppedAsADuplicate() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.defineAt(t, closedUrl())
	d := s.approved(t, "Printer jams")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, d.Id, true))
	queue := jobs.NewQueue(s.W.Pg.App)
	service := deliveries.New(s.W.Pg.App, queue, s.W.Kernel.Proposals, deliveries.Options{AllowPrivate: true, Signer: testfix.Signer})
	s.Require().NoError(queue.Init(1))
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		return queue.InsertTx(t.System, tx, deliveries.DeliverArgs{TenantId: t.Id, DeliveryId: d.Id, NotAfter: d.NotAfter},
			&river.InsertOpts{Queue: jobs.QueueDeliveries, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight}})
	}))
	_, err := service.SendAgain(t.Owner, d.ProposalId)
	s.Require().NoError(err)
	var count int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT count(*) FROM mandate_river.river_job WHERE args->>'deliveryId' = $1`, d.Id.String()).Scan(&count))
	s.Require().Equal(2, count, "the resend has its own job")
}

// A dry run a mandate would allow gets no delivery: the execution records
// nothing to send, and the table refuses one written by hand.
func (s *DeliveriesSuite) Test_ADryRunIsNeverDelivered() {
	t := s.W.NewTenant(s.T(), "Acme")
	h := s.defineWebhook(t, true)
	dry := s.W.DryRunAgent(s.T(), t)
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &dry.Id, Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 5, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	p, _, err := s.W.Kernel.Proposals.Propose(dry.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: "Printer jams", Payload: json.RawMessage(`{"subject":"Printer jams"}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, p.Id))
	after, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusWouldAllow, after.Status)
	_, found, err := s.W.Kernel.Deliveries.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().False(found, "nothing to deliver")
	s.Require().Empty(h.receiver.Received())

	var url, audience string
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Owner, `SELECT connector_url, connector_audience FROM mandate.actions WHERE key = 'tickets.create'`).Scan(&url, &audience)
	}))
	err = db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.System, `INSERT INTO mandate.deliveries (id, tenant_id, proposal_id, action_key, url, audience, not_after)
			VALUES (gen_random_uuid(), $1, $2, 'tickets.create', $3, $4, now())`, t.Id, p.Id, url, audience)
		return err
	})
	s.Require().Error(err)
	s.Require().Equal(fault.Name(fault.PermissionDenied), fault.Name(fault.CodeOf(fault.FromDb(err))), "the table refuses a delivery for a dry run")
}

// Another tenant sees none of a tenant's deliveries: not the receiver's
// address, audience or result, and it can neither change, settle, send
// again, deliver nor finish one.
func (s *DeliveriesSuite) Test_AnotherTenantSeesNoDeliveries() {
	a := s.W.NewTenant(s.T(), "Acme")
	b := s.W.NewTenant(s.T(), "Bolt")
	h := s.defineWebhook(a, false)
	s.defineWebhook(b, false)
	ours := s.approved(a, "Printer jams")

	_, found, err := s.W.Kernel.Deliveries.Get(b.Owner, ours.ProposalId)
	s.Require().NoError(err)
	s.Require().False(found)
	byProposal, err := s.W.Kernel.Deliveries.ByProposals(b.Owner, []uuid.UUID{ours.ProposalId})
	s.Require().NoError(err)
	s.Require().Empty(byProposal)
	var visible int
	s.Require().NoError(db.InTx(b.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(b.Owner, `SELECT count(*) FROM mandate.deliveries WHERE id = $1 OR proposal_id = $2`, ours.Id, ours.ProposalId).Scan(&visible)
	}))
	s.Require().Zero(visible)

	var affected int64
	s.Require().NoError(db.InTx(b.System, s.W.Pg.App, func(tx pgx.Tx) error {
		tag, err := tx.Exec(b.System, `UPDATE mandate.deliveries SET attempts_started = attempts_started + 1 WHERE id = $1`, ours.Id)
		affected = tag.RowsAffected()
		return err
	}))
	s.Require().Zero(affected)

	// A job naming another tenant sends nothing.
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), b.Id, ours.Id, false))
	s.Require().Empty(h.receiver.Received())
	s.Require().Zero(s.delivery(a, ours.ProposalId).AttemptsStarted)

	// Sent and left unknown, so that A could settle it, and another one
	// never sent, so that A could send it again.
	recorded := &recorder{answer: func() (int, string) { return http.StatusOK, `{"outcome":"unknown","reason":"no answer"}` }}
	server := httptest.NewServer(recorded)
	defer server.Close()
	s.defineAt(a, server.URL)
	unknown := s.approved(a, "Refund")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), a.Id, unknown.Id, false))
	s.Require().Equal(deliveries.StatusUnknown, s.delivery(a, unknown.ProposalId).Status)
	s.backdate(unknown.Id, `not_after = now() - interval '5 minutes 20 seconds'`)
	s.defineAt(a, closedUrl())
	notSent := s.approved(a, "Toner")
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), a.Id, notSent.Id, true))
	s.Require().Equal(deliveries.StatusNotSent, s.delivery(a, notSent.ProposalId).Status)

	_, err = s.W.Kernel.Deliveries.Settle(b.Reviewer, unknown.ProposalId, true)
	s.code(err, fault.NotFound)
	_, err = s.W.Kernel.Deliveries.SendAgain(b.Reviewer, notSent.ProposalId)
	s.code(err, fault.NotFound)

	// The other tenant's sweep neither finishes a due delivery nor closes
	// a stale attempt.
	s.openAttempt(a, ours.Id)
	s.backdate(ours.Id, `attempt_open_at = now() - interval '1 minute'`)
	due := s.approved(a, "Due")
	s.backdate(due.Id, `not_after = now() + interval '5 seconds'`)
	finished, err := s.W.Kernel.Deliveries.FinishDue(b.System)
	s.Require().NoError(err)
	s.Require().Zero(finished)

	after := s.delivery(a, ours.ProposalId)
	s.Require().Equal(deliveries.StatusPending, after.Status)
	s.Require().Equal(1, after.AttemptsStarted, "only A's own attempt counted")
	s.Require().NotNil(after.AttemptOpenAt, "B's sweep did not close it")
	s.Require().Equal(deliveries.StatusPending, s.delivery(a, due.ProposalId).Status)
	s.Require().Equal(deliveries.StatusUnknown, s.delivery(a, unknown.ProposalId).Status)
	s.Require().Equal(deliveries.StatusNotSent, s.delivery(a, notSent.ProposalId).Status)

	// A's own sweep does both, so they were due.
	finished, err = s.W.Kernel.Deliveries.FinishDue(a.System)
	s.Require().NoError(err)
	s.Require().Equal(1, finished)
	s.Require().Nil(s.delivery(a, ours.ProposalId).AttemptOpenAt)
	s.Require().Equal(deliveries.StatusNotSent, s.delivery(a, due.ProposalId).Status)
}
