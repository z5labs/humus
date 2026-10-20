CREATE TABLE IF NOT EXISTS carts (
    id          UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    owner       TEXT            NOT NULL CHECK (owner <> ''),
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ     NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cart_items (
    id          UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id     UUID            NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    product_id  VARCHAR(255)    NOT NULL,
    quantity    INTEGER         NOT NULL CHECK (quantity > 0),
    unit_price  DECIMAL(10, 2)  NOT NULL CHECK (unit_price >= 0),
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ     NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_cart_items_cart_id ON cart_items(cart_id);
