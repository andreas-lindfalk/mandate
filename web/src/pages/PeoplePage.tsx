import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Users } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { PageHeader } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/button";
import { Badge, Card, ErrorState, Field, Input, SectionLabel, Skeleton } from "@/components/ui/primitives";
import { Select } from "@/components/ui/select";
import type { Person } from "@/gen/mandate/v1/persons_pb";
import { api } from "@/lib/api";
import { errorText } from "@/lib/errors";
import { formatDate, roleLabel } from "@/lib/format";
import { useSession } from "@/lib/session";

function PersonRow({ person, canRemove, onChanged }: { person: Person; canRemove: boolean; onChanged: () => void }) {
  const [confirming, setConfirming] = React.useState(false);
  const remove = useMutation({
    mutationFn: () => api.persons.removePerson({ id: person.id }),
    onSuccess: () => {
      toast.success(`${person.name} is removed`);
      onChanged();
    },
    onError: (error) => toast.error("Could not remove", { description: errorText(error) }),
  });
  const removed = !!person.removedAt;
  return (
    <li className="border-t border-border px-5 py-4 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <p className={removed ? "font-medium text-muted line-through" : "font-medium"}>{person.name}</p>
        <Badge tone={person.role === "owner" ? "accent" : "neutral"}>{roleLabel[person.role] ?? person.role}</Badge>
        {removed ? <Badge tone="neutral">removed {formatDate(person.removedAt)}</Badge> : null}
        {canRemove && !removed ? (
          <div className="ml-auto flex gap-2">
            {confirming ? (
              <>
                <Button variant="ghost" size="sm" onClick={() => setConfirming(false)}>
                  Cancel
                </Button>
                <Button variant="danger" size="sm" onClick={() => remove.mutate()} loading={remove.isPending}>
                  Remove now
                </Button>
              </>
            ) : (
              <Button variant="secondary" size="sm" onClick={() => setConfirming(true)}>
                Remove
              </Button>
            )}
          </div>
        ) : null}
      </div>
      <p className="mt-1 text-[13px] text-muted">
        {person.email} · since {formatDate(person.createdAt)}
      </p>
    </li>
  );
}

export function PeoplePage() {
  const session = useSession();
  const queryClient = useQueryClient();
  const people = useQuery({ queryKey: ["people"], queryFn: () => api.persons.listPersons({}) });
  const [email, setEmail] = React.useState("");
  const [name, setName] = React.useState("");
  const [role, setRole] = React.useState("reviewer");
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["people"] });
    queryClient.invalidateQueries({ queryKey: ["chain"] });
  };
  const add = useMutation({
    mutationFn: () => api.persons.addPerson({ email: email.trim(), name: name.trim(), role }),
    onSuccess: () => {
      setEmail("");
      setName("");
      refresh();
    },
    onError: (error) => toast.error("Could not add", { description: errorText(error) }),
  });
  return (
    <div>
      <PageHeader
        eyebrow={
          <>
            <Users className="size-3.5" /> Who decides
          </>
        }
        title="People"
      />
      <div className="flex max-w-3xl flex-col gap-6 px-6 py-6 lg:px-8">
        <p className="text-[15px] leading-relaxed text-foreground/85">
          Owners define actions, register agents, grant mandates and decide proposals. Reviewers decide proposals. A person signs in with the
          email address listed here. Removing someone ends their access at once; their name stays in the audit trail.
        </p>
        {session.isOwner ? (
          <Card className="p-5">
            <SectionLabel>Add a person</SectionLabel>
            <form
              className="mt-3 grid gap-3 sm:grid-cols-[1.4fr_1fr_140px_auto] sm:items-end"
              onSubmit={(e) => {
                e.preventDefault();
                add.mutate();
              }}
            >
              <Field label="Email" htmlFor="person-email">
                <Input id="person-email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
              </Field>
              <Field label="Name" htmlFor="person-name">
                <Input id="person-name" value={name} onChange={(e) => setName(e.target.value)} />
              </Field>
              <Field label="Role" htmlFor="person-role">
                <Select
                  id="person-role"
                  value={role}
                  onValueChange={setRole}
                  options={[
                    { value: "reviewer", label: "Reviewer" },
                    { value: "owner", label: "Owner" },
                  ]}
                />
              </Field>
              <Button type="submit" disabled={!email.trim() || !name.trim()} loading={add.isPending}>
                <Plus /> Add
              </Button>
            </form>
          </Card>
        ) : (
          <p className="text-[13.5px] text-muted">You are a reviewer here. Owners add and remove people.</p>
        )}
        {people.isPending ? (
          <Skeleton className="h-32" />
        ) : people.isError ? (
          <ErrorState error={people.error} onRetry={() => people.refetch()} />
        ) : (
          <Card>
            <ul>
              {people.data.persons.map((person) => (
                <PersonRow key={person.id} person={person} canRemove={session.isOwner} onChanged={refresh} />
              ))}
            </ul>
          </Card>
        )}
      </div>
    </div>
  );
}
