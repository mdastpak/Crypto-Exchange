ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_reseal_request_reason_check,
    DROP CONSTRAINT integrity_findings_reseal_request_check,
    DROP COLUMN reseal_request_reason,
    DROP COLUMN reseal_requested_at,
    DROP COLUMN reseal_requested_by;
