# Security

## How the hub keeps your home safe

- **No secrets in code.** Tokens, passwords and keys live only in `deploy/.env`
  (ignored by git) and in the `gateway-data` volume.
- **Every request is authenticated** with a bearer token: the local admin token
  (`HOMEAPP_TOKEN`, at least 32 characters, or the hub won't start) or a signed
  account token when account sign-in is enabled.
- **The message bus is private.** Mosquitto is not published outside the
  compose network.
- **Containers run as non-root** on distroless images, with no shell.
- **Requests arriving through a proxy or tunnel** are not treated as coming from
  your home network.

## Before exposing the hub to the internet

The hub listens on your local network only. To reach it from outside, prefer:

1. A VPN such as WireGuard or Tailscale (best: nothing is exposed), or
2. A tunnel with its own access control in front (for example Cloudflare
   Tunnel with Cloudflare Access).

Never forward port 8780 directly on your router.

## Checklist

- Generate tokens with `openssl rand -hex 32`; never reuse a password.
- Give Home Assistant a dedicated user and long-lived token for the hub.
- Keep the host, Docker and images up to date (`docker compose pull && docker compose up -d --build`).
- Back up the `gateway-data` volume; it holds the hub's identity key.

## Reporting a vulnerability

Please report privately through the repository's security advisories rather than
opening a public issue. We aim to reply within a week.
