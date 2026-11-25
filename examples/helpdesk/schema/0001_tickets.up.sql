-- The example host: a helpdesk whose tickets agents may open and close.
-- Every write names the proposal that caused it, and the policy lets a row
-- be written only by a person or by the system while executing exactly
-- that proposal (mandate.may_write_for). No agent, and no code path outside
-- an approval or an execution, can touch a ticket.
CREATE TABLE helpdesk_tickets (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    -- The proposal behind the latest write to this row.
    last_proposal_id  uuid NOT NULL,
    subject           text NOT NULL CHECK (length(subject) BETWEEN 1 AND 200),
    body              text NOT NULL CHECK (length(body) <= 5000),
    priority          text NOT NULL CHECK (priority IN ('low', 'normal', 'high')),
    status            text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    resolution        text CHECK (resolution IS NULL OR length(resolution) <= 2000),
    refund_amount     numeric(12, 2) CHECK (refund_amount IS NULL OR refund_amount > 0),
    created_at        timestamptz NOT NULL DEFAULT now(),
    closed_at         timestamptz,
    FOREIGN KEY (tenant_id, last_proposal_id) REFERENCES mandate.proposals (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX helpdesk_tickets_tenant_idx ON helpdesk_tickets (tenant_id, created_at DESC);

ALTER TABLE helpdesk_tickets ENABLE ROW LEVEL SECURITY;
CREATE POLICY helpdesk_tickets_read ON helpdesk_tickets FOR SELECT
    USING (tenant_id = mandate.current_tenant());
CREATE POLICY helpdesk_tickets_insert ON helpdesk_tickets FOR INSERT
    WITH CHECK (tenant_id = mandate.current_tenant() AND mandate.may_write_for(last_proposal_id));
CREATE POLICY helpdesk_tickets_update ON helpdesk_tickets FOR UPDATE
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant() AND mandate.may_write_for(last_proposal_id));

GRANT SELECT, INSERT, UPDATE ON helpdesk_tickets TO mandate_app;
