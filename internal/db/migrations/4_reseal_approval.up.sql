-- The second administrator of a re-seal (#308): one administrator asks, with
-- the reason, and another approves. The request stays on the finding.
ALTER TABLE integrity_findings
    ADD COLUMN reseal_requested_by UUID,
    ADD COLUMN reseal_requested_at TIMESTAMPTZ,
    ADD COLUMN reseal_request_reason TEXT,
    ADD CONSTRAINT integrity_findings_reseal_request_check CHECK (
        (reseal_requested_by IS NULL) = (reseal_requested_at IS NULL)
        AND (reseal_requested_by IS NULL) = (reseal_request_reason IS NULL)),
    ADD CONSTRAINT integrity_findings_reseal_request_reason_check CHECK (length(reseal_request_reason) <= 1000);
