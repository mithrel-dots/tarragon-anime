package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSummaryCountsLeafChecksAndNeverPassesSkippedOrMissingTests(t *testing.T) {
	for _, test := range []struct {
		name, action, want      string
		checks                  map[string]*check
		passed, failed, skipped int
	}{
		{"pass", "pass", "PASS", map[string]*check{"TestLive": {action: "pass"}}, 1, 0, 0},
		{"nested failure", "fail", "FAIL", map[string]*check{
			"TestLive": {action: "fail"}, "TestLive/sub": {action: "pass"}, "TestLive/dub": {action: "fail"},
		}, 1, 1, 0},
		{"missing browser", "pass", "INCOMPLETE", map[string]*check{"TestLive": {action: "skip"}}, 0, 0, 1},
		{"no matching tests", "pass", "ERROR", nil, 0, 0, 0},
		{"build failure", "fail", "FAIL", nil, 0, 0, 0},
		{"interrupted", "", "FAIL", map[string]*check{"TestLive": {}}, 0, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := result{action: test.action, checks: test.checks}
			status, passed, failed, skipped := r.summary()
			if status != test.want || passed != test.passed || failed != test.failed || skipped != test.skipped {
				t.Fatalf("summary = %s %d/%d/%d", status, passed, failed, skipped)
			}
		})
	}
}

func TestEventReportPreservesDiagnosticsAndRedactsSignedURLs(t *testing.T) {
	results := newResults()
	for _, e := range []event{
		{Action: "run", Package: packagePrefix + "anipub", Test: "TestLive/sub"},
		{Action: "output", Package: packagePrefix + "anipub", Test: "TestLive/sub", Output: "    live_test.go:10: HTTP 403 https://media.test/x?token=secret\n"},
		{Action: "fail", Package: packagePrefix + "anipub", Test: "TestLive/sub"},
		{Action: "fail", Package: packagePrefix + "anipub", Elapsed: 1.25},
	} {
		consume(results, e)
	}
	var output strings.Builder
	if report(&output, results, time.Unix(0, 0), errors.New("exit status 1")) {
		t.Fatal("failed run reported success")
	}
	text := output.String()
	for _, want := range []string{"| anipub | FAIL | 0 | 1 | 0 | 1.2 |", "HTTP 403 [URL]", "| animeflix | ERROR", "Runner: exit status 1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in report:\n%s", want, text)
		}
	}
	if strings.Contains(text, "secret") {
		t.Fatal("signed URL leaked into report")
	}
}
