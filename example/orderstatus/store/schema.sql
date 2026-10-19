CREATE TABLE order_events (
    source_partition integer     NOT NULL,
    source_offset    bigint      NOT NULL,
    order_id         text        NOT NULL,
    type             text        NOT NULL,
    at               timestamptz NOT NULL,
    PRIMARY KEY (source_partition, source_offset)
);

CREATE INDEX order_events_by_order ON order_events (order_id, at);

CREATE TABLE order_status (
    order_id text        PRIMARY KEY,
    status   text        NOT NULL,
    at       timestamptz NOT NULL
);
