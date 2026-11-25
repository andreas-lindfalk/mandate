-- Back to 0005: payload keys are not checked for case.

DROP TRIGGER proposals_payload_keys ON mandate.proposals;
DROP FUNCTION mandate.proposals_payload_keys();
DROP FUNCTION mandate.keys_unambiguous(jsonb);
