package anime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider"
	"tarragon-anime/internal/provider/animeflix"
	"tarragon-anime/internal/streamlog"
)

type raceTestProvider struct {
	stackTestProvider
	match    func(context.Context, int, []string, string) (provider.Anime, error)
	episodes func(context.Context, provider.Anime, string) ([]provider.Episode, error)
	streams  func(context.Context, provider.Episode, string, string) ([]provider.Stream, error)
}

func (p raceTestProvider) Match(ctx context.Context, id int, aliases []string, translation string) (provider.Anime, error) {
	if p.match != nil {
		return p.match(ctx, id, aliases, translation)
	}
	return p.stackTestProvider.Match(ctx, id, aliases, translation)
}

func (p raceTestProvider) Episodes(ctx context.Context, anime provider.Anime, translation string) ([]provider.Episode, error) {
	if p.episodes != nil {
		return p.episodes(ctx, anime, translation)
	}
	return p.stackTestProvider.Episodes(ctx, anime, translation)
}

func (p raceTestProvider) Streams(ctx context.Context, episode provider.Episode, translation, quality string) ([]provider.Stream, error) {
	if p.streams != nil {
		return p.streams(ctx, episode, translation, quality)
	}
	return p.stackTestProvider.Streams(ctx, episode, translation, quality)
}

func resolveRaceAsync(s *Service, ctx context.Context, episode Episode) <-chan nextResult {
	done := make(chan nextResult, 1)
	go func() {
		stream, resolved, err := s.resolveStream(ctx, episode)
		done <- nextResult{stream: stream, episode: resolved, err: err}
	}()
	return done
}

func awaitRace[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider race")
		var zero T
		return zero
	}
}

func TestProviderRaceQuickLaterWinnerDoesNotWaitForLoser(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	releaseLoser := sync.OnceFunc(func() { close(release) })
	ready := make(chan struct{})
	providers := map[string]provider.Client{
		"slow": raceTestProvider{streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
			started <- ctx
			close(ready)
			<-release // Deliberately ignore cancellation until test cleanup.
			return []provider.Stream{{URL: "https://video.test/slow"}}, nil
		}},
		"quick": raceTestProvider{streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
			select {
			case <-ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []provider.Stream{{URL: "https://video.test/quick", Source: "quick_source"}}, nil
		}},
	}
	config := DefaultConfig()
	config.Providers = []string{"slow", "quick"}
	metadata := &cachingAniList{}
	var logs bytes.Buffer
	service := NewServiceWithProviders(metadata, providers, nil, nil, nil, nil, config, log.New(&logs, "", 0))
	t.Cleanup(func() { releaseLoser(); service.Close() })
	result := awaitRace(t, resolveRaceAsync(service, t.Context(), Episode{MediaID: 154587, Number: 1}))
	if result.err != nil || result.episode.Provider != "quick" || result.stream.URL != "https://video.test/quick" {
		t.Fatalf("resolution = %#v", result)
	}
	if metadata.getCalls != 1 {
		t.Fatalf("AniList lookups = %d, want one preloaded lookup", metadata.getCalls)
	}
	if cause := context.Cause(awaitRace(t, started)); !errors.Is(cause, streamlog.ErrWinnerSelected) {
		t.Fatalf("loser cancellation = %v", cause)
	}
	releaseLoser()
	service.Close() // Join deferred terminal logging before reading bytes.Buffer.
	for _, name := range config.Providers {
		starts, ends := 0, 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if !strings.Contains(line, "stream_attempt ") || !strings.Contains(line, "provider="+name+" source=provider stage=resolve ") {
				continue
			}
			if strings.Contains(line, "status=started ") {
				starts++
			} else {
				ends++
				want := "status=resolved reason=-"
				if name == "slow" {
					want = "status=cancelled reason=winner_selected"
				}
				if !strings.Contains(line, want) {
					t.Errorf("terminal log = %q, want %q", line, want)
				}
			}
		}
		if starts != 1 || ends != 1 {
			t.Errorf("%s starts=%d ends=%d; logs:\n%s", name, starts, ends, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "provider=quick source=quick_source stage=select") {
		t.Fatalf("selected source missing: %s", logs.String())
	}
}

func TestProviderRaceParentCancellation(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("before=%t", before), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			releaseWorker := sync.OnceFunc(func() { close(release) })
			client := raceTestProvider{streams: func(context.Context, provider.Episode, string, string) ([]provider.Stream, error) {
				close(started)
				<-release
				return []provider.Stream{{URL: "https://video.test/late"}}, nil
			}}
			metadata := &cachingAniList{}
			var logs bytes.Buffer
			service := NewService(metadata, client, nil, nil, nil, nil, DefaultConfig(), log.New(&logs, "", 0))
			t.Cleanup(func() { releaseWorker(); service.Close() })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if before {
				cancel()
			}
			done := resolveRaceAsync(service, ctx, Episode{MediaID: 154587, Number: 1})
			if !before {
				awaitRace(t, started)
				cancel()
			}
			if result := awaitRace(t, done); !errors.Is(result.err, context.Canceled) || result.stream.URL != "" {
				t.Fatalf("canceled resolution = %#v", result)
			}
			releaseWorker()
			service.Close()
			if before {
				if metadata.getCalls != 0 || logs.Len() != 0 {
					t.Fatalf("already canceled request made lookups: calls=%d logs=%s", metadata.getCalls, logs.String())
				}
			} else if strings.Count(logs.String(), "status=started") != 1 || strings.Count(logs.String(), "status=cancelled reason=request_cancelled") != 1 {
				t.Fatalf("cancellation logs = %s", logs.String())
			}
		})
	}
}

func TestProviderRaceGlobalLimitIncludesPrefetch(t *testing.T) {
	var current, peak atomic.Int32
	entered := make(chan int, 18)
	release := make(chan struct{})
	releaseWorkers := sync.OnceFunc(func() { close(release) })
	client := raceTestProvider{match: func(ctx context.Context, id int, _ []string, _ string) (provider.Anime, error) {
		count := current.Add(1)
		defer current.Add(-1)
		for previous := peak.Load(); count > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, count) {
				break
			}
		}
		entered <- id
		select {
		case <-release:
		case <-ctx.Done():
			return provider.Anime{}, ctx.Err()
		}
		return provider.Anime{}, errors.New("unavailable")
	}}
	config := DefaultConfig()
	config.Providers = []string{"a", "b", "c", "d", "e", "f"}
	providers := make(map[string]provider.Client)
	for _, name := range config.Providers {
		providers[name] = client
	}
	service := NewServiceWithProviders(nil, providers, nil, nil, nil, nil, config, nil)
	t.Cleanup(func() { releaseWorkers(); service.Close() })
	for id := 1; id <= 3; id++ {
		service.media[id] = anilist.Media{ID: id, Title: "Cached"}
	}
	first := resolveRaceAsync(service, t.Context(), Episode{MediaID: 1, Number: 1})
	seen := make(map[int]int)
	for range 4 {
		seen[awaitRace(t, entered)]++
	}
	second := resolveRaceAsync(service, t.Context(), Episode{MediaID: 2, Number: 1})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	active := &activePlayback{
		ctx: ctx, cancel: cancel, session: newFakeSession(),
		episodes:    []Episode{{MediaID: 3, Number: 1}, {MediaID: 3, Number: 2}},
		nextResults: make(chan nextResult, 1),
	}
	service.startNextPrefetch(active)
	select {
	case id := <-entered:
		t.Fatalf("lookup for media %d exceeded four occupied slots", id)
	case <-time.After(50 * time.Millisecond):
	}
	releaseWorkers()
	for range 14 {
		seen[awaitRace(t, entered)]++
	}
	for _, done := range []<-chan nextResult{first, second, active.nextResults} {
		if result := awaitRace(t, done); result.err == nil {
			t.Fatal("expected all providers to fail")
		}
	}
	service.Close()
	if peak.Load() != 4 || current.Load() != 0 || !reflect.DeepEqual(seen, map[int]int{1: 6, 2: 6, 3: 6}) {
		t.Fatalf("peak=%d current=%d calls=%v", peak.Load(), current.Load(), seen)
	}
}

func TestProviderRaceSimultaneousSuccesses(t *testing.T) {
	for iteration := range 10 {
		t.Run(fmt.Sprint(iteration), func(t *testing.T) {
			type attempt struct {
				name string
				ctx  context.Context
			}
			ready := make(chan attempt, 2)
			release := make(chan struct{})
			releaseWorkers := sync.OnceFunc(func() { close(release) })
			providers := make(map[string]provider.Client)
			for _, name := range []string{"one", "two"} {
				providers[name] = raceTestProvider{streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
					ready <- attempt{name, ctx}
					<-release
					return []provider.Stream{{URL: "https://video.test/" + name}}, nil
				}}
			}
			config := DefaultConfig()
			config.Providers = []string{"one", "two"}
			service := NewServiceWithProviders(&cachingAniList{}, providers, nil, nil, nil, nil, config, nil)
			t.Cleanup(func() { releaseWorkers(); service.Close() })
			done := resolveRaceAsync(service, t.Context(), Episode{MediaID: 154587, Number: 1})
			attempts := []attempt{awaitRace(t, ready), awaitRace(t, ready)}
			releaseWorkers()
			result := awaitRace(t, done)
			if result.err != nil || result.stream.URL != "https://video.test/"+result.episode.Provider {
				t.Fatalf("simultaneous resolution = %#v", result)
			}
			for _, attempt := range attempts {
				cause := context.Cause(attempt.ctx)
				if attempt.name == result.episode.Provider {
					if !errors.Is(cause, context.Canceled) {
						t.Fatalf("winner lookup was not released: %v", cause)
					}
				} else if !errors.Is(cause, streamlog.ErrWinnerSelected) {
					t.Fatalf("loser cause = %v", cause)
				}
			}
			service.Close()
		})
	}
}

func TestProviderRaceFailuresKeepConfiguredOrder(t *testing.T) {
	ready := make(chan string, 3)
	returned := make(chan string, 3)
	gates := make(map[string]chan struct{})
	providers := make(map[string]provider.Client)
	config := DefaultConfig()
	config.Providers = []string{"first", "second", "third"}
	for _, name := range config.Providers {
		gate := make(chan struct{})
		gates[name] = gate
		providers[name] = raceTestProvider{streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
			ready <- name
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			returned <- name
			switch name {
			case "first":
				return nil, errors.New("secret upstream body https://video.test/?token=private")
			case "second":
				return nil, nil
			default:
				return []provider.Stream{{URL: ""}, {URL: "not a URL"}}, nil
			}
		}}
	}
	var logs bytes.Buffer
	service := NewServiceWithProviders(&cachingAniList{}, providers, nil, nil, nil, nil, config, log.New(&logs, "", 0))
	t.Cleanup(service.Close)
	done := resolveRaceAsync(service, t.Context(), Episode{MediaID: 154587, Number: 1})
	for range 3 {
		awaitRace(t, ready)
	}
	for _, name := range []string{"third", "second", "first"} {
		close(gates[name])
		if got := awaitRace(t, returned); got != name {
			t.Fatalf("completion = %s, want %s", got, name)
		}
	}
	result := awaitRace(t, done)
	want := "resolve episode 1 stream: first: streams_failed; second: no_stream; third: invalid_stream"
	if result.err == nil || result.err.Error() != want {
		t.Fatalf("failure = %v, want %q", result.err, want)
	}
	service.Close()
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "token=") || strings.Contains(logs.String(), "https://") {
		t.Fatalf("unsafe logs: %s", logs.String())
	}
}

func TestProviderRaceFailureReasons(t *testing.T) {
	for _, reason := range []string{"match_failed", "episodes_failed", "episode_unavailable", "provider_unavailable", "no_stream", "media_failed"} {
		t.Run(reason, func(t *testing.T) {
			client := raceTestProvider{}
			metadata := aniListClient(&cachingAniList{})
			upstream := errors.New("private upstream body")
			switch reason {
			case "match_failed":
				client.match = func(context.Context, int, []string, string) (provider.Anime, error) {
					return provider.Anime{}, upstream
				}
			case "episodes_failed":
				client.episodeErr = upstream
			case "episode_unavailable":
				client.numbers = []int{2}
			case "no_stream":
				client.streams = func(context.Context, provider.Episode, string, string) ([]provider.Stream, error) {
					return nil, nil
				}
			case "media_failed":
				metadata = offlineAniList{}
			}
			providers := map[string]provider.Client{"allanime": client}
			if reason == "provider_unavailable" {
				delete(providers, "allanime")
			}
			var logs bytes.Buffer
			service := NewServiceWithProviders(metadata, providers, nil, nil, nil, nil, DefaultConfig(), log.New(&logs, "", 0))
			t.Cleanup(service.Close)
			_, _, err := service.resolveStream(t.Context(), Episode{MediaID: 154587, Number: 1})
			service.Close()
			if err == nil || err.Error() != "resolve episode 1 stream: allanime: "+reason {
				t.Fatalf("failure = %v", err)
			}
			status := "failed"
			if reason == "episode_unavailable" || reason == "provider_unavailable" || reason == "no_stream" {
				status = "unavailable"
			}
			terminal := "source=provider stage=resolve status=" + status + " reason=" + reason + " "
			if strings.Count(logs.String(), "status=started ") != 1 || strings.Count(logs.String(), terminal) != 1 || strings.Contains(logs.String(), "private") {
				t.Fatalf("logs = %s", logs.String())
			}
		})
	}
}

func TestProviderRaceSkipsInvalidStreams(t *testing.T) {
	for _, invalid := range []string{"", "not a URL", "/relative.m3u8", "https://", "file:///tmp/video", "https://video.test/a b", "https://video.test/%zz", "https://video.test:bad/stream"} {
		t.Run(invalid, func(t *testing.T) {
			client := raceTestProvider{streams: func(context.Context, provider.Episode, string, string) ([]provider.Stream, error) {
				return []provider.Stream{{URL: invalid}, {URL: "https://video.test/valid"}}, nil
			}}
			service := NewService(&cachingAniList{}, client, nil, nil, nil, nil, DefaultConfig(), nil)
			t.Cleanup(service.Close)
			stream, _, err := service.resolveStream(t.Context(), Episode{MediaID: 154587, Number: 1})
			if err != nil || stream.URL != "https://video.test/valid" {
				t.Fatalf("stream = %#v, %v", stream, err)
			}
		})
	}
}

func TestProviderRaceWinnerResourcesAndMetadataSurviveLookupCleanup(t *testing.T) {
	for _, prefetch := range []bool{false, true} {
		t.Run(fmt.Sprintf("prefetch=%t", prefetch), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "stream contents")
			}))
			t.Cleanup(server.Close)
			loserStarted := make(chan struct{})
			winnerCtx := make(chan context.Context, 1)
			wantStream := provider.Stream{URL: server.URL, Headers: map[string]string{"Authorization": "private-token"}, Subtitle: server.URL + "/sub?token=private", Source: "local_proxy"}
			winner := raceTestProvider{
				match: func(_ context.Context, id int, aliases []string, translation string) (provider.Anime, error) {
					if id != 154587 || translation != "dub" || aliases[0] != "Frieren" {
						t.Errorf("match arguments = %d, %v, %s", id, aliases, translation)
					}
					return provider.Anime{ID: "winner-catalogue"}, nil
				},
				episodes: func(_ context.Context, anime provider.Anime, translation string) ([]provider.Episode, error) {
					if anime.ID != "winner-catalogue" || translation != "dub" {
						t.Errorf("episodes arguments = %#v, %s", anime, translation)
					}
					return []provider.Episode{{ShowID: "playback-show", Number: 1, Value: "opaque-value"}}, nil
				},
				streams: func(ctx context.Context, episode provider.Episode, translation, quality string) ([]provider.Stream, error) {
					if episode != (provider.Episode{ShowID: "playback-show", Number: 1, Value: "opaque-value"}) || translation != "dub" || quality != "worst" {
						t.Errorf("streams arguments = %#v, %s, %s", episode, translation, quality)
					}
					select {
					case <-loserStarted:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					winnerCtx <- ctx
					return []provider.Stream{wantStream}, nil
				},
			}
			loser := raceTestProvider{streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
				close(loserStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			config := DefaultConfig()
			config.Providers = []string{"loser", "winner"}
			config.Translation, config.PreferredQuality = "dub", "worst"
			state := newMemoryState()
			_ = state.SaveProviderMapping(t.Context(), 154587, "loser", "loser-catalogue")
			var logs bytes.Buffer
			service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{"loser": loser, "winner": winner}, nil, state, nil, nil, config, log.New(&logs, "", 0))
			t.Cleanup(service.Close)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			episode := Episode{MediaID: 154587, Number: 1}
			var result nextResult
			if prefetch {
				active := &activePlayback{ctx: ctx, cancel: cancel, session: newFakeSession(), episodes: []Episode{{Number: 0}, episode}, nextResults: make(chan nextResult, 1)}
				service.startNextPrefetch(active)
				prefetchCanceled := false
				cancelPrefetch := active.nextPrefetch.cancel
				active.nextPrefetch.cancel = func() { cancelPrefetch(); prefetchCanceled = true }
				result = awaitRace(t, active.nextResults)
				service.handleNextResult(active, result)
				if active.nextReady == nil || !prefetchCanceled {
					t.Fatalf("prefetch result not retained or context not released: %v", result.err)
				}
			} else {
				result = awaitRace(t, resolveRaceAsync(service, ctx, episode))
			}
			wantEpisode := Episode{MediaID: 154587, Number: 1, Title: "Frieren - Episode 1", Provider: "winner", ProviderID: "playback-show", Value: "opaque-value"}
			if result.err != nil || result.episode != wantEpisode || !reflect.DeepEqual(result.stream, mpv.Stream{URL: wantStream.URL, Headers: wantStream.Headers, Subtitle: wantStream.Subtitle}) {
				t.Fatalf("resolved metadata = %#v", result)
			}
			if err := awaitRace(t, winnerCtx).Err(); !errors.Is(err, context.Canceled) {
				t.Fatalf("winner lookup context not released: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatalf("lookup cleanup canceled the request: %v", ctx.Err())
			}
			response, err := server.Client().Get(result.stream.URL)
			if err != nil {
				t.Fatalf("winning local server unusable: %v", err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != "stream contents" {
				t.Fatalf("winning local stream = %q, %v", body, err)
			}
			cancel()
			service.Close()
			if strings.Count(logs.String(), "stream_selected ") != 1 || !strings.Contains(logs.String(), "provider=winner source=local_proxy stage=select") || strings.Contains(logs.String(), "private") || strings.Contains(logs.String(), server.URL) {
				t.Fatalf("selection logs = %s", logs.String())
			}
		})
	}
}

type raceHTTPTransport func(*http.Request) (*http.Response, error)

func (f raceHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderRaceAnimeFlixAssetsSurviveLookupCleanup(t *testing.T) {
	loserStarted := make(chan context.Context, 1)
	ready := make(chan struct{})
	winnerCtx := make(chan context.Context, 1)
	upstream := &http.Client{Transport: raceHTTPTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/server/episode":
			select {
			case <-ready:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			winnerCtx <- r.Context()
			body = `{"success":true,"data":{"sub":[{"url":"https://as-cdn1.top/video/0123456789abcdef0123456789abcdef"}]}}`
		case "/video/0123456789abcdef0123456789abcdef":
			body = `playerjsSubtitle="[English]/subtitle.jpg"`
		case "/player/index.php":
			body = `{"hls":true,"videoSource":"/master.m3u8"}`
		case "/master.m3u8":
			body = "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"j\",NAME=\"Japanese\",LANGUAGE=\"jpn\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:RESOLUTION=1280x720,AUDIO=\"j\"\nvideo.m3u8\n"
		case "/subtitle.jpg":
			body = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"
		default:
			return nil, fmt.Errorf("unexpected upstream request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	client := animeflix.NewClientWithEndpoints(upstream, "https://api.test", "https://origin.test")
	t.Cleanup(client.Close)
	loser := raceTestProvider{streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
		loserStarted <- ctx
		close(ready)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	config := DefaultConfig()
	config.Providers = []string{"slow", "animeflix"}
	var logs bytes.Buffer
	service := NewServiceWithProviders(&cachingAniList{}, map[string]provider.Client{"slow": loser, "animeflix": client}, nil, nil, nil, nil, config, log.New(&logs, "", 0))
	t.Cleanup(service.Close)
	result := awaitRace(t, resolveRaceAsync(service, t.Context(), Episode{MediaID: 154587, Number: 1, Provider: "animeflix", ProviderID: "show", Value: "episode"}))
	if result.err != nil || result.episode.Provider != "animeflix" {
		t.Fatalf("AnimeFlix resolution = %#v", result)
	}
	if cause := context.Cause(awaitRace(t, winnerCtx)); !errors.Is(cause, context.Canceled) {
		t.Fatalf("winning lookup was not released: %v", cause)
	}
	if cause := context.Cause(awaitRace(t, loserStarted)); !errors.Is(cause, streamlog.ErrWinnerSelected) {
		t.Fatalf("loser cancellation cause = %v", cause)
	}
	service.Close() // Neither lookup cleanup nor service shutdown owns the provider client.
	playbackHTTP := &http.Client{Timeout: time.Second}
	for _, asset := range []struct {
		url, want string
	}{
		{result.stream.URL, "#EXTM3U"},
		{result.stream.Subtitle, "WEBVTT"},
	} {
		if !strings.HasPrefix(asset.url, "http://127.0.0.1:") {
			t.Fatalf("expected a native local asset, got %q", asset.url)
		}
		response, err := playbackHTTP.Get(asset.url)
		if err != nil {
			t.Fatalf("client-owned asset unavailable: %v", err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), asset.want) {
			t.Fatalf("native asset = %q, status=%d, error=%v", body, response.StatusCode, err)
		}
	}
	if !strings.Contains(logs.String(), "provider=animeflix source=mystream-1 stage=select") {
		t.Fatalf("native selected source missing: %s", logs.String())
	}
}

func TestProviderRaceCloseCancelsAndJoinsWorkers(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	releaseWorker := sync.OnceFunc(func() { close(release) })
	client := raceTestProvider{streams: func(ctx context.Context, _ provider.Episode, _, _ string) ([]provider.Stream, error) {
		started <- ctx
		<-release
		return []provider.Stream{{URL: "https://video.test/late"}}, nil
	}}
	service := NewService(&cachingAniList{}, client, nil, nil, nil, nil, DefaultConfig(), nil)
	t.Cleanup(func() { releaseWorker(); service.Close() })
	done := resolveRaceAsync(service, context.Background(), Episode{MediaID: 154587, Number: 1})
	workerCtx := awaitRace(t, started)
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	awaitRace(t, workerCtx.Done())
	if result := awaitRace(t, done); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("root-canceled resolution = %#v", result)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before worker cleanup")
	default:
	}
	releaseWorker()
	awaitRace(t, closed)
	if _, _, err := service.resolveStream(context.Background(), Episode{MediaID: 154587, Number: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed service resolution = %v", err)
	}
}
