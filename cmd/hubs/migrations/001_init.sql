-- A hub's id is derived from its Ed25519 public key (see hubID in main.go), so
-- nobody can register a hub id they don't hold the key for.
create table hubs.hubs (
    id         text primary key,
    name       text not null default 'Home',
    public_key bytea not null unique,
    lan_url    text,
    public_url text,
    version    text,
    last_seen  timestamptz,
    created_at timestamptz not null default now()
);

-- user_id/email/name come from the auth service's access token.
create table hubs.members (
    hub_id   text not null references hubs.hubs on delete cascade,
    user_id  uuid not null,
    email    text not null,
    name     text not null default '',
    role     text not null check (role in ('owner', 'member')),
    added_at timestamptz not null default now(),
    primary key (hub_id, user_id)
);
create unique index members_one_owner on hubs.members (hub_id) where role = 'owner';
create index on hubs.members (user_id);

-- Pairing codes (owner, shown by an unclaimed hub) and invite codes (member).
create table hubs.codes (
    code       text primary key,
    hub_id     text not null references hubs.hubs on delete cascade,
    role       text not null check (role in ('owner', 'member')),
    created_by uuid,
    expires_at timestamptz not null
);
create index on hubs.codes (hub_id);
