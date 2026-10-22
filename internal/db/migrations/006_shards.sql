-- Shard the external account. Every deposit and settled withdrawal for an
-- asset touches its external account, so with one row per asset that row's
-- lock serialises all of them. Spreading it over N rows lets N postings
-- proceed at once; the asset's external position is the sum of its shards.
ALTER TABLE accounts ADD COLUMN shard INT NOT NULL DEFAULT 0;

DROP INDEX accounts_one_system_per_asset;
CREATE UNIQUE INDEX accounts_system_shard
    ON accounts (asset, kind, shard) WHERE kind <> 'user';
