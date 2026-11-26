// Package testfix is the shared fixture of the integration tests: a real
// Postgres with pgmandate's and the helpdesk's schema, a kernel with the
// helpdesk actions and a few synthetic ones, and tenants with an owner, a
// reviewer and an agent ready to act.
package testfix

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/andreas-lindfalk/pgmandate"
	"github.com/andreas-lindfalk/pgmandate/examples/helpdesk"
	"github.com/andreas-lindfalk/pgmandate/modules/agents"
	"github.com/andreas-lindfalk/pgmandate/modules/tenants"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

// World is one Postgres and one kernel for a suite.
type World struct {
	Pg     *testkit.Postgres
	Kernel *pgmandate.Kernel
	// Queue is set when the world was started with jobs.
	Queue *jobs.Queue
	// Chat receives the notifications.
	Chat *Chat

	mu      sync.Mutex
	now     *time.Time
	applied []uuid.UUID
}

// Start starts Postgres and a kernel. With jobs, the kernel executes
// proposals through River; without, tests call ExecuteProposal themselves.
func Start(ctx context.Context, withJobs bool) (*World, error) {
	pg, err := testkit.StartPostgres(ctx, helpdesk.Migrations)
	if err != nil {
		return nil, err
	}
	w := &World{Pg: pg, Chat: newChat()}
	opts := pgmandate.Options{Now: w.Now, AllowPrivateConnectors: true, PublicUrl: PublicUrl, NotifyClient: w.Chat.Client(), Signer: Signer}
	if withJobs {
		w.Queue = jobs.NewQueue(pg.App)
		opts.Queue = w.Queue
	}
	w.Kernel = pgmandate.New(pg.App, opts)
	for _, action := range append(helpdesk.Actions(), w.actions()...) {
		if err := w.Kernel.Register(action); err != nil {
			pg.Close()
			return nil, err
		}
	}
	if withJobs {
		if err := w.Queue.Init(2); err != nil {
			pg.Close()
			return nil, err
		}
		if err := w.Queue.Start(ctx); err != nil {
			pg.Close()
			return nil, err
		}
	}
	return w, nil
}

// Instance is a second server on the world's database: its own pool, its
// own kernel with the same actions, and its own running queue, as two
// replicas behind a load balancer are. stop closes it.
func (w *World) Instance(ctx context.Context) (*pgmandate.Kernel, func(), error) {
	pool, err := db.Open(ctx, w.Pg.AppUrl)
	if err != nil {
		return nil, nil, err
	}
	queue := jobs.NewQueue(pool)
	kernel := pgmandate.New(pool, pgmandate.Options{Now: w.Now, AllowPrivateConnectors: true, PublicUrl: PublicUrl, NotifyClient: w.Chat.Client(), Signer: Signer, Queue: queue})
	for _, action := range append(helpdesk.Actions(), w.actions()...) {
		if err := kernel.Register(action); err != nil {
			pool.Close()
			return nil, nil, err
		}
	}
	if err := queue.Init(2); err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err := queue.Start(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = queue.Stop(stopCtx)
		pool.Close()
	}
	return kernel, stop, nil
}

func (w *World) Close() {
	if w.Queue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Queue.Stop(ctx)
	}
	w.Chat.server.Close()
	w.Pg.Close()
}

// Now is the clock the kernel stamps new agent tokens with: the real time,
// or what SetNow set. Every other time is the database's.
func (w *World) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.now != nil {
		return *w.now
	}
	return time.Now()
}

// SetNow moves the kernel's clock; the zero time restores the real one.
func (w *World) SetNow(at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if at.IsZero() {
		w.now = nil
		return
	}
	w.now = &at
}

// CommitUnder runs second while first's transaction, scoped by ctx, is
// open: first has made its changes and holds their locks. Once a statement
// whose text contains waitsIn waits on a lock, first commits, and
// CommitUnder returns what second returned. second thus reads the state
// from before first and writes after it, as the loser of two concurrent
// calls does.
func (w *World) CommitUnder(tb testing.TB, ctx context.Context, first func(tx pgx.Tx) error, waitsIn string, second func() error) error {
	tb.Helper()
	held, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		firstDone <- db.InTx(ctx, w.Pg.App, func(tx pgx.Tx) error {
			if err := first(tx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()
	select {
	case <-held:
	case err := <-firstDone:
		tb.Fatalf("the first transaction failed: %v", err)
	case <-time.After(10 * time.Second):
		tb.Fatal("timed out waiting for the first transaction")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- second() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		err := w.Pg.Owner.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND strpos(query, $1) > 0)`, waitsIn).Scan(&waiting)
		if err != nil {
			tb.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-secondDone:
			tb.Fatalf("the second call returned without waiting on a lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %q to wait on a lock", waitsIn)
		}
		time.Sleep(10 * time.Millisecond)
	}
	releaseOnce()
	if err := <-firstDone; err != nil {
		tb.Fatalf("the first transaction did not commit: %v", err)
	}
	select {
	case err := <-secondDone:
		return err
	case <-time.After(10 * time.Second):
		tb.Fatal("timed out waiting for the second call")
		return nil
	}
}

// Applied lists the proposals the synthetic actions have applied, in order.
func (w *World) Applied() []uuid.UUID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.applied)
}

func (w *World) record(_ context.Context, _ pgx.Tx, p pgmandate.Proposal) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.applied = append(w.applied, p.Id)
	return nil
}

// ErrApplyFailed is what test.failing returns.
var ErrApplyFailed = errors.New("the action failed on purpose")

// The synthetic actions: one per risk, one the host never executes, one
// that fails, one that refuses.
func (w *World) actions() []pgmandate.Action {
	return []pgmandate.Action{
		{Key: "test.low", Description: "low risk", Risk: pgmandate.RiskLow, MayExecute: true, Apply: w.record},
		{Key: "test.medium", Description: "medium risk", Risk: pgmandate.RiskMedium, MayExecute: true, Apply: w.record},
		{Key: "test.high", Description: "high risk", Risk: pgmandate.RiskHigh, MayExecute: true, Apply: w.record},
		{Key: "test.manual", Description: "never without a person", Risk: pgmandate.RiskLow, MayExecute: false, Apply: w.record},
		{Key: "test.failing", Description: "fails to apply", Risk: pgmandate.RiskLow, MayExecute: true,
			Apply: func(context.Context, pgx.Tx, pgmandate.Proposal) error { return ErrApplyFailed }},
		{Key: "test.refusing", Description: "refuses with a coded error", Risk: pgmandate.RiskLow, MayExecute: true,
			Apply: func(context.Context, pgx.Tx, pgmandate.Proposal) error {
				return fault.New(fault.NotFound, "no such row")
			}},
	}
}

// Tenant is a tenant with the people and the agent a test needs.
type Tenant struct {
	Id         uuid.UUID
	OwnerId    uuid.UUID
	ReviewerId uuid.UUID
	AgentId    uuid.UUID
	Token      string
	Grant      agents.Grant

	System   context.Context
	Owner    context.Context
	Reviewer context.Context
	Agent    context.Context
}

// NewTenant creates a tenant with an owner, a reviewer and an agent with a
// token, and purges it when the test ends; a purge that fails fails the
// test.
func (w *World) NewTenant(tb testing.TB, name string) Tenant {
	tb.Helper()
	ctx := context.Background()
	tenant, err := w.Kernel.Tenants.Create(ctx, name)
	if err != nil {
		tb.Fatalf("create tenant: %v", err)
	}
	// The purge is the one way a tenant's append-only rows leave, so every
	// test tenant doubles as a test of it: a trigger that blocked the cascade
	// fails the test that left the rows.
	tb.Cleanup(func() {
		if err := w.Pg.PurgeTenant(context.Background(), tenant.Id); err != nil {
			tb.Errorf("purge tenant: %v", err)
		}
	})
	t := Tenant{Id: tenant.Id, System: actor.AsSystem(ctx, tenant.Id)}
	suffix := tenant.Id.String()[:8]
	owner, err := w.Kernel.Tenants.AddPerson(t.System, "owner-"+suffix+"@test.example", "Owner", tenants.RoleOwner)
	if err != nil {
		tb.Fatalf("add owner: %v", err)
	}
	t.OwnerId = owner.Id
	t.Owner = actor.AsPerson(ctx, tenant.Id, owner.Id)
	reviewer, err := w.Kernel.Tenants.AddPerson(t.Owner, "reviewer-"+suffix+"@test.example", "Reviewer", tenants.RoleReviewer)
	if err != nil {
		tb.Fatalf("add reviewer: %v", err)
	}
	t.ReviewerId = reviewer.Id
	t.Reviewer = actor.AsPerson(ctx, tenant.Id, reviewer.Id)
	// The fixture's agent is live, so that what tests propose can be carried
	// out; a dry-run agent is made with DryRunAgent.
	agent, err := w.Kernel.Agents.Create(t.Owner, "bot", "", agents.ModeLive)
	if err != nil {
		tb.Fatalf("create agent: %v", err)
	}
	t.AgentId = agent.Id
	t.Agent = actor.AsAgent(ctx, tenant.Id, agent.Id)
	_, secret, err := w.Kernel.Agents.IssueToken(t.Owner, agent.Id, 30)
	if err != nil {
		tb.Fatalf("issue token: %v", err)
	}
	t.Token = secret
	t.Grant, err = w.Kernel.Agents.Authenticate(ctx, secret)
	if err != nil {
		tb.Fatalf("authenticate: %v", err)
	}
	return t
}

// DryRun is an agent of a tenant in dry run, with its scope and a token.
type DryRun struct {
	Id    uuid.UUID
	Agent context.Context
	Token string
	Grant agents.Grant
}

// DryRunAgent registers a second agent of the tenant, in dry run.
func (w *World) DryRunAgent(tb testing.TB, t Tenant) DryRun {
	tb.Helper()
	agent, err := w.Kernel.Agents.Create(t.Owner, "trial-bot", "", agents.ModeDryRun)
	if err != nil {
		tb.Fatalf("create dry-run agent: %v", err)
	}
	_, secret, err := w.Kernel.Agents.IssueToken(t.Owner, agent.Id, 30)
	if err != nil {
		tb.Fatalf("issue token: %v", err)
	}
	grant, err := w.Kernel.Agents.Authenticate(context.Background(), secret)
	if err != nil {
		tb.Fatalf("authenticate: %v", err)
	}
	return DryRun{Id: agent.Id, Agent: actor.AsAgent(context.Background(), t.Id, agent.Id), Token: secret, Grant: grant}
}

// Signer is the key the fixture's kernel signs deliveries with, and Keys
// the set a receiver in a test trusts.
var (
	Signer = mustSigner()
	Keys   = sign.NewKeySet(Signer.PublicKey())
)

func mustSigner() *sign.Signer {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(100 + i)
	}
	s, err := sign.NewSigner(seed)
	if err != nil {
		panic(err)
	}
	return s
}
