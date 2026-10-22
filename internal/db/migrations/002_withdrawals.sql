-- Destination addresses a customer has saved. New addresses are held for a
-- cooling off period by the risk engine.
CREATE TABLE addresses (
    account_id  UUID        NOT NULL REFERENCES accounts (id),
    address     TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (account_id, address)
);

-- A withdrawal request and its lifecycle:
--   approved        funds sent to the external account straight away
--   pending_review  funds parked in the asset's hold account
--   rejected        refused by the risk engine (nothing moved) or by a
--                   reviewer (held funds released back to the customer)
CREATE TABLE withdrawals (
    id               UUID        PRIMARY KEY,
    account_id       UUID        NOT NULL REFERENCES accounts (id),
    address          TEXT        NOT NULL,
    amount           BIGINT      NOT NULL CHECK (amount > 0),
    status           TEXT        NOT NULL
                     CHECK (status IN ('approved', 'pending_review', 'rejected')),
    reasons          TEXT[]      NOT NULL DEFAULT '{}',
    idempotency_key  TEXT        NOT NULL UNIQUE,
    hold_txn         UUID        REFERENCES transactions (id),
    final_txn        UUID        REFERENCES transactions (id),
    reviewed_by      TEXT,
    created_at       TIMESTAMPTZ NOT NULL,
    decided_at       TIMESTAMPTZ
);

CREATE INDEX withdrawals_account_time ON withdrawals (account_id, created_at DESC);
CREATE INDEX withdrawals_pending ON withdrawals (created_at) WHERE status = 'pending_review';
