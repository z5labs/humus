CREATE TABLE polls (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    question   text        NOT NULL CHECK (question <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE options (
    id       bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    poll_id  uuid    NOT NULL REFERENCES polls (id),
    position integer NOT NULL,
    text     text    NOT NULL CHECK (text <> ''),
    UNIQUE (poll_id, id),
    UNIQUE (poll_id, position),
    UNIQUE (poll_id, text)
);

CREATE TABLE votes (
    poll_id   uuid        NOT NULL,
    option_id bigint      NOT NULL,
    voter     text        NOT NULL,
    voted_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT votes_one_per_voter PRIMARY KEY (poll_id, voter),
    CONSTRAINT votes_option_of_poll FOREIGN KEY (poll_id, option_id) REFERENCES options (poll_id, id)
);

CREATE INDEX votes_by_option ON votes (poll_id, option_id);
