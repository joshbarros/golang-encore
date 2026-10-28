CREATE TABLE gigs.gigs (
    id                uuid PRIMARY KEY,
    code              text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    name              text        NOT NULL,
    stage            text        NOT NULL,
    band            text        NOT NULL,
    gig_date      timestamptz NOT NULL,
    price_cents       bigint      NOT NULL,
    seats_total       integer     NOT NULL,
    seats_left        integer     NOT NULL,
    max_per_fan integer     NOT NULL,
    created_at        timestamptz NOT NULL,
    CONSTRAINT gigs_remaining CHECK (seats_left >= 0 AND seats_left <= seats_total),
    CONSTRAINT gigs_cap CHECK (max_per_fan >= 1 AND max_per_fan <= seats_total)
);

-- How many seats one fan holds in one gig. Written only by the reserve and release handlers.
CREATE TABLE gigs.fans (
    gig_id  uuid    NOT NULL REFERENCES gigs.gigs (id),
    owner_id uuid    NOT NULL,
    seats    integer NOT NULL CHECK (seats >= 0),
    PRIMARY KEY (gig_id, owner_id)
);

CREATE TABLE gigs.seat_holds (
    order_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    gig_id        uuid        NOT NULL REFERENCES gigs.gigs (id),
    seats          integer     NOT NULL,
    subtotal_cents bigint      NOT NULL DEFAULT 0,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text
);

CREATE TABLE gigs.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX gigs_outbox_unsent ON gigs.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE gigs.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
