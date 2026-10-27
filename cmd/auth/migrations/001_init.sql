create table auth.users (
    id              uuid primary key default gen_random_uuid(),
    email           text not null unique,
    name            text not null default '',
    password_hash   text,
    apple_sub       text unique,
    webauthn_handle bytea not null unique,
    created_at      timestamptz not null default now()
);

-- Passkeys (WebAuthn credentials). credential holds the library's JSON.
create table auth.passkeys (
    id           bytea primary key,
    user_id      uuid not null references auth.users on delete cascade,
    credential   jsonb not null,
    created_at   timestamptz not null default now(),
    last_used_at timestamptz
);
create index on auth.passkeys (user_id);

-- Refresh tokens are stored as SHA-256 hashes and rotated on every use.
create table auth.refresh_tokens (
    token_hash bytea primary key,
    user_id    uuid not null references auth.users on delete cascade,
    expires_at timestamptz not null,
    created_at timestamptz not null default now()
);
create index on auth.refresh_tokens (user_id);
create index on auth.refresh_tokens (expires_at);

-- Short-lived WebAuthn challenges between begin and finish.
create table auth.webauthn_sessions (
    id         text primary key,
    user_id    uuid references auth.users on delete cascade,
    data       jsonb not null,
    expires_at timestamptz not null
);
