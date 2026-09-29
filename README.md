# Tarragon Anime

Anime playback for Tarragon with AniList metadata and mpv.

## Playback without Chromium

Set the following in `~/.config/tarragon/anime.toml` (or
`$XDG_CONFIG_HOME/tarragon/anime.toml`):

```toml
providers = ["anipub"]
preferred_quality = "best" # also "worst" or a resolution such as "720p"
translation = "sub"        # or "dub"

[browser]
enabled = false
```

AniPub resolves MegaPlay sources over HTTP using the AES parameters published
in the player's current JavaScript. It selects the requested quality and, for
subbed playback, the full English subtitle track. A bounded loopback proxy
serves playlists and relays media with the correct filenames and MIME types:
the upstream serves video segments with `.jpg`, `.html`, and similar extensions
that current FFmpeg rejects. mpv needs no special FFmpeg options.

The plugin must remain running while a proxied stream plays. Provider lookup
cancellation does not stop that stream. Closing the plugin stops its proxy.
Episode lists are cached for one minute to avoid repeated catalogue rate limits;
resolved streams are cached for five minutes and invalidated after playback errors.

The default provider is still `allanime`; use the configuration above to select
browser-free playback. `animeflix` and `anineko` also use HTTP-only resolution
and can be included in `providers` when their upstreams are available.
Configured providers race concurrently: adding `allanime` with browser support
enabled can start Chromium even when another provider wins quickly.

With `[browser] enabled = true`, AniPub uses the managed browser as a fallback
if its native adapter fails. `[browser] auto_clearance = false` prevents visible
bot-check windows while still permitting headless resolution. Setting
`enabled = false` also disables AnimePahe's separate challenge-solving browser.

## Provider verification

Live checks on **2026-09-29**, from the test machine's network:

| Provider | Result |
| --- | --- |
| AniPub | Native sub/dub playback verified for episode 1 of Slime season 4, Frieren, and Cowboy Bebop. Uncached source resolution approximately 0.4–0.7 seconds, excluding catalogue lookup and player startup. Browser fallback playback also verified. |
| AllAnime | Frieren and Steins;Gate media verified; Slime season 4 and Kaiju No. 8 season 2 decoded through mpv. Fresh-profile clearance worked; Cowboy Bebop episode 1 returned no media after source retries. |
| AnimeFlix | Metadata worked; tested MyStream hosts returned HTTP 522/523. |
| AnimePahe | Requests remained blocked by an upstream Cloudflare challenge. |
| AniNeko | Origin returned a database connection error instead of its catalogue. |

These are point-in-time checks. Live tests intentionally fail when an upstream
is down, challenged, rate-limited, or returns unplayable media.

AniPub's Slime season 4 catalogue currently repeats links to episodes 7–12 in
place of 13–18. Those missing episodes are unavailable through its episode list;
the explicit episode numbers take precedence over list position. In particular,
the default episode-18 provider-race test still needs a working alternate such
as AllAnime.

## Checks

```sh
make check
make test-race
```

Network and real-browser integration tests are opt-in:

```sh
make test-live
```

To verify AniPub without Chromium, including full-episode audio/video probing
and one decoded frame through the production mpv handoff:

```sh
ANIME_LIVE_MEDIA=1 go test -tags=live -count=1 -v \
  -run '^TestLiveNativeStreams$' ./internal/provider/anipub
```

That check requires `mpv` and `ffprobe`; it opens no player window or audio
device and writes no watch progress. Avoid repeatedly running the catalogue
checks back-to-back: AniPub enforces a low per-minute request limit.

`ANIME_LIVE_FRESH=1` gives AllAnime tests temporary browser profiles.
`ANIME_LIVE_MEDIA=1` also enables playback checks for the other providers.
`ANIME_LIVE_EPISODE=1` changes the provider-race check from Slime season 4
episode 18 to episode 1, which AniPub advertises for browser-free testing.
See `internal/provider/animeflix/README.md` for MyStream-specific constraints.
