CREATE TABLE IF NOT EXISTS products (
    id          VARCHAR(255)    PRIMARY KEY CHECK (id <> ''),
    unit_price  DECIMAL(10, 2)  NOT NULL CHECK (unit_price >= 0)
);
