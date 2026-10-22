-- Transactional outbox. Events are inserted in the same database transaction
-- as the state change they announce, so an approved withdrawal and its
-- "send it" event either both exist or neither does. A relay process then
-- delivers events to the signer and marks them published.
CREATE TABLE outbox (
    id               BIGSERIAL   PRIMARY KEY,
    topic            TEXT        NOT NULL,
    -- Idempotency key for consumers; also stops the same event being
    -- enqueued twice.
    key              TEXT        NOT NULL,
    payload          JSONB       NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts         INT         NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error       TEXT,
    published_at     TIMESTAMPTZ,
    UNIQUE (topic, key)
);

CREATE INDEX outbox_due ON outbox (next_attempt_at, id) WHERE published_at IS NULL;
