-- Back to 0002: proposals without the agent's account. The rationales and
-- evidence are lost; the log keeps their digests.

DROP TRIGGER proposals_keep_account ON mandate.proposals;
DROP FUNCTION mandate.proposals_keep_account();
DROP TRIGGER proposals_require_rationale ON mandate.proposals;
DROP FUNCTION mandate.proposals_require_rationale();

ALTER TABLE mandate.proposals DROP COLUMN rationale, DROP COLUMN evidence, DROP COLUMN provenance;

DROP FUNCTION mandate.valid_evidence(jsonb);
DROP FUNCTION mandate.valid_provenance(jsonb);
