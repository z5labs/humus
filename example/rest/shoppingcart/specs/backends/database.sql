-- Shopping Cart Database Schema

CREATE TABLE IF NOT EXISTS carts (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at  TIMESTAMP   NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMP   NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS cart_items (
    id          UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id     UUID            NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    product_id  VARCHAR(255)    NOT NULL,
    quantity    INTEGER         NOT NULL CHECK (quantity > 0),
    unit_price  DECIMAL(10, 2)  NOT NULL,
    created_at  TIMESTAMP       NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMP       NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cart_items_cart_id ON cart_items(cart_id);
