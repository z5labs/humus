CREATE TABLE IF NOT EXISTS subscriptions (
    account_id    TEXT     PRIMARY KEY,
    amount_cents  BIGINT   NOT NULL CHECK (amount_cents > 0),
    currency      CHAR(3)  NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    active        BOOLEAN  NOT NULL
);

CREATE TABLE IF NOT EXISTS invoices (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    TEXT         NOT NULL,
    period        DATE         NOT NULL CHECK (period = date_trunc('month', period)),
    amount_cents  BIGINT       NOT NULL CHECK (amount_cents > 0),
    currency      CHAR(3)      NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    state         TEXT         NOT NULL DEFAULT 'pending',
    charge_id     TEXT,
    decline_code  TEXT,
    answer        TEXT,
    issued_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    settled_at    TIMESTAMPTZ,
    UNIQUE (account_id, period),
    CHECK (
        (state = 'pending'  AND charge_id IS NULL     AND decline_code IS NULL     AND answer IS NULL     AND settled_at IS NULL) OR
        (state = 'charged'  AND charge_id IS NOT NULL AND decline_code IS NULL     AND answer IS NULL     AND settled_at IS NOT NULL) OR
        (state = 'declined' AND charge_id IS NULL     AND decline_code IS NOT NULL AND answer IS NULL     AND settled_at IS NOT NULL) OR
        (state = 'failed'   AND charge_id IS NULL     AND decline_code IS NULL     AND answer IS NOT NULL AND settled_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS invoices_pending ON invoices (period, issued_at, account_id) WHERE state = 'pending';
