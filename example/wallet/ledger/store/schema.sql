CREATE TABLE accounts (
    id                 text        PRIMARY KEY,
    balance_cents      bigint      NOT NULL DEFAULT 0,
    balance_changed_at timestamptz NOT NULL DEFAULT now(),
    CHECK (id = 'external' OR balance_cents >= 0)
);

INSERT INTO accounts (id) VALUES ('external');

CREATE TABLE transfers (
    id              text        PRIMARY KEY,
    from_account_id text        NOT NULL REFERENCES accounts (id) DEFERRABLE INITIALLY DEFERRED,
    to_account_id   text        NOT NULL REFERENCES accounts (id) DEFERRABLE INITIALLY DEFERRED,
    amount_cents    bigint      NOT NULL CHECK (amount_cents > 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (from_account_id <> to_account_id)
);
