# gotify-phonelink

A [Gotify](https://gotify.net) plugin that delivers messages **addressed to a
phone number** to the Gotify user that number belongs to — a push service for
senders that only know phone numbers (Matrix identity servers like ma1sd,
monitoring like Zabbix, …), with **SMS failover** for messages nobody opens.

- Users link their number once. It comes from the OpenID Connect provider
  Gotify logs in with (scope `phone`, claim `phone_number`), so nobody can
  type in someone else's number. The provider account must be the Gotify
  account (`preferred_username` = Gotify user name).
- Optionally the number is confirmed with a code sent by SMS
  (`sms.verify_on_link`).
- A number can be linked to one account only.
- Links are rechecked with the refresh token (`oidc.recheck_interval`): a
  changed number, a disabled account or a revoked grant removes the link.
- Senders POST to one endpoint, each with its own token and rules. A message
  that is not opened (tapped) in time is also sent by SMS through
  [smstools-http-api](https://github.com/geosone/smstools-http-api).

## Install

Download the `.so` built for **your exact Gotify release and architecture**
from the releases (a Go plugin only loads into the server it was built for),
put it into Gotify's plugin directory (`GOTIFY_PLUGINSDIR`, default
`data/plugins`) and restart Gotify.

Builds exist for every Gotify release since v2.6.2 and every Linux
architecture Gotify ships (amd64, 386, arm-7, arm64, and riscv64 from Gotify
v2.9.1 on, the first built with a Go that supports plugins there). A daily job
adds builds for new Gotify releases to the latest plugin release.

Building yourself (needs Docker):

```sh
make build GOTIFY_VERSION=v3.1.1 ARCHS=linux-amd64
```

## Configure

An administrator enables the plugin and sets the configuration (Gotify web UI
→ Plugins → phonelink). Only the enabled administrator instance with the lowest
user ID that has `public_url` set is used.

```yaml
public_url: https://gotify.example.org
oidc:
  issuer: https://auth.example.org
  client_id: gotify-phonelink
  client_secret: ...
  scopes: [openid, profile, phone, offline_access]
  username_claim: preferred_username
  trust_unverified_phone: false   # accept phone_number_verified=false
  recheck_interval: 24h
sms:                              # smstools-http-api
  url: https://smsapi.example.org
  username: phonelink
  password: ...
  timeout: 30s
  verify_on_link: true
senders:
  - name: ma1sd
    token: ...                    # at least 16 characters
    failover: 2m                  # SMS if not opened within 2 minutes
    failover_max: 5m              # upper limit for a per-message failover
    sms: true
    sms_unlinked: true            # unlinked numbers get an SMS at once
    hide_content: true            # push shows only "tap to view"
  - name: zabbix
    token: ...
    failover: 20m
    failover_max: 1h
    sms: true
    sms_min_priority: 8           # only high-priority problems by SMS
```

The plugin page shows the redirect URI for the OIDC client and the send
endpoint. The OIDC client needs the scopes above, the authorization code and
refresh token grants, and a refresh token lifetime longer than
`recheck_interval` (otherwise rechecks fail and links are removed).

Example for Authelia:

```yaml
identity_providers:
  oidc:
    lifespans:
      custom:
        phonelink:
          refresh_token: 30d
    clients:
      - client_id: gotify-phonelink
        client_secret: '$pbkdf2-sha512$...'
        authorization_policy: two_factor
        lifespan: phonelink
        require_pkce: true
        pkce_challenge_method: S256
        consent_mode: pre-configured
        redirect_uris:
          - https://gotify.example.org/plugin/<id>/custom/<token>/oidc/callback
        scopes: [openid, profile, phone, offline_access]
        grant_types: [authorization_code, refresh_token]
        response_types: [code]
        token_endpoint_auth_method: client_secret_basic
```

Each user enables the plugin and follows *Link phone number* on the plugin
page. Disabling the plugin stops pushes to that account (senders with
`sms_unlinked` then use SMS).

## Send

```sh
curl -X POST https://gotify.example.org/plugin/<id>/custom/<token>/send \
  -H "Authorization: Bearer <sender token>" -H "Content-Type: application/json" \
  -d '{"to": "+436761234567", "title": "Matrix", "message": "Your code is 123456"}'
```

| field          | meaning                                                        |
|----------------|----------------------------------------------------------------|
| `to`           | phone number (E.164; `+`/`00` optional)                        |
| `user`         | Gotify user name, instead of `to`                              |
| `title`        | optional                                                       |
| `message`      | required                                                       |
| `priority`     | Gotify priority (default 5)                                    |
| `failover`     | seconds or a duration (`"2m"`, `0` = none), capped at `failover_max` |
| `hide_content` | push only "tap to view"                                        |

Answers: `200 {"status":"push","id":"…","failover_in":"2m0s"}`,
`200 {"status":"sms"}`, `404` recipient not linked, `409` number linked to
several accounts, `401` unknown token, `400` bad request, `502` SMS failed.

Pending failovers are kept in memory: a Gotify restart drops them.

## License

MIT
