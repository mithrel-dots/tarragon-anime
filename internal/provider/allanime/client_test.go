package allanime

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
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
