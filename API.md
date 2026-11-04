# Pushover plugin API

The settings page is a thin client over a JSON API that the plugin serves
through Silo. Anything the page can do, a script can do with the same calls.

A machine-readable OpenAPI 3.1 description of every endpoint is served at
`api/openapi.json` (for example
`https://<your-silo>/api/v2/plugin-content/plugins/9/api/openapi.json`, with
the same headers as below). Its server URL is relative (`..`), so tools that
load it from that address call the right base. Silo's own
`/api/v2/openapi.json` covers only Silo's routes, not plugins'.

## Base URL and sign-in

```
https://<your-silo>/api/v2/plugin-content/plugins/<installation_id>/
```

`<installation_id>` is the plugin's installation number in Silo
(Admin > Plugins, or `GET /api/v2/admin/plugins/installations`).

Every request needs:

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <full-access Silo API key>`. Silo only accepts **full-access** keys on plugin routes; scoped keys are refused. |
| `X-Profile-Id` | The profile to act as. It must belong to the key's account. Use the account's **primary** profile for admin routes (Silo treats only that profile as an admin). PIN locks are skipped for API keys. |
| `Content-Type` | `application/json` on every `POST` and `PUT`. Anything else gets `415`. |

Silo checks the key and profile and passes the plugin the account, role and
profile. The plugin never sees the key.

```bash
SILO=https://silo.example.com
INST=9
KEY=sa_...                     # full-access key; keep it out of your shell history
PROFILE=3338b374-...           # the account's primary profile ID
api() { curl -sS -H "Authorization: Bearer $KEY" -H "X-Profile-Id: $PROFILE" \
             -H "Content-Type: application/json" "$SILO/api/v2/plugin-content/plugins/$INST/$@"; }

api api/state
```

Profile IDs come from `GET /api/v2/admin/users/{account_id}/profiles` or
`GET /api/v2/profiles`.

## Errors

Errors are JSON with a stable `code` and a readable `message`:

```json
{ "code": "monitor_key_missing", "message": "..." }
```

| Status | Codes you may see |
| --- | --- |
| 400 | `bad_request` (body isn't valid JSON) |
| 401 | `unauthenticated` |
| 403 | `forbidden` (not an acting admin, or not your profile) |
| 404 | `not_found`, `session_ended` |
| 405 | `method_not_allowed` |
| 409 | `no_profile` (no `X-Profile-Id`), `monitor_key_missing`, `monitor_key_rejected`, `monitor_key_scope` (admin routes need the plugin's read-only Silo key), `busy` (a key setup is already running) |
| 415 | `unsupported_media_type` |
| 422 | `invalid` (a field failed validation; the message says which), `pushover_rejected` (Pushover refused the key pair), `pushover_not_set_up` |
| 502, 503 | `silo_unavailable`, `host_unavailable`, `create_failed`, `verify_failed` and other upstream failures |

## Profile notifications

Each profile has its own Pushover settings. A primary profile can manage every
profile on its account when the plugin has a read-only Silo key; otherwise a
profile manages only itself.

### `GET api/state`

The page's start-up call. Returns the notification types and their default
wording, Pushover's sounds, whether a shared app token exists, the profiles you
may manage (`id`, `name`, `active`, `configured`, `enabled`), `admin` (true for
an acting admin), and, for admins, `admin_keys` (which Silo keys are set). A
setup problem comes back as a `problem` object rather than an error.

### `GET api/profiles/{profile_id}`

One profile's settings. Keys are never returned, only hints
(`user_key_hint`, `app_token_hint`).

Fields: `enabled`, `device`, `sound`, `priority` (-2 to 1), `html`,
`attach_poster`, `include_link`, `types` (per type: `enabled`, `title`,
`message`, optional `sound`, `priority`, and for `episode.available`
`group_title`, `group_message`), `episode_reasons` (any of `favorite`,
`watchlist`, `continue_watching`, `next_up`; empty means all), `group_minutes`
(0 = off, 1 to 120), `quiet` (`enabled`, `start`, `end` as `HH:MM`,
`silence_emergency`), `time_zone` (IANA name). Admin alerts follow the quiet
hours of the admin's own profile, so `silence_emergency` (also turn emergency
admin alerts down) is kept only on that profile.

Notification types: `episode.available`, `request.approved`,
`request.declined`, `request.fulfilled`, `other`.

### `PUT api/profiles/{profile_id}`

Saves a profile. Send the whole settings object (the shape `GET` returns,
without the hints). Key handling:

- `user_key`: a new Pushover user or group key. Leave it out or empty to keep
  the saved one.
- `app_token`: the profile's own application token. Empty keeps the saved
  one; `"clear_app_token": true` removes it, so the server's shared token is
  used.

The plugin checks the key pair with Pushover before saving and answers `422`
if Pushover rejects it.

```bash
api api/profiles/$PROFILE | jq '.group_minutes = 10' > p.json
api api/profiles/$PROFILE -X PUT --data @p.json
```

### `DELETE api/profiles/{profile_id}`

Removes the profile's Pushover settings. Answers `{"ok": true}`.

### `POST api/profiles/{profile_id}/test`

Sends a sample to Pushover using unsaved values. Body:
`{"settings": {...}, "type": "episode.available", "group": false}`.
`group: true` sends the grouped season-drop wording. Counts against the
Pushover app's monthly limit.

### `POST api/profiles/{profile_id}/validate`

Checks a key pair with Pushover and lists the account's devices and sounds.
Body: `{"user_key": "...", "app_token": "...", "clear_app_token": false}`.
Empty values fall back to the saved ones. Sends nothing.

## Admin alerts

Only for an acting admin (the account's primary profile), and only while the
plugin has a working read-only Silo key; otherwise `409 monitor_key_*`.
Alerts go to the admin's own profile's Pushover settings.

### `GET api/admin/alerts`

Returns:

- `settings`: this admin's saved settings (defaults if none).
- `configured`: whether this admin has saved settings.
- `pushover_ready`: whether the admin's profile has Pushover set up.
- `quota`: `limit`, `remaining`, `reset` (Unix time), `low` for the app token
  in use, once a send has reported it.
- `alert_types`, `noise_modes`, `media_types`, `libraries`, `sounds`: what the
  page needs to draw its form.

`settings` fields:

| Field | Meaning |
| --- | --- |
| `alerts` | Per alert key: `enabled`, `title`, `message`, optional `sound`, `priority` (-2 to 2; 2 is emergency) |
| `filters` | `ignore_users` (account IDs), `only_remote`, `only_transcodes`, `media_types`, `libraries` (library IDs) |
| `noise` | `mode` (`every`, `cooldown`, `binge`) and `window_minutes` (1 to 1440) |
| `limits` | `user_streams`, `server_streams`, `bandwidth_mbps`, `bandwidth_remote_only`, `disk_pct`, `memory_pct` (0 = off for the last two) |
| `quiet` | Deprecated. Admin alerts follow the quiet hours of the admin's profile; this is used only while that profile has none, and the plugin moves it to the profile the next time you `GET` these settings |
| `time_zone` | IANA name for the digest schedule; empty on `PUT` keeps the saved one |
| `digest` | `day` (0 = Sunday) and `hour` (0 to 23), in `time_zone` |
| `attach_poster` | Posters on playback alerts |
| `silo_link` | "Open in Silo" link to `/admin/activity` on stream alerts |

Alert keys: `stream.started`, `stream.paused` (pause and resume),
`stream.stopped`, `playback.failed`, `streams.user`, `streams.server`,
`bandwidth`, `device.new`, `ip.new`, `scan.failed`, `job.failed`, `health`,
`health.recovered`, `digest.weekly`.

### `PUT api/admin/alerts`

Saves this admin's settings. Send the whole `settings` object; read it with
`GET` first and change what you need, since missing fields reset to defaults.

```bash
api api/admin/alerts | jq '.settings | .limits.user_streams = 2' > a.json
api api/admin/alerts -X PUT --data @a.json
```

### `DELETE api/admin/alerts`

Removes this admin's alert settings, which stops their admin alerts.

### `POST api/admin/alerts/test`

Sends a sample of one alert with unsaved values. Body:
`{"settings": {...}, "alert": "stream.started"}`. An emergency alert is sent
at priority 1, and quiet hours are ignored.

### `GET api/admin/users`

Accounts for the "users to ignore" filter: `id`, `name`.

## Live streams (stop-stream)

Admin only.

| Call | What it does |
| --- | --- |
| `GET api/admin/sessions` | Live streams, read with the plugin's read-only key |
| `GET api/admin/sessions/{session_id}` | One stream: `session`, `ended` (true once it's gone, with `session: null`), `control_key_set` |
| `POST api/admin/sessions/{session_id}/stop` | Stops it with the plugin's stream-control key (`404 session_ended` if it already ended). Body `{}` or `{"message": "..."}` (up to 1000 characters): with a message, the player shows it, then the stream stops about 2 seconds later. Answers `ok`, `level`, `title`, `details`. |

Opening or reading never stops anything.

## Silo key setup

### `POST api/admin/key-setup`

Creates the plugin's scoped Silo keys from a full-access key, saves them in
the plugin's settings, and can revoke the key you pasted. Admin only.

```json
{ "source": "paste", "key": "sa_...", "revoke": true, "control": false }
```

- `source`: `paste` (use `key`) or `legacy` (use the full-access key saved by
  version 0.2.0, which is always revoked afterwards).
- `revoke`: revoke the pasted key once the new key is saved. Only done when
  exactly one unscoped key in Silo matches its prefix.
- `control`: also create a key with `admin:sessions:control` for stopping
  streams.

The answer reports what was created, saved and revoked (`saved`, `level`,
`title`, `details`, `keys`). It never contains a key. This call changes keys
in Silo, so treat it like an admin action, not a read.

## Not in this API

Server-wide settings (the shared Pushover app token, public URL override and
the two Silo keys) are Silo plugin config: read them with
`GET /api/v2/admin/plugins/installations` (secrets show only as set or not)
and change them in Admin > Plugins or with
`PUT /api/v2/admin/plugins/installations/{id}/config`. Scheduled task
schedules use `PUT /api/v2/admin/plugins/installations/{id}/task-bindings/{capability_id}`.
