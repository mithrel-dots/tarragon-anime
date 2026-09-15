package browser

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

func TestCollectorRequiresSuccessfulResponse(t *testing.T) {
	for _, status := range []int{0, 200, 206, 299, 300, 302, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := newCollector(testRequest())
			c.request(&proto.NetworkRequestWillBeSent{
				RequestID: "1", Request: &proto.NetworkRequest{URL: "https://cdn.test/a.mp4"},
			})
			if len(c.matches()) != 0 || !c.firstMatch.IsZero() {
				t.Fatal("request accepted before a response")
			}
			c.response(&proto.NetworkResponseReceived{
				RequestID: "1", Response: &proto.NetworkResponse{Status: status},
			})
			if got, want := len(c.matches()) == 1, status >= 200 && status < 300; got != want {
				t.Fatalf("accepted = %t, want %t", got, want)
			}
		})
	}
}

// settled never fires while nothing has matched, so a page that loads and then
// requests no media has no way to end a capture except the deadline. The load
// event plus a grace period is what makes that answer both reachable and fast.
func TestCollectorGivesUpAfterLoadWithoutMedia(t *testing.T) {
	c := newCollector(Request{
		PageURL: "https://example.test/watch",
		Accept:  func(Candidate) bool { return false },
		Settle:  time.Hour,
	})
	c.request(&proto.NetworkRequestWillBeSent{
		RequestID: "doc", Type: proto.NetworkResourceTypeDocument,
		Request: &proto.NetworkRequest{URL: "https://example.test/watch"},
	})
	c.response(&proto.NetworkResponseReceived{
		RequestID: "doc", Type: proto.NetworkResourceTypeDocument,
		Response: &proto.NetworkResponse{Status: 200, URL: "https://example.test/watch"},
	})
	if c.exhausted() {
		t.Fatal("gave up before the page finished loading")
	}
	c.loaded()
	if c.exhausted() {
		t.Fatal("gave up without giving the player its grace period")
	}
	c.mu.Lock()
	c.loadedAt = time.Now().Add(-noMediaGrace - time.Second)
	c.mu.Unlock()
	if !c.exhausted() {
		t.Fatal("kept waiting for media the page was never going to request")
	}
	if _, done := c.settled(); done {
		t.Fatal("settled reported a result with nothing matched")
	}
	// The summary is the whole point: it has to say what the page did instead.
	summary := c.observed()
	for _, want := range []string{"document=200", "requests=1", "accepted=0", "Document:1"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("observed() = %q, want it to report %q", summary, want)
		}
	}
}

func TestCollectorObservedSummarizesRejectedMediaWithoutSecrets(t *testing.T) {
	c := newCollector(Request{Accept: func(Candidate) bool { return false }})
	add := func(id, kind, mime, url string, status int) {
		c.request(&proto.NetworkRequestWillBeSent{RequestID: proto.NetworkRequestID(id), Type: proto.NetworkResourceType(kind), Request: &proto.NetworkRequest{URL: url,
			Headers: proto.NetworkHeaders{"Authorization": gson.New("secret-header")}}})
		c.response(&proto.NetworkResponseReceived{RequestID: proto.NetworkRequestID(id), Type: proto.NetworkResourceType(kind), Response: &proto.NetworkResponse{Status: status, MIMEType: mime}})
	}
	add("image", "Image", "image/gif", "https://cdn.test/cover.gif?token=secret-query", 200)
	add("gif-ok", "Media", "image/gif", "https://cdn.test/image.gif?token=secret-query", 200)
	add("gif-denied", "Media", "image/gif", "https://cdn.test/image.gif?token=secret-query", 403)
	add("segment-small", "Media", "video/x-secret-mime", "https://cdn.test/segment.m4s?path=secret-path", 200)
	c.data(&proto.NetworkDataReceived{RequestID: "segment-small", DataLength: 1})
	add("segment-large", "Media", "video/mp4", "https://cdn.test/segment.m4s?path=secret-path", 200)
	c.data(&proto.NetworkDataReceived{RequestID: "segment-large", DataLength: 64 << 10})
	// HasExtraInfo without a matching ExtraInfo event leaves this response awaiting it.
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "awaiting", Type: proto.NetworkResourceTypeMedia,
		Request: &proto.NetworkRequest{URL: "https://cdn.test/await.m4s"}})
	c.response(&proto.NetworkResponseReceived{RequestID: "awaiting", Type: proto.NetworkResourceTypeMedia, HasExtraInfo: true,
		Response: &proto.NetworkResponse{Status: 200, MIMEType: "video/mp4"}})
	add("failed", "Media", "video/mp4", "https://cdn.test/failed.mp4", 200)
	c.failed(&proto.NetworkLoadingFailed{RequestID: "failed"})

	got := c.observed()
	want := "document=0 requests=7 accepted=0 kinds=Media:6,Image:1 media=status=200,type=Media,mime=image/gif,ext=gif,bytes=small,discarded=false,awaiting-extra=false:1;status=200,type=Media,mime=video/mp4,ext=m4s,bytes=large,discarded=false,awaiting-extra=false:1;status=200,type=Media,mime=video/mp4,ext=m4s,bytes=small,discarded=false,awaiting-extra=true:1;status=200,type=Media,mime=video/mp4,ext=mp4,bytes=small,discarded=true,awaiting-extra=false:1;status=200,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=403,type=Media,mime=image/gif,ext=gif,bytes=small,discarded=false,awaiting-extra=false:1"
	if got != want {
		t.Fatalf("observed() = %q, want %q", got, want)
	}
	for _, secret := range []string{"secret-query", "secret-path", "secret-header", "cdn.test", "https://"} {
		if strings.Contains(got, secret) {
			t.Fatalf("observed() leaked %q: %s", secret, got)
		}
	}
}

func TestCollectorObservedBoundsMediaSummary(t *testing.T) {
	c := newCollector(Request{Accept: func(Candidate) bool { return false }})
	for status := 400; status < 410; status++ {
		id := proto.NetworkRequestID(fmt.Sprintf("media-%d", status))
		c.request(&proto.NetworkRequestWillBeSent{RequestID: id, Type: proto.NetworkResourceTypeMedia,
			Request: &proto.NetworkRequest{URL: fmt.Sprintf("https://secret.host/media-%d.m4s?token=secret", status), Headers: proto.NetworkHeaders{"X-Secret": gson.New("secret-header")}}})
		c.response(&proto.NetworkResponseReceived{RequestID: id, Type: proto.NetworkResourceTypeMedia,
			Response: &proto.NetworkResponse{Status: status, MIMEType: fmt.Sprintf("video/x-secret-%d", status)}})
	}
	want := "document=0 requests=10 accepted=0 kinds=Media:10 media=status=400,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=401,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=402,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=403,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=404,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=405,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=406,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;status=407,type=Media,mime=video/other,ext=m4s,bytes=small,discarded=false,awaiting-extra=false:1;other:2"
	first := c.observed()
	if first != want || len(first) > 1200 {
		t.Fatalf("observed() = %q, want exact bounded summary (length=%d)", first, len(first))
	}
	if second := c.observed(); second != first {
		t.Fatalf("observed() was not deterministic: %q then %q", first, second)
	}
	for _, secret := range []string{"secret.host", "secret", "X-Secret", "video/x-secret"} {
		if strings.Contains(first, secret) {
			t.Fatalf("observed() leaked %q: %s", secret, first)
		}
	}
}

// Split-track origins fetch a small initialisation segment from every quality
// before committing to one, and those probes are identical in shape to the
// track itself. Only the volume that follows separates them, so the byte count
// has to accumulate and re-open acceptance as it grows.
func TestCollectorAccumulatesTransferredBytes(t *testing.T) {
	const threshold = 64 << 10
	c := newCollector(Request{
		Accept: func(c Candidate) bool { return c.Bytes >= threshold },
		Settle: time.Hour,
	})
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "t", Request: &proto.NetworkRequest{URL: "https://cdn.test/t.m4s"}})
	c.response(&proto.NetworkResponseReceived{RequestID: "t", Response: &proto.NetworkResponse{Status: 200}})
	if len(c.matches()) != 0 {
		t.Fatal("accepted a track before any of it had arrived")
	}
	c.data(&proto.NetworkDataReceived{RequestID: "t", DataLength: 6515})
	if len(c.matches()) != 0 {
		t.Fatal("accepted an initialisation probe as a whole track")
	}
	c.data(&proto.NetworkDataReceived{RequestID: "t", DataLength: threshold})
	matches := c.matches()
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want the track accepted once it grew past the threshold", len(matches))
	}
	if want := int64(6515 + threshold); matches[0].Bytes != want {
		t.Fatalf("Bytes = %d, want the accumulated total %d", matches[0].Bytes, want)
	}
}

// A capture that matched media must never be cut short by the grace period.
func TestCollectorGraceDoesNotDiscardMatchedMedia(t *testing.T) {
	c := newCollector(Request{Accept: func(Candidate) bool { return true }, Settle: time.Hour})
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "m", Request: &proto.NetworkRequest{URL: "https://cdn.test/a.mp4"}})
	c.response(&proto.NetworkResponseReceived{RequestID: "m", Response: &proto.NetworkResponse{Status: 200}})
	c.loaded()
	c.mu.Lock()
	c.loadedAt = time.Now().Add(-noMediaGrace - time.Second)
	c.mu.Unlock()
	if c.exhausted() {
		t.Fatal("discarded a capture that had already matched media")
	}
}

func TestCollectorWaitsForReadyMedia(t *testing.T) {
	c := newCollector(Request{
		Accept: func(Candidate) bool { return true },
		Ready:  func(c Candidate) bool { return c.MIME == "video/mp4" },
		Settle: time.Hour,
	})
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "sub", Request: &proto.NetworkRequest{URL: "https://cdn.test/en.vtt"}})
	c.response(&proto.NetworkResponseReceived{RequestID: "sub", Response: &proto.NetworkResponse{Status: 200, MIMEType: "text/vtt"}})
	if !c.firstMatch.IsZero() || len(c.matches()) != 1 {
		t.Fatal("subtitle must be retained without starting settlement")
	}
	// Even a zero settle window must not return subtitles alone.
	c.req.Settle = 0
	if _, done := c.settled(); done {
		t.Fatal("subtitle alone settled")
	}
	c.req.Settle = time.Hour
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "video", Request: &proto.NetworkRequest{URL: "https://cdn.test/video"}})
	c.response(&proto.NetworkResponseReceived{RequestID: "video", Response: &proto.NetworkResponse{Status: 200, MIMEType: "video/mp4"}})
	if c.firstMatch.IsZero() {
		t.Fatal("media did not start settlement")
	}
	if _, done := c.settled(); done {
		t.Fatal("media settled before its window elapsed")
	}
	c.firstMatch = time.Now().Add(-2 * time.Hour)
	found, done := c.settled()
	if !done || len(found) != 2 || found[0].URL != "https://cdn.test/en.vtt" || found[1].URL != "https://cdn.test/video" {
		t.Fatalf("settled = %t, matches = %#v; want subtitle and media", done, found)
	}
}

func TestCollectorResetsWhenNoReadyCandidatesRemain(t *testing.T) {
	for _, change := range []string{"failed", "rejected", "not ready", "redirected"} {
		t.Run(change, func(t *testing.T) {
			c := newCollector(Request{
				Accept: func(Candidate) bool { return true },
				Ready:  func(c Candidate) bool { return c.MIME == "video/mp4" },
				Settle: time.Hour,
			})
			for _, id := range []proto.NetworkRequestID{"sub", "video", "alternate"} {
				mime := "video/mp4"
				if id == "sub" {
					mime = "text/vtt"
				}
				c.request(&proto.NetworkRequestWillBeSent{RequestID: id, Request: &proto.NetworkRequest{URL: "https://cdn.test/" + string(id)}})
				c.response(&proto.NetworkResponseReceived{RequestID: id, Response: &proto.NetworkResponse{Status: 200, MIMEType: mime}})
			}
			c.firstMatch = time.Now().Add(-2 * time.Hour)
			first := c.firstMatch
			c.failed(&proto.NetworkLoadingFailed{RequestID: "alternate"})
			if c.firstMatch != first {
				t.Fatal("timer reset while ready media remained")
			}
			switch change {
			case "failed":
				c.failed(&proto.NetworkLoadingFailed{RequestID: "video"})
			case "rejected":
				c.response(&proto.NetworkResponseReceived{RequestID: "video", Response: &proto.NetworkResponse{Status: 403}})
			case "not ready":
				c.response(&proto.NetworkResponseReceived{RequestID: "video", Response: &proto.NetworkResponse{Status: 200, MIMEType: "text/vtt"}})
			case "redirected":
				c.request(&proto.NetworkRequestWillBeSent{RequestID: "video", RedirectResponse: &proto.NetworkResponse{Status: 302}, Request: &proto.NetworkRequest{URL: "https://cdn.test/redirect"}})
			}
			if !c.firstMatch.IsZero() || len(c.matches()) == 0 {
				t.Fatal("timer must reset while retaining subtitles")
			}
			if _, done := c.settled(); done {
				t.Fatal("settled without ready media")
			}
			c.request(&proto.NetworkRequestWillBeSent{RequestID: "new", Request: &proto.NetworkRequest{URL: "https://cdn.test/new"}})
			c.response(&proto.NetworkResponseReceived{RequestID: "new", Response: &proto.NetworkResponse{Status: 200, MIMEType: "video/mp4"}})
			if c.firstMatch.IsZero() || c.firstMatch == first {
				t.Fatal("new media did not restart the timer")
			}
			if _, done := c.settled(); done {
				t.Fatal("new media inherited expired settle window")
			}
		})
	}
}

func TestCollectorAcceptsResponseMIME(t *testing.T) {
	c := newCollector(Request{Accept: func(c Candidate) bool { return c.MIME == "video/mp4" }})
	c.request(&proto.NetworkRequestWillBeSent{
		RequestID: "1", Type: "XHR", Request: &proto.NetworkRequest{URL: "https://cdn.test/stream?id=1"},
	})
	c.response(&proto.NetworkResponseReceived{
		RequestID: "1", Type: "Media", Response: &proto.NetworkResponse{Status: 200, MIMEType: "video/mp4"},
	})
	if found := c.matches(); len(found) != 1 || found[0].Kind != "Media" {
		t.Fatalf("MIME match missing: %#v", found)
	}
	c.response(&proto.NetworkResponseReceived{
		RequestID: "1", Response: &proto.NetworkResponse{Status: 200, MIMEType: "text/html"},
	})
	if len(c.matches()) != 0 {
		t.Fatal("response predicate was not re-evaluated")
	}
}

func TestCollectorRemovesLoadingFailures(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(failFirst), func(t *testing.T) {
			c := newCollector(testRequest())
			c.request(&proto.NetworkRequestWillBeSent{RequestID: "1", Request: &proto.NetworkRequest{URL: "https://cdn.test/a"}})
			if failFirst {
				c.failed(&proto.NetworkLoadingFailed{RequestID: "1"})
			}
			c.response(&proto.NetworkResponseReceived{RequestID: "1", Response: &proto.NetworkResponse{Status: 200}})
			c.failed(&proto.NetworkLoadingFailed{RequestID: "1"})
			if len(c.matches()) != 0 || !c.firstMatch.IsZero() {
				t.Fatal("failed request retained or restarted settle window")
			}
			if _, done := c.settled(); done {
				t.Fatal("settled after the only match failed")
			}
		})
	}
}

func TestCollectorExtraInfoOrdering(t *testing.T) {
	// Enumerate every interleaving of the normal event stream and ExtraInfo
	// stream, including all headers arriving before requests or after responses.
	for _, redirect := range []bool{false, true} {
		for _, redirectExtra := range []bool{false, true} {
			if !redirect && redirectExtra {
				continue
			}
			normalCount, extraCount := 2, 1
			if redirect {
				normalCount++
				if redirectExtra {
					extraCount++
				}
			}
			for mask := 0; mask < 1<<(normalCount+extraCount); mask++ {
				var order []bool
				n := 0
				for i := 0; i < normalCount+extraCount; i++ {
					extra := mask&(1<<i) != 0
					order = append(order, extra)
					if extra {
						n++
					}
				}
				if n != extraCount {
					continue
				}
				t.Run(fmt.Sprintf("redirect=%t/redirectExtra=%t/order=%b", redirect, redirectExtra, mask), func(t *testing.T) {
					c := newCollector(testRequest())
					normal, extra := 0, 0
					for _, isExtra := range order {
						if isExtra {
							cookie := "final"
							if redirectExtra && extra == 0 {
								cookie = "redirect"
							}
							c.extraInfo(&proto.NetworkRequestWillBeSentExtraInfo{RequestID: "1", Headers: proto.NetworkHeaders{
								"cookie": gson.New(cookie), "Authorization": gson.New(cookie),
							}})
							extra++
						} else {
							if normal == normalCount-1 {
								c.response(&proto.NetworkResponseReceived{RequestID: "1", HasExtraInfo: true,
									Response: &proto.NetworkResponse{Status: 200, MIMEType: "video/mp4"}})
							} else {
								url := "https://cdn.test/final"
								if redirect && normal == 0 {
									url = "https://origin.test/redirect"
								}
								ev := &proto.NetworkRequestWillBeSent{RequestID: "1", Request: &proto.NetworkRequest{
									URL: url, Headers: proto.NetworkHeaders{"Cookie": gson.New("provisional"), "Referer": gson.New(url)},
								}}
								if normal == 1 {
									ev.RedirectResponse = &proto.NetworkResponse{Status: 302}
									ev.RedirectHasExtraInfo = redirectExtra
								}
								c.request(ev)
							}
							normal++
						}
						if normal < normalCount || extra < extraCount {
							if len(c.matches()) != 0 {
								t.Fatal("accepted before successful response and wire headers")
							}
						}
					}
					found := c.matches()
					if len(found) != 1 || found[0].URL != "https://cdn.test/final" {
						t.Fatalf("matches = %#v", found)
					}
					h := found[0].Headers
					if h["cookie"] != "final" || h["Authorization"] != "final" || h["Referer"] != found[0].URL || len(h) != 3 {
						t.Fatalf("incorrect wire headers: %#v", h)
					}
					h["cookie"] = "modified snapshot"
					if c.matches()[0].Headers["cookie"] != "final" {
						t.Fatal("snapshot aliases collector headers")
					}
				})
			}
		}
	}
}

func TestCollectorRedirectRemovesPreviousMatch(t *testing.T) {
	c := newCollector(testRequest())
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "1", Request: &proto.NetworkRequest{URL: "https://cdn.test/old"}})
	c.response(&proto.NetworkResponseReceived{RequestID: "1", Response: &proto.NetworkResponse{Status: 200}})
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "1", RedirectResponse: &proto.NetworkResponse{Status: 302},
		Request: &proto.NetworkRequest{URL: "https://cdn.test/new"}})
	if len(c.matches()) != 0 || !c.firstMatch.IsZero() {
		t.Fatal("redirect retained intermediate match")
	}
	// Responses may arrive in a different order than requests.
	c.request(&proto.NetworkRequestWillBeSent{RequestID: "2", Request: &proto.NetworkRequest{URL: "https://cdn.test/second"}})
	c.response(&proto.NetworkResponseReceived{RequestID: "2", Response: &proto.NetworkResponse{Status: 200}})
	c.firstMatch = time.Now().Add(-time.Hour)
	c.response(&proto.NetworkResponseReceived{RequestID: "1", Response: &proto.NetworkResponse{Status: 200}})
	found, done := c.settled()
	if !done || len(found) != 2 || found[0].URL != "https://cdn.test/new" || found[1].URL != "https://cdn.test/second" {
		t.Fatalf("matches not in request order: %#v, settled=%t", found, done)
	}
}
