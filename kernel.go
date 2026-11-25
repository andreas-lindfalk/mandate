// Package pgmandate is the kernel: agents propose; a person, or a person's
// standing permission, decides.
//
// Owners define actions as data: what an agent may propose, with a schema,
// a risk, how long a proposal for it may wait, and a connector, usually a
// signed webhook. People grant
// mandates: which agent's proposals for which actions may be carried out
// without them, up to which risk, within which monthly budget, under which
// policy reference and until when. Everything else waits for a person. A
// decided proposal is delivered to its connector, and every decision,
// delivery and refusal is a row in a hash linked log.
//
// The pgmandate binary serves all of this. A host that shares the database
// can instead embed the kernel and register builtin actions, which are
// carried out by its own code inside the deciding transaction.
package pgmandate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/andreas-lindfalk/pgmandate/mcp"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/agents"
	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/notifications"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/modules/tenants"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/schema"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

// Migrations is mandate's schema, to apply before the host's own.
var Migrations = schema.Migrations

type (
	Action         = proposals.Action
	Proposal       = proposals.Proposal
	ExecutionBasis = proposals.ExecutionBasis
	Mandate        = mandates.Mandate
	Grant          = agents.Grant
)

const (
	RiskLow    = mandates.RiskLow
	RiskMedium = mandates.RiskMedium
	RiskHigh   = mandates.RiskHigh
)

// Options tune a kernel.
type Options struct {
	// Queue runs executions as durable jobs. Nil means nothing is ever
	// executed without a person; proposals still queue.
	Queue *jobs.Queue
	// Now replaces the clock, for tests.
	Now func() time.Time
	// AllowPrivateConnectors lets webhook connectors live on loopback and
	// private addresses: for local development and for an on-premise
	// install whose connectors are internal.
	AllowPrivateConnectors bool
	// PublicUrl is where people open the web app, for links in
	// notifications.
	PublicUrl string
	// NotifyClient replaces the HTTP client of notifications, for tests.
	NotifyClient *http.Client
	// Signer signs deliveries: an Ed25519 key kept outside the database.
	// Without one, decisions are recorded but nothing is delivered.
	Signer *sign.Signer
}

type Kernel struct {
	pool  *pgxpool.Pool
	queue *jobs.Queue

	Tenants       *tenants.Service
	Agents        *agents.Service
	Mandates      *mandates.Service
	Proposals     *proposals.Service
	Actions       *actions.Service
	Deliveries    *deliveries.Service
	Notifications *notifications.Service

	mu       sync.RWMutex
	builtin  map[string]Action
	compiled sync.Map // action id -> compiledSchema
}

// compiledSchema is a validator for one version of an action's schema.
type compiledSchema struct {
	updatedAt time.Time
	validate  func(json.RawMessage) (json.RawMessage, error)
}

// New wires a kernel over the pool, which must connect as the app role.
// An embedded host registers its builtin actions before serving; with a
// queue, call queue.Init and Start after New.
func New(pool *pgxpool.Pool, opts Options) *Kernel {
	k := &Kernel{pool: pool, queue: opts.Queue, builtin: map[string]Action{}}
	k.Tenants = tenants.NewService(pool)
	k.Agents = agents.NewService(pool)
	k.Mandates = mandates.NewService(pool)
	k.Mandates.SetBuiltin(k.builtinSchemas)
	k.Actions = actions.NewService(pool)
	k.Proposals = proposals.NewService(pool, k)
	k.Deliveries = deliveries.New(pool, opts.Queue, k.Proposals, deliveries.Options{AllowPrivate: opts.AllowPrivateConnectors, Signer: opts.Signer})
	k.Proposals.SetDeliver(func(ctx context.Context, tx pgx.Tx, proposal Proposal, action Action) error {
		return k.Deliveries.Enqueue(ctx, tx, proposal, action.Webhook.Url, action.Webhook.Audience)
	})
	k.Actions.SetOnMoved(k.Deliveries.Retarget)
	k.Notifications = notifications.New(pool, opts.Queue, notifications.Options{PublicUrl: opts.PublicUrl, Client: opts.NotifyClient})
	k.Notifications.SetDescribe(k.describeWaiting)
	k.Proposals.SetOnWaiting(k.Notifications.Enqueue)
	if opts.Now != nil {
		k.Agents.SetNow(opts.Now)
		k.Mandates.SetNow(opts.Now)
	}
	if opts.Queue != nil {
		river.AddWorker(opts.Queue.Workers(), &executeWorker{kernel: k})
		river.AddWorker(opts.Queue.Workers(), &sweepWorker{kernel: k})
		river.AddWorker(opts.Queue.Workers(), &sweepTenantWorker{kernel: k})
		opts.Queue.AddPeriodic(river.NewPeriodicJob(river.PeriodicInterval(SweepInterval),
			func() (river.JobArgs, *river.InsertOpts) { return SweepArgs{}, nil }, &river.PeriodicJobOpts{RunOnStart: true}))
		k.Proposals.SetAfterStore(k.enqueueExecution)
	}
	return k
}

// Register adds a builtin action: one an embedded host carries out with Go
// code inside the deciding transaction. The key must be dotted lower case,
// the risk one of low, medium or high, and Apply set. The schema, when
// given, is compiled and payloads are validated against it. A tenant's own
// definition of the same key (connector builtin) overrides the description,
// risk, waiting time, schema and whether it may execute, and keeps the code.
func (k *Kernel) Register(action Action) error {
	switch {
	case !mandates.ValidKey(action.Key):
		return fmt.Errorf("action key %q: use dotted lower case such as tickets.create", action.Key)
	case mandates.RiskRank(action.Risk) < 0:
		return fmt.Errorf("action %s: risk must be low, medium or high", action.Key)
	case action.Apply == nil:
		return fmt.Errorf("action %s: Apply is required", action.Key)
	case action.WaitLimit != 0:
		// The database gives an action that exists only as code the
		// default waiting time; a tenant's definition can set another.
		return fmt.Errorf("action %s: a waiting time is set by defining the action, not in code", action.Key)
	}
	if len(action.Schema) > 0 {
		compiled, err := actions.Compile(action.Schema)
		if err != nil {
			return fmt.Errorf("action %s: schema: %w", action.Key, err)
		}
		action.Validate = validator(compiled)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, exists := k.builtin[action.Key]; exists {
		return fmt.Errorf("action %s is already registered", action.Key)
	}
	k.builtin[action.Key] = action
	return nil
}

func validator(compiled *jsonschema.Schema) func(json.RawMessage) (json.RawMessage, error) {
	return func(payload json.RawMessage) (json.RawMessage, error) {
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		if err := compiled.Validate(value); err != nil {
			return nil, err
		}
		return payload, nil
	}
}

// Pool is the connection pool the kernel runs on, for handlers that read
// the chain directly.
func (k *Kernel) Pool() *pgxpool.Pool {
	return k.pool
}

// Builtin lists the actions registered in code, sorted by key.
func (k *Kernel) Builtin() []Action {
	k.mu.RLock()
	defer k.mu.RUnlock()
	result := make([]Action, 0, len(k.builtin))
	for _, action := range k.builtin {
		result = append(result, action)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

// builtinSchemas lists the builtin actions by key with their schemas, for
// checking a mandate's condition at the grant.
func (k *Kernel) builtinSchemas() map[string]json.RawMessage {
	k.mu.RLock()
	defer k.mu.RUnlock()
	result := make(map[string]json.RawMessage, len(k.builtin))
	for key, action := range k.builtin {
		result[key] = action.Schema
	}
	return result
}

// Lookup implements proposals.Actions: the tenant's own definition first,
// the builtin registry as the fallback. A removed definition hides the key.
func (k *Kernel) Lookup(ctx context.Context, key string) (Action, bool, error) {
	defined, err := k.Actions.Get(ctx, key)
	return k.resolve(key, defined, err)
}

// LookupIn is Lookup inside the caller's transaction.
func (k *Kernel) LookupIn(ctx context.Context, tx pgx.Tx, key string) (Action, bool, error) {
	defined, err := k.Actions.GetIn(ctx, tx, key)
	return k.resolve(key, defined, err)
}

func (k *Kernel) resolve(key string, defined actions.Action, err error) (Action, bool, error) {
	k.mu.RLock()
	code, hasCode := k.builtin[key]
	k.mu.RUnlock()
	if err != nil {
		if fault.CodeOf(err) == fault.NotFound {
			return code, hasCode, nil
		}
		return Action{}, false, err
	}
	if defined.RemovedAt != nil {
		return Action{}, false, nil
	}
	return k.merge(defined, code, hasCode)
}

// merge turns a definition into an action, with the builtin code when the
// definition says so and the registry has it.
func (k *Kernel) merge(defined actions.Action, code Action, hasCode bool) (Action, bool, error) {
	action := Action{
		Key: defined.Key, Description: defined.Description, Risk: defined.Risk, WaitLimit: defined.WaitLimit,
		MayExecute: defined.MayExecute, Schema: defined.Schema, Department: defined.Department,
	}
	if len(defined.Schema) > 0 {
		// One entry per action, replaced when the definition changes.
		cached, ok := k.compiled.Load(defined.Id)
		entry, fresh := cached.(compiledSchema)
		if !ok || !fresh || !entry.updatedAt.Equal(defined.UpdatedAt) {
			compiled, err := actions.Compile(defined.Schema)
			if err != nil {
				return Action{}, false, fmt.Errorf("action %s: schema: %w", defined.Key, err)
			}
			entry = compiledSchema{updatedAt: defined.UpdatedAt, validate: validator(compiled)}
			k.compiled.Store(defined.Id, entry)
		}
		action.Validate = entry.validate
	}
	switch defined.ConnectorKind {
	case actions.ConnectorWebhook:
		action.Webhook = &proposals.Webhook{Url: defined.ConnectorUrl, Audience: defined.ConnectorAudience}
	case actions.ConnectorBuiltin:
		if hasCode {
			action.Apply, action.Assess = code.Apply, code.Assess
		}
	}
	return action, true, nil
}

// ListActions lists the actions an agent of the tenant in ctx may propose:
// the tenant's definitions that can be carried out here, and the builtin
// ones it has not defined. A builtin definition without code in this
// binary is left out, since proposing it would only be refused.
func (k *Kernel) ListActions(ctx context.Context) ([]Action, error) {
	defined, err := k.Actions.List(ctx, false)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var result []Action
	for _, d := range defined {
		k.mu.RLock()
		code, hasCode := k.builtin[d.Key]
		k.mu.RUnlock()
		action, _, err := k.merge(d, code, hasCode)
		if err != nil {
			return nil, err
		}
		seen[d.Key] = true
		if action.Applicable() {
			result = append(result, action)
		}
	}
	for _, code := range k.Builtin() {
		if !seen[code.Key] {
			result = append(result, code)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

// Handler serves MCP to agents. Mount it where agents connect, such as /mcp.
func (k *Kernel) Handler() http.Handler {
	return mcp.NewHandler(k)
}

// Unmet implements mcp.Host: what in a payload no mandate's condition took.
func (k *Kernel) Unmet(ctx context.Context, agentId uuid.UUID, action, risk string, payload json.RawMessage) ([]string, error) {
	return k.Mandates.Unmet(ctx, agentId, action, risk, payload)
}

// LiveMandates implements mcp.Host: the mandates that hold now for an agent.
func (k *Kernel) LiveMandates(ctx context.Context) ([]Mandate, error) {
	a, ok := actor.From(ctx)
	if !ok || a.Kind != actor.Agent {
		return nil, errors.New("live mandates: not an agent")
	}
	all, err := k.Mandates.List(ctx, false)
	if err != nil {
		return nil, err
	}
	now := k.Mandates.Now()
	live := all[:0]
	for _, m := range all {
		if (m.AgentId == nil || *m.AgentId == a.Id) && !now.Before(m.ValidFrom) && now.Before(m.ValidUntil) {
			live = append(live, m)
		}
	}
	return slices.Clip(live), nil
}

// SpentCount, DeliveryOf and DeliveriesOf implement mcp.Host.
func (k *Kernel) SpentCount(ctx context.Context, mandateIds []uuid.UUID) (map[uuid.UUID]int64, error) {
	return k.Mandates.SpentFor(ctx, mandateIds)
}

func (k *Kernel) DeliveryOf(ctx context.Context, proposalId uuid.UUID) (deliveries.Delivery, bool, error) {
	return k.Deliveries.Get(ctx, proposalId)
}

func (k *Kernel) DeliveriesOf(ctx context.Context, proposalIds []uuid.UUID) (map[uuid.UUID]deliveries.Delivery, error) {
	return k.Deliveries.ByProposals(ctx, proposalIds)
}

// AgentsService, ProposalsService, ActionsOf and Executes implement mcp.Host.
func (k *Kernel) AgentsService() *agents.Service                  { return k.Agents }
func (k *Kernel) ProposalsService() *proposals.Service            { return k.Proposals }
func (k *Kernel) ActionsOf(ctx context.Context) ([]Action, error) { return k.ListActions(ctx) }
func (k *Kernel) Executes() bool                                  { return k.queue != nil }

// ExecuteArgs is the job that executes a proposal under a mandate.
type ExecuteArgs struct {
	TenantId   uuid.UUID `json:"tenantId"`
	ProposalId uuid.UUID `json:"proposalId"`
}

func (ExecuteArgs) Kind() string { return "mandate.execute_proposal" }

type executeWorker struct {
	river.WorkerDefaults[ExecuteArgs]
	kernel *Kernel
}

func (w *executeWorker) Work(ctx context.Context, job *river.Job[ExecuteArgs]) error {
	return w.kernel.ExecuteJob(ctx, job)
}

// ExecuteJob runs the execution for a job. When the last attempt fails too,
// the failure is written to the chain, so a proposal whose action keeps
// failing is never left pending without a reason.
func (k *Kernel) ExecuteJob(ctx context.Context, job *river.Job[ExecuteArgs]) error {
	err := k.ExecuteProposal(ctx, job.Args.TenantId, job.Args.ProposalId)
	if err != nil && job.Attempt >= job.MaxAttempts {
		slog.ErrorContext(ctx, "execution failed on its last attempt", "proposal", job.Args.ProposalId, "error", err)
		reason := SkipExecutionFailed
		if errors.Is(err, proposals.ErrActionFailed) {
			reason = SkipActionFailed
		}
		// Recorded even when the job's own context has run out.
		scoped := actor.AsSystem(context.WithoutCancel(ctx), job.Args.TenantId)
		if recordErr := k.Proposals.RecordSkip(scoped, job.Args.ProposalId, reason); recordErr != nil {
			return errors.Join(err, recordErr)
		}
	}
	return err
}

// describeWaiting says what a notification about a proposal says: the
// action, its risk, the agent's registered name and why it waits. Nothing
// the agent wrote. False when the proposal no longer waits.
func (k *Kernel) describeWaiting(ctx context.Context, proposalId uuid.UUID) (notifications.Message, bool, error) {
	p, err := k.Proposals.Get(ctx, proposalId)
	if fault.CodeOf(err) == fault.NotFound {
		return notifications.Message{}, false, nil
	}
	if err != nil || p.Status != proposals.StatusPending {
		return notifications.Message{}, false, err
	}
	agent, err := k.Agents.Get(ctx, p.AgentId)
	if err != nil {
		return notifications.Message{}, false, err
	}
	reason, found, err := k.Proposals.SkipReason(ctx, proposalId)
	if err != nil {
		return notifications.Message{}, false, err
	}
	if !found {
		action, ok, err := k.Lookup(ctx, p.Action)
		if err != nil {
			return notifications.Message{}, false, err
		}
		if ok {
			reason = proposals.WhyNotCandidate(action, p)
		}
	}
	return notifications.Message{ProposalId: p.Id, Action: p.Action, Risk: p.Risk, Agent: agent.Name, Reason: reason}, true, nil
}

// enqueueExecution queues the execution in the transaction that stores the
// proposal, so the two are committed or rolled back together.
func (k *Kernel) enqueueExecution(ctx context.Context, tx pgx.Tx, proposalId uuid.UUID) error {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return errors.New("enqueue execution: no tenant in context")
	}
	return k.queue.InsertTx(ctx, tx, ExecuteArgs{TenantId: tenantId, ProposalId: proposalId},
		&river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight}})
}

// SweepInterval is how often the sweep looks for work that is due.
const SweepInterval = time.Minute

// SweepArgs is the periodic job that finds the tenants with work due, such
// as proposals past their deadline. River runs it on its leader only.
type SweepArgs struct{}

func (SweepArgs) Kind() string { return "mandate.sweep" }

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	kernel *Kernel
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	tenantIds, err := w.kernel.tenantsWithDueWork(ctx)
	if err != nil {
		return err
	}
	for _, tenantId := range tenantIds {
		// One job per tenant at a time: a tenant already being swept is
		// not queued twice.
		if err := w.kernel.queue.Insert(ctx, SweepTenantArgs{TenantId: tenantId},
			&river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight}}); err != nil {
			return err
		}
	}
	return nil
}

// SweepTenantArgs does the due work of one tenant, as the system.
type SweepTenantArgs struct {
	TenantId uuid.UUID `json:"tenantId"`
}

func (SweepTenantArgs) Kind() string { return "mandate.sweep_tenant" }

type sweepTenantWorker struct {
	river.WorkerDefaults[SweepTenantArgs]
	kernel *Kernel
}

func (w *sweepTenantWorker) Work(ctx context.Context, job *river.Job[SweepTenantArgs]) error {
	scoped := actor.AsSystem(ctx, job.Args.TenantId)
	if _, err := w.kernel.Proposals.ExpireDue(scoped); err != nil {
		return err
	}
	_, err := w.kernel.Deliveries.FinishDue(scoped)
	return err
}

// ExpireDue expires, in every tenant, the waiting proposals past their
// deadline, and returns how many. The sweep does this every minute; a host
// that runs the kernel without a queue calls it, and FinishDue, itself.
func (k *Kernel) ExpireDue(ctx context.Context) (int, error) {
	return k.forDueTenants(ctx, k.Proposals.ExpireDue)
}

// FinishDue, in every tenant, closes delivery attempts left open by a crash
// and finishes the deliveries past the last moment an attempt may open,
// and returns how many it finished.
func (k *Kernel) FinishDue(ctx context.Context) (int, error) {
	return k.forDueTenants(ctx, k.Deliveries.FinishDue)
}

func (k *Kernel) forDueTenants(ctx context.Context, do func(context.Context) (int, error)) (int, error) {
	tenantIds, err := k.tenantsWithDueWork(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, tenantId := range tenantIds {
		n, err := do(actor.AsSystem(ctx, tenantId))
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// tenantsWithDueWork lists the tenants the sweep must visit. The database
// function returns ids only, across tenants, for the system to scope itself
// to each.
func (k *Kernel) tenantsWithDueWork(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := k.pool.Query(ctx, `SELECT mandate.tenants_with_due_work()`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
