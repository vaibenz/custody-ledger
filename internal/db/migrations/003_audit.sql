-- Append only audit trail. Every state change writes a row in the same
-- database transaction as the change itself, so the log can never disagree
-- with the ledger. A trigger refuses UPDATE and DELETE, so rows cannot be
-- rewritten after the fact even by application code.
CREATE TABLE audit_log (
    id          BIGSERIAL   PRIMARY KEY,
    actor       TEXT        NOT NULL,
    action      TEXT        NOT NULL,
    subject     TEXT        NOT NULL,
    detail      JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_subject ON audit_log (subject, id);

CREATE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
