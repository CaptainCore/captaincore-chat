# CaptainCore Chat protocol

How the CaptainCore Manager and browsers talk to `captaincore-chat`. Version 1.

## Identity tokens

The Manager is the only identity provider. A logged-in user's browser asks the Manager for a token, then presents it to the chat service. The chat service holds only the Manager's **public** key(s), so it can check tokens but never create them.

### Format

```
v1.<payload>.<signature>
```

- `payload` = base64url (no padding) of the JSON claims.
- `signature` = base64url (no padding) of an Ed25519 signature over the ASCII string `captaincore-chat.v1.` followed by `payload`.

There is one algorithm and no header, so there's nothing to downgrade. The `captaincore-chat.v1.` prefix means a signature made for anything else never verifies here.

### Claims

| Claim | Type | Meaning |
|---|---|---|
| `iss` | string | The Manager's home URL, exactly as in the chat config's `issuer` (e.g. `https://anchor.host`). |
| `aud` | string | The chat service URL, exactly as in the config's `audience` (e.g. `https://chat.example.com`). |
| `sub` | int | WordPress user ID on the Manager. |
| `name`, `email` | string | Display name and email, snapshotted onto conversations. |
| `role` | string | `customer`, or `operator` for Manager administrators. |
| `accounts` | int[] | CaptainCore account IDs the user belongs to. A conversation can only be filed under one of these. |
| `sites` | int[] | Site IDs the user can access. A conversation can only be linked to one of these. Operators may send `[]`. |
| `iat`, `exp` | int | Unix seconds. `exp - iat` must be 15 minutes or less. 60 seconds of clock skew is tolerated. |
| `jti` | string | Random ID (logging and debugging). |

### Signing in PHP (the Manager side)

```php
$b64url  = fn( $s ) => rtrim( strtr( base64_encode( $s ), '+/', '-_' ), '=' );
$claims  = [
    'iss' => home_url(), 'aud' => $chat_url, 'sub' => $user->ID,
    'name' => $user->display_name, 'email' => $user->user_email,
    'role' => $is_admin ? 'operator' : 'customer',
    'accounts' => $account_ids, 'sites' => $site_ids,
    'iat' => time(), 'exp' => time() + 600, 'jti' => bin2hex( random_bytes( 8 ) ),
];
$payload = $b64url( wp_json_encode( $claims, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE ) );
$sig     = sodium_crypto_sign_detached( 'captaincore-chat.v1.' . $payload, $secret_key ); // 64-byte sodium secret key
$token   = 'v1.' . $payload . '.' . $b64url( $sig );
```

Generate the keypair once with `sodium_crypto_sign_keypair()`. Keep the secret key out of anything the frontend config can read (the same treatment as `intercom_secret_key`). Put `base64_encode( sodium_crypto_sign_publickey( $kp ) )` in the chat config's `public_keys`.

`internal/auth/token_test.go` (`TestPHPSodiumSigner`) runs this exact signing code through `php` and verifies the result in Go.

**Key rotation:** add the new public key to `public_keys` (both listed), switch the Manager to the new secret key, wait 15 minutes, then remove the old public key.

## REST API

Base: the service's public URL. Every `/v1` request needs `Authorization: Bearer <token>`. Tokens never go in query strings. Bodies are JSON, a maximum of 64 KB, and unknown fields are rejected. Errors look like `{"error":{"code":"…","message":"…"}}`.

CORS allows only the configured `allowed_origins` (by default the issuer's origin). There is no cookie auth, so there is no `Allow-Credentials` and no CSRF surface.

| Method and path | Who | What |
|---|---|---|
| `GET /healthz` | anyone | `{"status":"ok","version":"…"}`; 503 if the database is unreachable. |
| `GET /v1/me` | any | The user, `operators_online` and the token's `expires`. |
| `GET /v1/conversations?status=&before=&limit=` | any | Newest activity first. `status` is `open`, `snoozed`, `closed`, `waiting`, or empty for all. Customers only ever get their own conversations. Operators may add `customer=<user id>`. Each row carries `preview` (the last comment) and `unread` for the viewer. Page with `before=<last_message_at of the last row>`. |
| `POST /v1/conversations` | customer | `{"body", "subject"?, "site_id"?, "account_id"?}`. The site and account must be in the token. The subject defaults to the first line of the body. Returns 201 `{conversation, messages}`. |
| `GET /v1/conversations/{id}?before=&limit=` | owner or operator | `{conversation, messages}`, oldest first. Page with `before=<message id>`. Operators also get `customer_last_read_message_id` (the "Seen" receipt) and internal messages. A customer who asks for someone else's conversation gets the same 404 as a missing one. |
| `POST /v1/conversations/{id}/messages` | owner or operator | `{"body", "kind"?}`. `kind` is `comment` (default), or `note` for operators only. A customer comment reopens a closed or snoozed conversation and starts the waiting clock. An operator comment clears it. Rate limit: 20 per minute per user. |
| `PATCH /v1/conversations/{id}` | owner or operator | Customers may only send `{"status":"closed"}` (resolve) or `{"status":"open"}` (reopen). Operators may also send `snoozed` (with `snoozed_until`, unix seconds within the next year), `site_id`, `account_id`, `assignee_user_id` (0 clears) and `subject`. Status, site and assignee changes add event messages. |
| `POST /v1/conversations/{id}/read` | owner or operator | `{"message_id"}` moves the user's read mark forward. It never moves back. |

### Messages are plain text

`body` is always plain text, never HTML. **Clients must escape it when rendering.** The server strips control characters and bidi overrides, and keeps everything else, including `<script>`, as inert text. Formatting such as links or a markdown subset is a client-side render of that text.

`internal: true` marks operator-only messages: every note, plus internal events such as snoozes and site links. The server never sends them to customers, over REST or websocket.

## Websocket

`GET /v1/ws`. The browser's `Origin` must be on the allowlist; otherwise the upgrade is refused with 403. A client that sends no `Origin` (not a browser) still needs a token.

1. The client sends `{"type":"auth","token":"v1…"}` within 10 seconds.
2. The server replies `{"type":"ready","user":{…},"operators_online":bool,"expires":unix}`.
3. Before `expires`, the client fetches a fresh token from the Manager and sends another `auth` frame. The server answers `{"type":"reauthed"}`. A different user or role closes the socket.

Close code **4001** means the token is missing, invalid or expired: fetch a new token and reconnect. The server pings every 25 seconds.

Client to server:

| Frame | |
|---|---|
| `{"type":"auth","token":"…"}` | First frame, and refreshes. |
| `{"type":"typing","conversation_id":N}` | Relayed to the other side. Limited to one every 2 seconds per conversation. |
| `{"type":"ping"}` | Answered with `{"type":"pong"}`. |

Server to client:

| Frame | Sent to |
|---|---|
| `conversation.created` (`conversation`, `message`) | Operators and the owner. |
| `conversation.updated` (`conversation`) | Operators and the owner. |
| `message.created` (`message`) | Operators. The owner too, unless `message.internal`. |
| `typing` (`conversation_id`, `user`: `{name, operator}`) | The other side of the conversation. |
| `read` (`conversation_id`, `message_id`) | Operators, when the customer reads. |
| `presence` (`operators_online`) | Customers, when the first operator connects or the last one leaves. |
