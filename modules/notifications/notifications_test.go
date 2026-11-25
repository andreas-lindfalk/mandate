package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/notifications"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

const (
	slackUrl   = "https://hooks.slack.com/services/T000/B000/secret-part"
	discordUrl = "https://discord.com/api/webhooks/123456/secret_part-x"
)

type NotifiersSuite struct {
	IntegrationSuite
}

func TestNotifiersSuite(t *testing.T) {
	suite.Run(t, new(NotifiersSuite))
}

func (s *NotifiersSuite) propose(t testfix.Tenant, action string) proposals.Proposal {
	s.T().Helper()
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{
		IdempotencyKey: uuid.NewString(), Action: action, Title: "Call @everyone about <secret>", Payload: json.RawMessage(`{"note":"personal data"}`),
		Rationale: "The rationale stays out of the chat.",
	})
	s.Require().NoError(err)
	return p
}

func (s *NotifiersSuite) Test_AnOwnerSetsWhereNotificationsGo() {
	t := s.W.NewTenant(s.T(), "Acme")
	service := s.W.Kernel.Notifications
	none, err := service.Get(t.Reviewer)
	s.Require().NoError(err)
	s.Require().Nil(none)

	_, err = service.Set(t.Reviewer, notifications.KindSlack, slackUrl)
	s.code(err, fault.PermissionDenied)
	for kind, url := range map[string]string{
		"teams":                       slackUrl,
		notifications.KindSlack + "1": slackUrl,
	} {
		_, err := service.Set(t.Owner, kind, url)
		s.code(err, fault.InvalidArgument)
	}
	for _, url := range []string{
		"http://hooks.slack.com/services/T000/B000/x",
		"https://hooks.slack.com.evil.example/services/x",
		"https://hooks.slack.com/services/x?y=1",
		"https://127.0.0.1/services/x",
		discordUrl,
	} {
		_, err := service.Set(t.Owner, notifications.KindSlack, url)
		s.code(err, fault.InvalidArgument)
	}
	_, err = service.Set(t.Owner, notifications.KindDiscord, "https://discord.com/api/webhooks/abc/x")
	s.code(err, fault.InvalidArgument)

	first, err := service.Set(t.Owner, notifications.KindSlack, slackUrl)
	s.Require().NoError(err)
	s.Require().Equal(notifications.KindSlack, first.Kind)
	s.Require().Equal(&t.OwnerId, first.CreatedBy)
	second, err := service.Set(t.Owner, notifications.KindDiscord, discordUrl)
	s.Require().NoError(err)
	read, err := service.Get(t.Reviewer)
	s.Require().NoError(err)
	s.Require().Equal(second.Id, read.Id, "the second replaces the first")
	s.Require().Equal([]string{"notifier.set", "notifier.removed"}, s.eventTypes(t.Owner, first.Id))
	s.Require().NoError(service.Remove(t.Owner))
	s.code(service.Remove(t.Owner), fault.NotFound)
	gone, err := service.Get(t.Owner)
	s.Require().NoError(err)
	s.Require().Nil(gone)
}

// The address is a secret: no one reads it back but the system, which
// sends to it.
func (s *NotifiersSuite) Test_TheAddressIsReadOnlyByTheSystem() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Notifications.Set(t.Owner, notifications.KindSlack, slackUrl)
	s.Require().NoError(err)
	s.refused(t.Owner, `SELECT url FROM mandate.notifiers`)
	s.refused(t.Owner, `SELECT * FROM mandate.notifiers`)
	s.refused(t.Owner, `SELECT * FROM mandate.notifier_of()`)
	s.refused(t.Agent, `SELECT * FROM mandate.notifier_of()`)
	var url string
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(t.System, `SELECT url FROM mandate.notifier_of()`).Scan(&url)
	}))
	s.Require().Equal(slackUrl, url)
}

// The table holds the rules whatever the service does.
func (s *NotifiersSuite) Test_TheDatabaseGuardsNotifiers() {
	t := s.W.NewTenant(s.T(), "Acme")
	insert := `INSERT INTO mandate.notifiers (id, tenant_id, kind, url) VALUES (gen_random_uuid(), $1, $2, $3)`
	s.refused(t.Reviewer, insert, t.Id, "slack", slackUrl)
	s.refused(t.Agent, insert, t.Id, "slack", slackUrl)
	s.invalid(t.Owner, insert, t.Id, "slack", "https://example.com/hook")
	s.invalid(t.Owner, insert, t.Id, "discord", slackUrl)
	s.invalid(t.Owner, insert, t.Id, "email", slackUrl)
	s.Require().NoError(s.exec(t.Owner, insert, t.Id, "slack", slackUrl))
	err := s.exec(t.Owner, insert, t.Id, "discord", discordUrl)
	s.code(fault.FromDb(err), fault.Conflict)
	s.refused(t.Owner, `UPDATE mandate.notifiers SET kind = 'discord'`)
	s.refused(t.Owner, `UPDATE mandate.notifiers SET url = 'https://hooks.slack.com/services/other'`)
	s.refused(t.Owner, `DELETE FROM mandate.notifiers`)
	s.refused(t.Reviewer, `UPDATE mandate.notifiers SET removed_at = now()`)

	// Another tenant sees none of it.
	other := s.W.NewTenant(s.T(), "Other")
	seen, err := s.W.Kernel.Notifications.Get(other.Owner)
	s.Require().NoError(err)
	s.Require().Nil(seen)
	s.refused(other.Owner, `INSERT INTO mandate.notifiers (id, tenant_id, kind, url) VALUES (gen_random_uuid(), $1, 'slack', $2)`, t.Id, slackUrl)
	var urls int
	s.Require().NoError(db.InTx(other.System, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(other.System, `SELECT count(*) FROM mandate.notifier_of()`).Scan(&urls)
	}))
	s.Require().Zero(urls)
}

// A notification names the action, the risk, the agent and why it waits,
// and links to it. Nothing the agent wrote reaches the chat.
func (s *NotifiersSuite) Test_TheMessageCarriesNothingTheAgentWrote() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Notifications.Set(t.Owner, notifications.KindDiscord, discordUrl)
	s.Require().NoError(err)
	p := s.propose(t, "test.manual")
	before := len(s.W.Chat.Posts())
	s.Require().NoError(s.W.Kernel.Notifications.Notify(context.Background(), t.Id, p.Id))
	posts := s.W.Chat.Posts()[before:]
	s.Require().Len(posts, 1)
	s.Require().Equal(discordUrl, posts[0].Url)
	content := posts[0].Body["content"].(string)
	s.Require().Contains(content, "`test.manual`")
	s.Require().Contains(content, "low risk")
	s.Require().Contains(content, "a person always decides this action")
	s.Require().Contains(content, testfix.PublicUrl+"/inbox/"+p.Id.String())
	for _, agentText := range []string{"everyone", "secret", "personal", "rationale"} {
		s.Require().NotContains(content, agentText)
	}
	s.Require().Equal(map[string]any{"parse": []any{}}, posts[0].Body["allowed_mentions"])
	s.Require().Equal([]string{"proposal.created", "proposal.notified"}, s.eventTypes(t.Owner, p.Id))

	// Decided meanwhile: nothing more is sent.
	_, err = s.W.Kernel.Proposals.Reject(t.Reviewer, p.Id, "no")
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.Notifications.Notify(context.Background(), t.Id, p.Id))
	s.Require().Len(s.W.Chat.Posts(), before+1)

	// Slack gets its own form.
	_, err = s.W.Kernel.Notifications.Set(t.Owner, notifications.KindSlack, slackUrl)
	s.Require().NoError(err)
	q := s.propose(t, "test.manual")
	s.Require().NoError(s.W.Kernel.Notifications.Notify(context.Background(), t.Id, q.Id))
	last := s.W.Chat.Posts()[len(s.W.Chat.Posts())-1]
	s.Require().Equal(slackUrl, last.Url)
	s.Require().Contains(last.Body["text"], "<"+testfix.PublicUrl+"/inbox/"+q.Id.String()+"|Open it in pgmandate>")

	// Without a notifier, nothing is sent.
	s.Require().NoError(s.W.Kernel.Notifications.Remove(t.Owner))
	r := s.propose(t, "test.manual")
	count := len(s.W.Chat.Posts())
	s.Require().NoError(s.W.Kernel.Notifications.Notify(context.Background(), t.Id, r.Id))
	s.Require().Len(s.W.Chat.Posts(), count)
}

func (s *NotifiersSuite) Test_ARefusalIsFinalAndAFailureIsRetried() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Notifications.Set(t.Owner, notifications.KindSlack, slackUrl)
	s.Require().NoError(err)
	defer s.W.Chat.Answer(http.StatusOK)
	p := s.propose(t, "test.manual")
	s.W.Chat.Answer(http.StatusNotFound)
	err = s.W.Kernel.Notifications.Notify(context.Background(), t.Id, p.Id)
	s.Require().ErrorIs(err, notifications.ErrRefused)
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		s.W.Chat.Answer(status)
		err = s.W.Kernel.Notifications.Notify(context.Background(), t.Id, p.Id)
		s.Require().Error(err)
		s.Require().NotErrorIs(err, notifications.ErrRefused)
	}
	s.Require().Equal([]string{"proposal.created"}, s.eventTypes(t.Owner, p.Id), "only a notification that arrived is recorded")
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: i/o timeout")
}

// The address is a secret: an error from sending never names it, so it
// reaches neither the job's record nor the log.
func (s *NotifiersSuite) Test_AFailureNeverNamesTheAddress() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Notifications.Set(t.Owner, notifications.KindSlack, slackUrl)
	s.Require().NoError(err)
	service := notifications.New(s.W.Pg.App, nil, notifications.Options{Client: &http.Client{Transport: failingTransport{}}})
	service.SetDescribe(func(_ context.Context, id uuid.UUID) (notifications.Message, bool, error) {
		return notifications.Message{ProposalId: id, Action: "a.b", Risk: "low", Agent: "x"}, true, nil
	})
	err = service.Notify(context.Background(), t.Id, uuid.New())
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "i/o timeout")
	s.Require().NotContains(err.Error(), "hooks.slack.com")
	s.Require().NotContains(err.Error(), "secret-part")
}

// An owner can send a test message to see that the channel gets them.
func (s *NotifiersSuite) Test_AnOwnerSendsATestMessage() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.code(s.W.Kernel.Notifications.SendTest(t.Owner), fault.NotFound)
	_, err := s.W.Kernel.Notifications.Set(t.Owner, notifications.KindDiscord, discordUrl)
	s.Require().NoError(err)
	s.code(s.W.Kernel.Notifications.SendTest(t.Reviewer), fault.PermissionDenied)
	before := len(s.W.Chat.Posts())
	s.Require().NoError(s.W.Kernel.Notifications.SendTest(t.Owner))
	posts := s.W.Chat.Posts()[before:]
	s.Require().Len(posts, 1)
	s.Require().Equal(discordUrl, posts[0].Url)
	s.Require().Contains(posts[0].Body["content"], "a test message")
	s.Require().Equal(map[string]any{"parse": []any{}}, posts[0].Body["allowed_mentions"])

	defer s.W.Chat.Answer(http.StatusOK)
	s.W.Chat.Answer(http.StatusNotFound)
	err = s.W.Kernel.Notifications.SendTest(t.Owner)
	s.code(err, fault.InvalidArgument)
	s.Require().Contains(err.Error(), "check the address")
	s.Require().NotContains(err.Error(), "secret_part")
}
