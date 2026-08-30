package allanime

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeTitle(t *testing.T) {
	tests := map[string]string{
		"Frieren: Beyond Journey's End": "frieren beyond journey s end",
		"Spy x Family 2nd Season":       "spy x family season 2",
		"SPY×FAMILY S2":                 "spy family season 2",
	}
	for input, want := range tests {
		if got := NormalizeTitle(input); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEpisodeValues(t *testing.T) {
	got, err := episodeValues(json.RawMessage(`{"sub":["2",1,"1.5"],"dub":[1]}`), "sub")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2", "1", "1.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("episodeValues() = %#v, want %#v", got, want)
	}
}

func TestDecryptEnvelopeAuthenticates(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{3}, aead.NonceSize())
	raw := append([]byte{1}, nonce...)
	raw = aead.Seal(raw, nonce, []byte(`{"episode":{"sourceUrls":[]}}`), nil)
	plain, err := decryptEnvelope(key, base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, []byte("sourceUrls")) {
		t.Fatalf("plain = %s", plain)
	}
	raw[len(raw)-1] ^= 1
	if _, err := decryptEnvelope(key, base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("decryptEnvelope() accepted a modified authentication tag")
	}
}

func TestClockStreamsSelectsQualityAndSubtitle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"links":[{"link":"https://video.test/480.m3u8","hls":true,"resolutionStr":"480p"},{"link":"https://video.test/1080.m3u8","hls":true,"resolutionStr":"1080p","headers":{"Referer":"https://embed.test/"},"subtitles":[{"src":"https://sub.test/en.ass","default":true}]}]}`))
	}))
	defer server.Close()
	c := NewClient(server.Client())
	streams, err := c.clockStreams(t.Context(), server.URL, "best")
	if err != nil {
		t.Fatal(err)
	}
	if streams[0].URL != "https://video.test/1080.m3u8" || streams[0].Subtitle != "https://sub.test/en.ass" {
		t.Fatalf("clockStreams() = %#v", streams)
	}
}

func TestDecodeClockURL(t *testing.T) {
	c := NewClientWithEndpoints(http.DefaultClient, "https://api.test/api", "https://web.test", "https://clock.test")
	plain := "/apivtwo/clock?id=abc"
	raw := []byte(plain)
	for i := range raw {
		raw[i] ^= 0x38
	}
	got, err := c.decodeClockURL("--" + hex.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://clock.test/apivtwo/clock.json?id=abc" {
		t.Fatalf("decodeClockURL() = %q", got)
	}
}

func TestCryptoStateDeduplicatesConcurrentRefresh(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	key := bytes.Repeat([]byte{9}, 32)
	fixture := strings.Replace(bundleProfileFixture, `"148"`, `"149"`, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	var bootstrapRequests atomic.Int32
	var bundleRequests atomic.Int32
	var refreshedProfile cryptoProfile
	var partB string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client-crypto/v1/bootstrap":
			bootstrapRequests.Add(1)
			if r.URL.Query().Get("buildId") == currentProfile.BuildID {
				http.Error(w, "rotated", http.StatusUnauthorized)
				return
			}
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch": 1, "partB": partB, "switchAt": now.Add(time.Hour).UnixMilli(),
			})
		case "/":
			bundleRequests.Add(1)
			_, _ = w.Write([]byte(`import("/_app/immutable/entry/app.test.js")`))
		case "/_app/immutable/entry/app.test.js":
			bundleRequests.Add(1)
			_, _ = w.Write([]byte(`const cryptoChunk="../chunks/crypto.js"`))
		case "/_app/immutable/chunks/crypto.js":
			bundleRequests.Add(1)
			_, _ = w.Write([]byte(`const aaReq=true;` + fixture))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	profiles := parseBundleProfiles(fixture, server.URL)
	if len(profiles) != 1 {
		t.Fatalf("parseBundleProfiles() returned %d profiles", len(profiles))
	}
	refreshedProfile = profiles[0]
	partB = bootstrapPartB(refreshedProfile, key)

	client := NewClientWithEndpoints(server.Client(), server.URL+"/api", server.URL, server.URL)
	client.now = func() time.Time { return now }
	type result struct {
		snapshot cryptoSnapshot
		err      error
	}
	owner := make(chan result, 1)
	go func() {
		snapshot, err := client.cryptoState(t.Context())
		owner <- result{snapshot, err}
	}()
	<-started

	const waiterCount = 8
	waiters := make(chan result, waiterCount)
	for range waiterCount {
		ctx := &signalingContext{Context: t.Context(), entered: make(chan struct{})}
		go func() {
			snapshot, err := client.cryptoState(ctx)
			waiters <- result{snapshot, err}
		}()
		<-ctx.entered
	}
	close(release)

	results := []result{<-owner}
	for range waiterCount {
		results = append(results, <-waiters)
	}
	if got := bootstrapRequests.Load(); got != 2 {
		t.Fatalf("bootstrap requests = %d, want 2", got)
	}
	if got := bundleRequests.Load(); got != 3 {
		t.Fatalf("bundle requests = %d, want 3", got)
	}
	for _, result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.snapshot.generation != 1 || result.snapshot.profile.BuildID != refreshedProfile.BuildID || !bytes.Equal(result.snapshot.key, key) {
			t.Fatalf("crypto snapshot = %#v", result.snapshot)
		}
	}
}

func TestCryptoStateRetriesAfterOwnerCancellation(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	key := bytes.Repeat([]byte{4}, 32)
	partB := bootstrapPartB(currentProfile, key)
	firstStarted := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client-crypto/v1/bootstrap" {
			http.NotFound(w, r)
			return
		}
		if requests.Add(1) == 1 {
			close(firstStarted)
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"epoch": 1, "partB": partB, "switchAt": now.Add(time.Hour).UnixMilli(),
		})
	}))
	defer server.Close()

	client := NewClientWithEndpoints(server.Client(), server.URL+"/api", server.URL, server.URL)
	client.now = func() time.Time { return now }
	ownerCtx, cancelOwner := context.WithCancel(t.Context())
	ownerResult := make(chan error, 1)
	go func() {
		_, err := client.cryptoState(ownerCtx)
		ownerResult <- err
	}()
	<-firstStarted

	waiterCtx := &signalingContext{Context: t.Context(), entered: make(chan struct{})}
	type result struct {
		snapshot cryptoSnapshot
		err      error
	}
	waiterResult := make(chan result, 1)
	go func() {
		snapshot, err := client.cryptoState(waiterCtx)
		waiterResult <- result{snapshot, err}
	}()
	<-waiterCtx.entered
	cancelOwner()

	if err := <-ownerResult; err == nil {
		t.Fatal("owner cryptoState() succeeded after cancellation")
	}
	waiter := <-waiterResult
	if waiter.err != nil {
		t.Fatal(waiter.err)
	}
	if !bytes.Equal(waiter.snapshot.key, key) || waiter.snapshot.generation != 1 {
		t.Fatalf("waiter snapshot = %#v", waiter.snapshot)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("bootstrap requests = %d, want 2", got)
	}
}

func TestStaleSnapshotCannotInvalidateCurrentKey(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := requests.Add(1)
		key := bytes.Repeat([]byte{byte(request)}, 32)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"epoch": 1, "partB": bootstrapPartB(currentProfile, key), "switchAt": now.Add(time.Hour).UnixMilli(),
		})
	}))
	defer server.Close()

	client := NewClientWithEndpoints(server.Client(), server.URL+"/api", server.URL, server.URL)
	client.now = func() time.Time { return now }
	old, err := client.cryptoState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	client.clearKey(old)
	current, err := client.cryptoState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	client.clearKey(old)
	got, err := client.cryptoState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.generation != current.generation || !bytes.Equal(got.key, current.key) {
		t.Fatalf("stale invalidation replaced current snapshot: got %#v, want %#v", got, current)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("bootstrap requests = %d, want 2", got)
	}
}

func TestStreamsUsesOneCryptoSnapshot(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	oldProfile := currentProfile
	oldProfile.BuildID = "old-build"
	oldProfile.Lane = "old-lane"
	oldKey := bytes.Repeat([]byte{3}, 32)
	newProfile := currentProfile
	newProfile.BuildID = "new-build"
	newProfile.Lane = "new-lane"
	newKey := bytes.Repeat([]byte{7}, 32)
	requestResult := make(chan error, 1)

	var client *Client
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client.mu.Lock()
		client.state = cryptoSnapshot{
			profile: newProfile, key: append([]byte(nil), newKey...), epoch: 2,
			keyExpiry: now.Add(time.Hour), generation: 2,
		}
		client.mu.Unlock()

		var payload struct {
			Extensions struct {
				Token string `json:"aaReq"`
				Lane  string `json:"k"`
			} `json:"extensions"`
		}
		err := json.NewDecoder(r.Body).Decode(&payload)
		if err == nil && r.Header.Get("x-build-id") != oldProfile.BuildID {
			err = fmt.Errorf("x-build-id = %q", r.Header.Get("x-build-id"))
		}
		if err == nil && payload.Extensions.Lane != oldProfile.Lane {
			err = fmt.Errorf("extension lane = %q", payload.Extensions.Lane)
		}
		var token struct {
			BuildID string `json:"buildId"`
			Lane    string `json:"k"`
		}
		if err == nil {
			plain, decryptErr := decryptEnvelope(oldKey, payload.Extensions.Token)
			if decryptErr != nil {
				err = decryptErr
			} else {
				err = json.Unmarshal(plain, &token)
			}
		}
		if err == nil && (token.BuildID != oldProfile.BuildID || token.Lane != oldProfile.Lane) {
			err = fmt.Errorf("token profile = %#v", token)
		}
		requestResult <- err

		plain := []byte(`{"episode":{"sourceUrls":[{"sourceUrl":"https://tools.fast4speed.rsvp/video.mp4","sourceName":"Yt-mp4"}]}}`)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"tobeparsed": encryptEnvelope(oldKey, plain)},
		})
	}))
	defer server.Close()

	client = NewClientWithEndpoints(server.Client(), server.URL, server.URL, server.URL)
	client.now = func() time.Time { return now }
	client.state = cryptoSnapshot{
		profile: oldProfile, key: append([]byte(nil), oldKey...), epoch: 1,
		keyExpiry: now.Add(time.Hour), generation: 1,
	}
	streams, err := client.Streams(t.Context(), Episode{ShowID: "show", Number: 1, Value: "1"}, "sub", "best")
	if requestErr := <-requestResult; requestErr != nil {
		t.Fatal(requestErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].URL != "https://tools.fast4speed.rsvp/video.mp4" {
		t.Fatalf("Streams() = %#v", streams)
	}
}

type signalingContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *signalingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func bootstrapPartB(profile cryptoProfile, key []byte) string {
	mask, _ := hex.DecodeString(profile.MaskHex)
	partB := make([]byte, len(mask))
	for index := range mask {
		partB[index] = mask[index] ^ key[index]
	}
	return base64.StdEncoding.EncodeToString(partB)
}

func encryptEnvelope(key, plain []byte) string {
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{5}, aead.NonceSize())
	raw := append([]byte{1}, nonce...)
	raw = aead.Seal(raw, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(raw)
}
