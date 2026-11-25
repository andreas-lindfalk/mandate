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

/** Where people are told that a proposal waits for them. */
function NotifierCard({ isOwner }: { isOwner: boolean }) {
  const queryClient = useQueryClient();
  const notifier = useQuery({ queryKey: ["notifier"], queryFn: () => api.notifiers.getNotifier({}) });
  const [kind, setKind] = React.useState("slack");
  const [url, setUrl] = React.useState("");
  const done = (message: string) => {
    toast.success(message);
    setUrl("");
    queryClient.invalidateQueries({ queryKey: ["notifier"] });
    queryClient.invalidateQueries({ queryKey: ["chain"] });
  };
  const set = useMutation({
    mutationFn: () => api.notifiers.setNotifier({ kind, url: url.trim() }),
    onSuccess: () => done("Notifications are on"),
    onError: (error) => toast.error("Could not set notifications", { description: errorText(error) }),
  });
  const remove = useMutation({
    mutationFn: () => api.notifiers.removeNotifier({}),
    onSuccess: () => {
      setConfirmingOff(false);
      done("Notifications are off");
    },
    onError: (error) => toast.error("Could not turn notifications off", { description: errorText(error) }),
  });
  const test = useMutation({
    mutationFn: () => api.notifiers.testNotifier({}),
    onSuccess: () => toast.success("A test message was sent: check the channel"),
    onError: (error) => toast.error("The test message did not arrive", { description: errorText(error) }),
  });
  const [confirmingOff, setConfirmingOff] = React.useState(false);
  const current = notifier.data?.notifier;
  const kindLabel: Record<string, string> = { slack: "Slack", discord: "Discord" };
  return (
    <Card className="mb-6 p-5">
      <SectionLabel>Notifications</SectionLabel>
      <p className="mt-2 max-w-2xl text-[13.5px] text-muted">
        A message in a chat channel when a proposal waits for a person: the action, its risk, the agent, why it waits and a link here. Nothing the
        agent wrote is sent there.
      </p>
      {notifier.isPending ? (
        <Skeleton className="mt-3 h-9" />
      ) : notifier.isError ? (
        <ErrorState error={notifier.error} onRetry={() => notifier.refetch()} />
      ) : (
        <div className="mt-3 flex flex-wrap items-center gap-3 text-[13.5px]">
          {current ? (
            <>
              <Badge tone="success">On</Badge>
              <span>
                To a {kindLabel[current.kind] ?? current.kind} channel, since {formatDate(current.createdAt)}. The address is kept secret and is not shown.
              </span>
              {isOwner ? (
                <div className="ml-auto flex gap-2">
                  <Button variant="ghost" size="sm" onClick={() => test.mutate()} loading={test.isPending}>
                    Send a test message
                  </Button>
                  {confirmingOff ? (
                    <>
                      <Button variant="ghost" size="sm" onClick={() => setConfirmingOff(false)}>
                        Cancel
                      </Button>
                      <Button variant="danger" size="sm" onClick={() => remove.mutate()} loading={remove.isPending}>
                        Turn off now
                      </Button>
                    </>
                  ) : (
                    <Button variant="secondary" size="sm" onClick={() => setConfirmingOff(true)}>
                      Turn off
                    </Button>
                  )}
                </div>
              ) : null}
            </>
          ) : (
            <>
              <Badge tone="neutral">Off</Badge>
              <span className="text-muted">{isOwner ? "Paste a channel's incoming webhook to turn them on." : "An owner can turn them on."}</span>
            </>
          )}
        </div>
      )}
      {isOwner ? (
        <form
          className="mt-4 grid gap-3 sm:grid-cols-[140px_1fr_auto] sm:items-end"
          onSubmit={(e) => {
            e.preventDefault();
            set.mutate();
          }}
        >
          <Field label="Chat tool" htmlFor="notifier-kind">
            <Select
              id="notifier-kind"
              value={kind}
              onValueChange={setKind}
              options={[
                { value: "slack", label: "Slack" },
                { value: "discord", label: "Discord" },
              ]}
            />
          </Field>
          <Field label={current ? "New incoming webhook, replacing the current one" : "Incoming webhook"} htmlFor="notifier-url">
            <Input
              id="notifier-url"
              type="text"
              autoComplete="off"
              spellCheck={false}
              data-1p-ignore
              data-lpignore="true"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder={kind === "slack" ? "https://hooks.slack.com/services/..." : "https://discord.com/api/webhooks/..."}
            />
          </Field>
          <Button type="submit" disabled={!url.trim()} loading={set.isPending}>
            {current ? "Replace" : "Turn on"}
          </Button>
        </form>
      ) : null}
    </Card>
  );
}

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
        <NotifierCard isOwner={session.isOwner} />
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
