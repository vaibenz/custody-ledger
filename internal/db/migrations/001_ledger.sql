-- Double entry ledger.
--
-- Every movement of value is a transaction made of two or more entries whose
-- amounts sum to zero. Account balances are a cached projection of entries,
-- updated in the same database transaction that writes the entries.

CREATE TABLE accounts (
    id          UUID PRIMARY KEY,
    owner       TEXT        NOT NULL,
    asset       TEXT        NOT NULL,
    kind        TEXT        NOT NULL DEFAULT 'user'
                CHECK (kind IN ('user', 'external', 'hold')),
    balance     BIGINT      NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Only the external account (the outside world) may go negative.
    CONSTRAINT balance_non_negative CHECK (kind = 'external' OR balance >= 0)
);

-- One external and one hold account per asset.
CREATE UNIQUE INDEX accounts_one_system_per_asset
    ON accounts (asset, kind) WHERE kind <> 'user';

CREATE TABLE transactions (
    id               UUID PRIMARY KEY,
    kind             TEXT        NOT NULL,
    idempotency_key  TEXT        NOT NULL UNIQUE,
    request_hash     TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE entries (
    id              BIGSERIAL PRIMARY KEY,
    transaction_id  UUID        NOT NULL REFERENCES transactions (id),
    account_id      UUID        NOT NULL REFERENCES accounts (id),
    amount          BIGINT      NOT NULL CHECK (amount <> 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX entries_account_time ON entries (account_id, created_at DESC);
CREATE INDEX entries_transaction ON entries (transaction_id);

-- Enforce the double entry invariant in the database itself, checked at
-- commit time so a transaction can insert its legs one by one.
CREATE FUNCTION check_transaction_balanced() RETURNS trigger AS $$
BEGIN
    IF (SELECT COALESCE(SUM(amount), 0)
          FROM entries
         WHERE transaction_id = NEW.transaction_id) <> 0 THEN
        RAISE EXCEPTION 'transaction % is not balanced', NEW.transaction_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transaction_balanced();
