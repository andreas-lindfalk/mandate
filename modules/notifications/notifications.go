// Package notifications tells a tenant's people that a proposal waits for
// them, in the chat tool they use: a Slack or Discord incoming webhook, one
// per tenant, set by an owner. The message names the action, its risk, the
// agent and why it waits, and links to the inbox. It carries nothing the
// agent wrote (no title, payload or rationale), so a proposal cannot put
// text into the channel or personal data into the chat tool's records.
//
// The address is a secret and is kept like a connector secret: written by
// an owner, read only by the system, never returned.
package notifications

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
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/andreas-lindfalk/pgmandate/modules/notifications/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
)

const (
	KindSlack   = "slack"
	KindDiscord = "discord"
)

// The chat tools' own webhook addresses, as the database holds them to.
var addresses = map[string]*regexp.Regexp{
	KindSlack:   regexp.MustCompile(`^https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+$`),
	KindDiscord: regexp.MustCompile(`^https://(discord|discordapp)\.com/api/webhooks/[0-9]+/[A-Za-z0-9_-]+$`),
}

// Notifier is a tenant's notifier, without its address.
type Notifier struct {
	Id        uuid.UUID
	Kind      string
	CreatedBy *uuid.UUID
	CreatedAt time.Time
}

// Message is what a notification says. The kernel builds it from the
// proposal and the definitions around it, never from the agent's text.
type Message struct {
	ProposalId uuid.UUID
	Action     string
	Risk       string
	// Agent is the agent's name as an owner registered it.
	Agent string
	// Reason is why it waits, as the skip reasons name it, or empty.
	Reason string
}

// Describe says what a notification about a proposal would say, and
// whether the proposal still waits for a person.
type Describe func(ctx context.Context, proposalId uuid.UUID) (Message, bool, error)

type Options struct {
	// PublicUrl is where people open the web app; the message links to the
	// proposal there. Empty leaves the link out.
	PublicUrl string
	// Client replaces the HTTP client, for tests.
	Client *http.Client
}

type Service struct {
	pool      *pgxpool.Pool
	queue     *jobs.Queue
	describe  Describe
	client    *http.Client
	publicUrl string
}

// New creates the service. With a queue, notifications are sent by jobs;
// without one, nothing is sent.
func New(pool *pgxpool.Pool, queue *jobs.Queue, opts Options) *Service {
	s := &Service{pool: pool, queue: queue, client: opts.Client, publicUrl: strings.TrimSuffix(opts.PublicUrl, "/")}
	if s.client == nil {
		s.client = &http.Client{
			Timeout: 10 * time.Second,
			// The address is the chat tool's; a redirect elsewhere is not followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if queue != nil {
		river.AddWorker(queue.Workers(), &worker{service: s})
	}
	return s
}

// SetDescribe sets how a message is built. The kernel wires it.
func (s *Service) SetDescribe(describe Describe) {
	s.describe = describe
}

// Set makes kind and url the tenant's notifier, replacing the one before.
// Only an owner may, which the database enforces.
func (s *Service) Set(ctx context.Context, kind, url string) (Notifier, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Notifier{}, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	pattern, known := addresses[kind]
	switch {
	case !known:
		return Notifier{}, fault.New(fault.InvalidArgument, "the kind is slack or discord")
	case len(url) > 500 || !pattern.MatchString(url):
		return Notifier{}, fault.New(fault.InvalidArgument, "the address must be the chat tool's own incoming webhook: https://hooks.slack.com/services/... for Slack, https://discord.com/api/webhooks/... for Discord")
	}
	var result Notifier
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		if err := s.removeLive(ctx, tx, q); err != nil {
			return err
		}
		id := uuid.New()
		if err := q.InsertNotifier(ctx, repo.InsertNotifierParams{Id: id, TenantId: tenantId, Kind: kind, Url: url}); err != nil {
			return err
		}
		row, err := q.LiveNotifier(ctx)
		if err != nil {
			return err
		}
		result = Notifier{Id: row.Id, Kind: row.Kind, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt}
		_, err = events.Append(ctx, tx, events.Event{
			Type: "notifier.set", SubjectId: id, Payload: map[string]any{"kind": kind, "urlSha256": urlDigest(url)},
		})
		return err
	})
	return result, fault.FromDb(err)
}

// urlDigest names an address in the log without writing it there: the
// SHA-256 of the address as written, as for connectors.
func urlDigest(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// Remove stops notifications for the tenant.
func (s *Service) Remove(ctx context.Context) error {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		if _, err := q.LiveNotifier(ctx); errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "no notifier is set")
		} else if err != nil {
			return err
		}
		return s.removeLive(ctx, tx, q)
	})
	return fault.FromDb(err)
}

func (s *Service) removeLive(ctx context.Context, tx pgx.Tx, q *repo.Queries) error {
	live, err := q.LiveNotifier(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := q.RemoveLiveNotifier(ctx); err != nil {
		return err
	}
	_, err = events.Append(ctx, tx, events.Event{Type: "notifier.removed", SubjectId: live.Id, Payload: map[string]any{"kind": live.Kind}})
	return err
}

// Get returns the tenant's notifier, or nil when none is set.
func (s *Service) Get(ctx context.Context) (*Notifier, error) {
	var result *Notifier
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := repo.New(tx).LiveNotifier(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result = &Notifier{Id: row.Id, Kind: row.Kind, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt}
		return nil
	})
	return result, err
}

// NotifyArgs is the job that sends one notification.
type NotifyArgs struct {
	TenantId   uuid.UUID `json:"tenantId"`
	ProposalId uuid.UUID `json:"proposalId"`
}

func (NotifyArgs) Kind() string { return "mandate.notify" }

// Enqueue queues a notification that the proposal waits, inside the
// transaction that leaves it waiting, when the tenant has a notifier. A
// proposal is notified once, however often it is left waiting.
func (s *Service) Enqueue(ctx context.Context, tx pgx.Tx, proposalId uuid.UUID) error {
	if s.queue == nil {
		return nil
	}
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	has, err := repo.New(tx).HasLiveNotifier(ctx)
	if err != nil || !has {
		return err
	}
	return s.queue.InsertTx(ctx, tx, NotifyArgs{TenantId: tenantId, ProposalId: proposalId}, &river.InsertOpts{
		Queue: jobs.QueueDeliveries, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true},
	})
}

// ErrRefused is a chat tool's final refusal of a notification, such as a
// webhook deleted in the tool.
var ErrRefused = errors.New("the chat tool refused the notification")

// Notify sends the notification about a proposal, as the system. A
// proposal decided meanwhile, or a tenant without a notifier, sends
// nothing. A refusal the tool will repeat is ErrRefused; anything else is
// worth a retry.
func (s *Service) Notify(ctx context.Context, tenantId, proposalId uuid.UUID) error {
	ctx = actor.AsSystem(ctx, tenantId)
	if s.describe == nil {
		return errors.New("notifications: no describe set")
	}
	message, waiting, err := s.describe(ctx, proposalId)
	if err != nil || !waiting {
		return err
	}
	var target *repo.NotifierOfRow
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).NotifierOf(ctx)
		if len(rows) > 0 {
			target = &rows[0]
		}
		return err
	}); err != nil || target == nil {
		return err
	}
	if err := s.post(ctx, target.Kind, target.Url, s.render(target.Kind, message)); err != nil {
		return err
	}
	// The message is out. If its record fails, the job must not post it
	// again: the miss is logged instead.
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := events.Append(ctx, tx, events.Event{Type: "proposal.notified", SubjectId: proposalId, Payload: map[string]any{"kind": target.Kind}})
		return err
	}); err != nil {
		slog.ErrorContext(ctx, "a notification was sent but not recorded", "tenant", tenantId, "proposal", proposalId, "error", err)
	}
	return nil
}

// post sends one message to a chat tool. A refusal the tool will repeat is
// ErrRefused; anything else is worth a retry. No error names the address.
func (s *Service) post(ctx context.Context, kind, address string, message map[string]any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return errors.New("notify: the address does not read as a URL")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		// An error from the client names the address, which is a secret:
		// only the cause goes on, to the job's record and the log.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("notify %s: %w", kind, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	switch status := response.StatusCode; {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusTooManyRequests || status >= 500:
		return fmt.Errorf("notify: the chat tool answered %d", status)
	default:
		return fmt.Errorf("%w: it answered %d", ErrRefused, status)
	}
}

// SendTest posts a test message to the tenant's notifier, so an owner sees
// that it works. Only an owner may.
func (s *Service) SendTest(ctx context.Context) error {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error { return db.RequireOwner(ctx, tx) }); err != nil {
		return fault.FromDb(err)
	}
	// The address is read as the system, the only reader it has.
	system := actor.AsSystem(ctx, tenantId)
	var target *repo.NotifierOfRow
	if err := db.InTx(system, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).NotifierOf(system)
		if len(rows) > 0 {
			target = &rows[0]
		}
		return err
	}); err != nil {
		return err
	}
	if target == nil {
		return fault.New(fault.NotFound, "no notifier is set")
	}
	text := "pgmandate: a test message. When a proposal waits for a person, a message like the others will come here."
	message := map[string]any{"text": text}
	if target.Kind == KindDiscord {
		message = map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}}
	}
	if err := s.post(ctx, target.Kind, target.Url, message); err != nil {
		slog.WarnContext(ctx, "a test notification did not arrive", "tenant", tenantId, "error", err)
		return fault.New(fault.InvalidArgument, "the chat tool did not take the test message ("+err.Error()+"): check the address, or set a new one")
	}
	return nil
}

// reasons are the skip reasons as a person reads them; the web app has the
// same words.
var reasons = map[string]string{
	"no_mandate":            "no mandate covers it",
	"risk_above_mandate":    "the risk is above what the mandate allows",
	"budget_spent":          "the mandate's budget for this month is spent",
	"condition_not_met":     "the payload does not meet the mandate's condition",
	"flagged":               "it carries text that tries to steer a model",
	"action_not_executable": "a person always decides this action",
	"action_failed":         "the system that carries it out refused it",
	"execution_failed":      "the execution failed",
	"agent_revoked":         "the agent is revoked",
	"action_changed":        "the action's risk was raised since it was proposed",
	"payload_invalid":       "the payload no longer fits the action's schema",
}

// render writes the message for the chat tool. Every value in it comes
// from an owner's definitions or the server, and is escaped all the same.
func (s *Service) render(kind string, m Message) map[string]any {
	reason := reasons[m.Reason]
	if reason == "" && m.Reason != "" {
		reason = "it was left for a person"
	}
	link := ""
	if s.publicUrl != "" {
		link = s.publicUrl + "/inbox/" + m.ProposalId.String()
	}
	if kind == KindSlack {
		escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
		text := fmt.Sprintf("*A proposal waits for a person:* `%s`, %s risk, from %s.", escape(m.Action), escape(m.Risk), escape(m.Agent))
		if reason != "" {
			text += " Why: " + reason + "."
		}
		if link != "" {
			text += " <" + link + "|Open it in pgmandate>"
		}
		return map[string]any{"text": text}
	}
	escape := strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "'", "|", "\\|", "@", "@\u200b", "<", "\\<").Replace
	text := fmt.Sprintf("**A proposal waits for a person:** `%s`, %s risk, from %s.", strings.ReplaceAll(m.Action, "`", "'"), escape(m.Risk), escape(m.Agent))
	if reason != "" {
		text += " Why: " + reason + "."
	}
	if link != "" {
		text += " Open it in pgmandate: " + link
	}
	// No mention of anyone, whatever the text holds.
	return map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}}
}

type worker struct {
	river.WorkerDefaults[NotifyArgs]
	service *Service
}

func (w *worker) Work(ctx context.Context, job *river.Job[NotifyArgs]) error {
	err := w.service.Notify(ctx, job.Args.TenantId, job.Args.ProposalId)
	if errors.Is(err, ErrRefused) {
		slog.WarnContext(ctx, "a notification was refused", "tenant", job.Args.TenantId, "proposal", job.Args.ProposalId, "error", err)
		return river.JobCancel(err)
	}
	return err
}
