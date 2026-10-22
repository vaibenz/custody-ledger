-- Four-eyes review. A held withdrawal settles only once it has approvals
-- from the required number of distinct operators. The requirement is copied
-- onto each withdrawal when it is requested, so tightening or loosening the
-- policy later does not change withdrawals already in the queue.
ALTER TABLE withdrawals
    ADD COLUMN approvals_required INT NOT NULL DEFAULT 1 CHECK (approvals_required >= 1);

CREATE TABLE withdrawal_approvals (
    withdrawal_id  UUID        NOT NULL REFERENCES withdrawals (id),
    reviewer       TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The same person can never count twice.
    PRIMARY KEY (withdrawal_id, reviewer)
);
