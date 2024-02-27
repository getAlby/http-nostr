CREATE TABLE subscriptions (
    id bigint NOT NULL,
    relay_url text,
    webhook_url text,
    ids_string text,
    kinds_string text,
    authors_string text,
    tags_string text,
    since timestamp with time zone,
    until timestamp with time zone,
    "limit" integer,
    search text,
    "open" boolean,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);

-- Create sequence for subscriptions
CREATE SEQUENCE subscriptions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE subscriptions_id_seq OWNED BY subscriptions.id;

ALTER TABLE ONLY subscriptions ALTER COLUMN id SET DEFAULT nextval('subscriptions_id_seq'::regclass);

ALTER TABLE ONLY subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);

-- Create request_events table
CREATE TABLE request_events (
    id bigint NOT NULL,
    subscription_id bigint,
    nostr_id text UNIQUE,
    content text,
    "state" text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);

-- Create sequence for request_events
CREATE SEQUENCE request_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE request_events_id_seq OWNED BY request_events.id;

ALTER TABLE ONLY request_events ALTER COLUMN id SET DEFAULT nextval('request_events_id_seq'::regclass);

ALTER TABLE ONLY request_events
    ADD CONSTRAINT request_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY request_events
    ADD CONSTRAINT fk_request_events_subscription FOREIGN KEY (subscription_id) REFERENCES subscriptions(id) ON DELETE CASCADE;

-- Create response_events table
CREATE TABLE response_events (
    id bigint NOT NULL,
    subscription_id bigint,
    request_id bigint NULL,
    nostr_id text,
    content text,
    replied_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);

-- Create sequence for response_events
CREATE SEQUENCE response_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE response_events_id_seq OWNED BY response_events.id;

ALTER TABLE ONLY response_events ALTER COLUMN id SET DEFAULT nextval('response_events_id_seq'::regclass);

ALTER TABLE ONLY response_events
    ADD CONSTRAINT response_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY response_events
    ADD CONSTRAINT fk_response_events_subscription FOREIGN KEY (subscription_id) REFERENCES subscriptions(id) ON DELETE CASCADE;

ALTER TABLE ONLY response_events
    ADD CONSTRAINT fk_response_events_request_event FOREIGN KEY (request_id) REFERENCES request_events(id) ON DELETE CASCADE;