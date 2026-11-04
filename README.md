# Pushover Notifications for Silo

A [Silo](https://github.com/Silo-Server/silo-server) plugin that sends notifications to [Pushover](https://pushover.net).

- **Every profile:** new episodes and request updates go to the profile's own Pushover account, with its own device, sound, priority, wording and quiet hours.
- **Admins (opt-in):** stream started, paused, resumed and stopped; playback failures; too many streams; bandwidth limits; new devices and IP addresses; failed scans and jobs; a server health check; and a weekly digest. There is also a Live streams page for stopping streams.

Runs on Linux (amd64, arm64) and macOS (arm64).

## Install

### From the catalog (recommended)

Silo then downloads updates for you.

1. In Silo, open **Admin > Plugins**, go to **Repositories** and click **Add repository**.
2. Name it `Pushover Notifications` and use this URL:

   ```
   https://github.com/joshtheblack/silo-pushover-notifications/releases/latest/download/catalog.json
   ```

3. Install **Pushover Notifications** from the plugin catalog.

The plugin's **Updates** setting on its Admin > Plugins page controls what happens when a new release comes out. **Auto** installs it; **Notify** shows that an update is available.

### From a file

Download the zip for your server's platform from the [latest release](https://github.com/joshtheblack/silo-pushover-notifications/releases/latest), then use **Admin > Plugins > Install from a file**. Silo doesn't update a plugin installed this way. Installing from the catalog later replaces it in place and keeps its settings.

## Set up

The plugin's setup steps are shown on its Admin > Plugins page in Silo. In short:

1. Optionally paste a shared Pushover application token into the plugin's settings.
2. Each profile opens **Apps > Pushover** and adds their Pushover user key.
3. Admins who want admin alerts paste a full-access Silo API key into the **Silo API keys** tab. The plugin uses it once to create a read-only key, then revokes it.

## API

The settings page is backed by a JSON API. See [API.md](API.md). The OpenAPI document is served at `api/openapi.json` under the plugin's page.

## Build

Requires Go (the version in `go.mod`).

```sh
go test ./...
go run ./scripts/package   # zips and bare binaries in dist/
```

## Releasing

There are two tracks: dev builds for testing, and stable releases for everyone.

### Dev builds

Every push to `dev` or a `dev/*` branch runs the Dev build workflow. It tests, builds version `<manifest version>-dev.<run number>` (for example `0.4.0-dev.12`) and replaces the `dev-build` pre-release on GitHub. A test server adds this catalog instead of the stable one:

```
https://github.com/joshtheblack/silo-pushover-notifications/releases/download/dev-build/catalog.json
```

Each push sorts above the last, and below the release it leads up to (`0.4.0-dev.12` < `0.4.0`). The dev catalog also lists the latest stable release, so a test server gets that too.

After a release, bump `version` in `manifest.json` on the dev branch (for example from `0.4.0` to `0.5.0`). The workflow refuses to build a version that is already released, because those dev builds would sort below it.

### Stable releases

1. Merge the dev branch into `main`. `manifest.json` holds the version being released, such as `0.4.0`.
2. On GitHub, open **Actions > Release > Run workflow** and run it on `main`. (Pushing a `v0.4.0` tag does the same.)
3. The workflow tests, builds and creates a **draft** release `v0.4.0` with the binaries, zips and `catalog.json`.
4. Check the draft and publish it. Publishing creates the tag, and the release becomes the latest one, which the stable catalog URL points to.

Silo checks plugin catalogs when it starts and then once a day (the **Check Plugin Updates** scheduled task). Run that task to pick up a release sooner.

## License

[MIT](LICENSE)
