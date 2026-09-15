package animeflix

import (
	"strings"
	"testing"
)

func TestFilterMasterChoosesQualityAndPreservesAudioAndSignedURLs(t *testing.T) {
	body := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="eng",NAME="English",LANGUAGE="eng",URI="audio/eng.m3u8?sig=a%2Cb"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="jpn",NAME="Japanese",LANGUAGE="jpn",URI="audio/jpn.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="jpn",NAME="Japanese",LANGUAGE="jpn",URI="audio/jpn.m3u8?sig=j%2Cb"
#EXT-X-STREAM-INF:CODECS="avc1.640028,mp4a.40.2",BANDWIDTH=700000,RESOLUTION=1280x720,AUDIO="eng",SUBTITLES="subs"
video/720.m3u8?token=720%2Cok
#EXT-X-STREAM-INF:BANDWIDTH=1500000,RESOLUTION=1920x1080,AUDIO="eng"
video/1080.m3u8?token=1080
#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360,AUDIO="eng"
video/360.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=600000,RESOLUTION=854x480,AUDIO="jpn"
video/jpn-480.m3u8
`
	for name, testCase := range map[string]struct{ quality, want string }{"best": {"best", "1080"}, "worst": {"worst", "360"}, "720p": {"720p", "720"}, "1080p": {"1080p", "1080"}} {
		got, err := filterMaster("https://as-cdn1.top/video/player?x=1", body, "eng", testCase.quality)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := string(got)
		if !strings.Contains(text, "video/"+testCase.want+".m3u8") || !strings.Contains(text, `GROUP-ID="eng"`) || strings.Contains(text, "SUBTITLES=") {
			t.Errorf("%s output = %s", name, text)
		}
		if !strings.Contains(text, "sig=a%2Cb") || (testCase.want == "720" && !strings.Contains(text, `CODECS="avc1.640028,mp4a.40.2"`)) {
			t.Errorf("%s lost quoted comma or signed query: %s", name, text)
		}
		for _, attribute := range []string{`AUTOSELECT=YES`, `DEFAULT=YES`, `GROUP-ID="eng"`, `LANGUAGE="eng"`, `NAME="English"`, `TYPE=AUDIO`} {
			if !strings.Contains(text, attribute) {
				t.Errorf("%s dropped %s: %s", name, attribute, text)
			}
		}
	}
	got, err := filterMaster("https://as-cdn1.top/path/master.m3u8", body, "jpn", "best")
	if err != nil || !strings.Contains(string(got), `GROUP-ID="jpn"`) {
		t.Fatalf("jpn filter = %s, %v", got, err)
	}
}

func TestFilterMasterRejectsMalformedUnsupportedOrUnverifiedPlaylists(t *testing.T) {
	tests := map[string]string{
		"not playlist": "hello",
		"session key":  "#EXTM3U\n#EXT-X-SESSION-KEY:METHOD=AES-128",
		"define":       "#EXTM3U\n#EXT-X-DEFINE:NAME=x,VALUE=y",
		"wrong group":  "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"x\",LANGUAGE=\"eng\",NAME=\"English\",URI=\"a\"\n#EXT-X-STREAM-INF:RESOLUTION=1280x720,AUDIO=\"missing\"\nv.m3u8",
		"video only":   "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\nv.m3u8",
		"bad scheme":   "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"e\",LANGUAGE=\"eng\",URI=\"javascript:a\"\n#EXT-X-STREAM-INF:RESOLUTION=1280x720,AUDIO=\"e\"\nv.m3u8",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := filterMaster("https://as-cdn1.top/master.m3u8", body, "eng", "best"); err == nil {
				t.Fatal("filterMaster accepted invalid playlist")
			}
		})
	}
}

func TestEnglishSubtitleRequiresFullEnglishAndResolvesRelativeURL(t *testing.T) {
	base := "https://as-cdn1.top/video/abc"
	for name, testCase := range map[string]struct {
		page string
		want string
	}{
		"english":   {`playerjsSubtitle="[Spanish]es.jpg,[English]subs/en.jpg?sig=a%2Cb"`, "sig=a%2Cb"},
		"case":      {`playerjsSubtitle="[eNgLiSh]subs.jpg"`, "/video/subs.jpg"},
		"lookalike": {`playerjsSubtitle="[English Commentary]commentary.jpg"`, ""},
		"missing":   {`<html>no subtitle variable</html>`, ""},
		"bad URL":   {`playerjsSubtitle="[English]javascript:alert(1)"`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := englishSubtitle(base, testCase.page)
			if testCase.want == "" && err == nil || testCase.want != "" && (err != nil || !strings.Contains(got, testCase.want)) {
				t.Fatalf("englishSubtitle() = %q, %v", got, err)
			}
		})
	}
	if _, err := englishSubtitle(base, `playerjsSubtitle="[English]https://evil.example/sub.jpg"`); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeSubtitleCentisecondsSRTWebVTTAndErrors(t *testing.T) {
	got, err := normalizeSubtitle("\ufeff1\r\n00:01:02,3 --> 00:01:04.12\r\nHello\r\n")
	// Numeric SRT indices are valid WebVTT cue identifiers.
	if err != nil || string(got) != "WEBVTT\n\n1\n00:01:02.300 --> 00:01:04.120\nHello\n" {
		t.Fatalf("SRT normalize = %q, %v", got, err)
	}
	webvtt, err := normalizeSubtitle("WEBVTT\n\n00:00.00 --> 00:01.001\nHi")
	if err != nil || !strings.HasPrefix(string(webvtt), "WEBVTT") || !strings.Contains(string(webvtt), "00:00:00.000 --> 00:00:01.001") {
		t.Fatalf("WebVTT normalize = %q, %v", webvtt, err)
	}
	for _, raw := range []string{"<html>bad</html>", "not cues", "00:61:00.00 --> 00:01:00.00\nno"} {
		if _, err := normalizeSubtitle(raw); err == nil {
			t.Errorf("normalizeSubtitle(%q) accepted invalid text", raw)
		}
	}
}
