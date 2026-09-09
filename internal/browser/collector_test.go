package browser

import (
	"fmt"
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
