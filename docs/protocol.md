# CaptainCore Chat protocol

How the CaptainCore Manager and browsers talk to `captaincore-chat`, the live relay. Version 1.

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
| `accounts`, `sites` | int[] | Optional. The relay ignores them; the Manager checks links itself. |
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

## Conversations live in the Manager

The browser reads and writes conversations through the Manager's REST API (`/wp-json/captaincore/v1/chat/*`, cookie and nonce auth), and the Manager enforces every rule. This service stores nothing. It only carries live events to connected browsers.

### Messages are plain text

`body` is always plain text, never HTML. **Clients must escape it when rendering.** The Manager strips control characters and bidi overrides, and keeps everything else, including `<script>`, as inert text.

`internal: true` marks operator-only messages: every note, plus internal events such as snoozes and site links. They are never sent to customers.

## Publish (Manager to relay)

After each change the Manager calls:

```
POST /internal/publish
X-Chat-Timestamp: <unix seconds>
X-Chat-Signature: <base64url Ed25519 signature>

{"event":{"type":"message.created",...},"customer_user_id":42,"internal":false}
```

- The signature is over `captaincore-chat.publish.v1.` + timestamp + `.` + the exact body bytes, made with the same secret key that signs tokens. The prefix differs from the token prefix, so neither signature can stand in for the other.
- The timestamp must be within 60 seconds of the relay's clock.
- A signature is accepted once. A repeat within the window gets 409.
- The body is at most 512 KB. Unknown fields get 400.
- Routing: operators get every event. The customer gets it unless `internal` is true or `customer_user_id` is 0 (a guest conversation).
- The event types `typing`, `presence` and `ready` are reserved for the relay.
- The response is `{"delivered":N,"customer":M}`: the number of connections it was queued for, and how many of them belong to the customer. The Manager uses `customer` to decide how soon to email an unread reply.

The PHP signer:

```php
$body = wp_json_encode( [ 'event' => $event, 'customer_user_id' => $customer, 'internal' => $internal ], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE );
$ts   = (string) time();
$sig  = sodium_crypto_sign_detached( 'captaincore-chat.publish.v1.' . $ts . '.' . $body, $secret_key );
// headers: X-Chat-Timestamp: $ts, X-Chat-Signature: $b64url( $sig )
```

`internal/auth/token_test.go` (`TestPHPPublishSigner`) runs this exact signing code through `php` and verifies the result in Go.

`GET /healthz` returns `{"status":"ok","version":"…"}`.

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
| `{"type":"typing","conversation_id":N}` | Customers. Sent to operators with the sender's `user.id`. Operators ignore it unless that id owns the conversation. |
| `{"type":"typing","conversation_id":N,"customer_user_id":C}` | Operators. Sent to customer C and the other operators. Typing is limited to one frame every 2 seconds per conversation. |
| `{"type":"ping"}` | Answered with `{"type":"pong"}`. |

Server to client:

| Frame | Sent to |
|---|---|
| `conversation.created` (`conversation`, `message`) | Operators and the owner. |
| `conversation.updated` (`conversation`) | Operators and the owner. |
| `message.created` (`message`) | Operators. The owner too, unless `message.internal`. |
| `typing` (`conversation_id`, `user`: `{id, name, operator}`) | See above. |
| `read` (`conversation_id`, `message_id`) | Operators, when the customer reads. |
| `presence` (`operators_online`) | Customers, when the first operator connects or the last one leaves. |
