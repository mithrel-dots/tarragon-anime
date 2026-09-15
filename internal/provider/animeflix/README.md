# AnimeFlix / MyStream

The `animeflix` provider uses the AnimeSalt catalogue API and resolves its
MyStream sources over HTTP. It is opt-in through the existing `providers` list;
registration does not change the configured fallback order or enable it by default.

- Matching is exact after case/punctuation normalization, with paginated search.
- Episode IDs are preserved. Catalogues that reset numbering across seasons are
  rejected rather than flattening distinct AniList seasons into one episode list.
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
because their episode numbers restart. Provider-season-to-AniList mapping is not
inferred from titles or list positions.

Run mocked tests with `go test -race ./internal/provider/animeflix`.
Run network checks with `go test -tags live -v ./internal/provider/animeflix`.
Set `ANIME_LIVE_MEDIA=1` for a bounded headless mpv decode in sub and dub modes;
this downloads a small amount of media but opens no window or audio device.
