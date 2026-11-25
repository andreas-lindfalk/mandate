// Package actor carries the tenant, the actor and the run through a request.
// Everything that touches the database reads the scope from here and sets
// the row level security variables per transaction; a tenant id is never
// passed around it as a loose parameter.
package actor

import (
	"context"

	"github.com/google/uuid"
)

// Kind is who acts. A person decides; an agent proposes; the system executes
// under a mandate. Nothing an agent does can look like a person did it.
type Kind string

const (
	Person Kind = "person"
	Agent  Kind = "agent"
	System Kind = "system"
)

type Actor struct {
	Kind Kind
	Id   uuid.UUID
}

// PersonId is the actor's id when the actor is a person, else nil: what a
// row's decider may be set to. Agents and the system own nothing.
func (a Actor) PersonId() *uuid.UUID {
	if a.Kind != Person {
		return nil
	}
	id := a.Id
	return &id
}

type actorKey struct{}
type tenantKey struct{}
type runKey struct{}

func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

func From(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// WithTenant scopes the context to a tenant.
func WithTenant(ctx context.Context, tenantId uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantId)
}

func TenantFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(tenantKey{}).(uuid.UUID)
	return id, ok
}

// WithRun tags the context with a run id: the agent's own id for a chain of
// work, written to every proposal and event it causes.
func WithRun(ctx context.Context, runId string) context.Context {
	return context.WithValue(ctx, runKey{}, runId)
}

func RunFrom(ctx context.Context) string {
	runId, _ := ctx.Value(runKey{}).(string)
	return runId
}

// AsSystem scopes ctx to a tenant as the system: what jobs and the
// executor run as.
func AsSystem(ctx context.Context, tenantId uuid.UUID) context.Context {
	return WithTenant(WithActor(ctx, Actor{Kind: System}), tenantId)
}

// AsPerson scopes ctx to a tenant as a person.
func AsPerson(ctx context.Context, tenantId, personId uuid.UUID) context.Context {
	return WithTenant(WithActor(ctx, Actor{Kind: Person, Id: personId}), tenantId)
}

// AsAgent scopes ctx to a tenant as an agent.
func AsAgent(ctx context.Context, tenantId, agentId uuid.UUID) context.Context {
	return WithTenant(WithActor(ctx, Actor{Kind: Agent, Id: agentId}), tenantId)
}
