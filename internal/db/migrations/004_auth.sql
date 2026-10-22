-- Customer API keys. Only a SHA 256 of each key is stored: keys are 256 bit
-- random values, so a fast hash is enough (no password stretching needed),
-- and a database leak does not leak usable keys.
CREATE TABLE api_keys (
    id          UUID        PRIMARY KEY,
    owner       TEXT        NOT NULL,
    key_hash    BYTEA       NOT NULL UNIQUE,
    prefix      TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX api_keys_owner ON api_keys (owner);

-- Named operators: staff who credit deposits and review withdrawals. Each
-- has their own token so every action is attributable to a person.
CREATE TABLE operators (
    name        TEXT        PRIMARY KEY,
    token_hash  BYTEA       NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ
);
