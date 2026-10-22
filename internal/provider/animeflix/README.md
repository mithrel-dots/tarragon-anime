# AnimeFlix / MyStream

The `animeflix` provider uses the AnimeSalt catalogue API and resolves its
MyStream sources over HTTP. It is opt-in through the existing `providers` list;
registration does not change the configured fallback order or enable it by default.

The supported MyStream hosts are `as-cdn<number>.top` and `ravok.buzz` (the
current player host as of 2026-10-22). Naruto Shippuden episode 1 and Slime
season 4 episode 5 were verified with headless mpv in both sub and dub modes.

- Matching is exact after case/punctuation normalization, with paginated search.
- Episode IDs are preserved. Catalogues that reset numbering across seasons are
  rejected unless an individually verified AniList-to-provider season mapping
  exists. The selected season is retained in the persisted provider ID.
- `sub` selects Japanese audio and requires the player's full English subtitle
  track. `dub` selects English audio. API sub/dub buckets are not language evidence.
- `best`, `worst`, and numeric resolutions such as `720p` select a video variant;
  numeric preferences choose the highest available resolution not exceeding the
  preference, or the lowest resolution if none fits.
- A bounded localhost server serves a filtered master and normalized WebVTT.
  Audio and video remain linked in the master; media goes directly to the CDN.
  Player cookies stay within one resolution session and are not forwarded to mpv.
- Signed URLs are resolved afresh, not cached across playback attempts.
- Abyss is unsupported. A failure leaves the existing provider fallback active.

Verified catalogue examples include Naruto Shippuden and One Piece. Aggregated
catalogues such as Jujutsu Kaisen and Solo Leveling currently fail conservatively
because their episode numbers restart. Slime season 4 (AniList 182205 / MAL 59970)
has an explicit mapping to AnimeSalt's season 4 of
`that-time-i-got-reincarnated-as-a-slime`. It does not enable arbitrary season
selection: mappings are not inferred from titles or list positions. The mapping
was checked against the provider's four-season catalogue and separate AniList
season-4 record; catalogue title and episode-ID structure are revalidated on use.

Run mocked tests with `go test -race ./internal/provider/animeflix`.
Run network checks with `go test -tags live -v ./internal/provider/animeflix`.
Set `ANIME_LIVE_MEDIA=1` for a bounded headless mpv decode in sub and dub modes;
this downloads a small amount of media but opens no window or audio device.
