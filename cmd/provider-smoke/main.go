// provider-smoke runs the opt-in provider integration tests and summarizes their
// JSON event stream without confusing successful builds with working providers.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const packagePrefix = "tarragon-anime/internal/provider/"
const liveTests = "^(TestLiveNativeStreams|TestLiveStreams|TestLiveMyStream|TestLiveMyStreamSlimeSeason4|TestLiveAnimePahe|TestLiveBrowserStreamsAcrossTitles)$"

var providers = []string{"anipub", "animeflix", "allanime", "animepahe", "anineko"}

type event struct {
	Action, Package, Test, Output string
	Elapsed                       float64
}

type check struct {
	action string
	output string
}

type result struct {
	action  string
	elapsed float64
	checks  map[string]*check
	output  string
}

func newResults() map[string]*result {
	results := make(map[string]*result)
	for _, name := range providers {
		results[name] = &result{checks: make(map[string]*check)}
	}
	return results
}

func tail(s string) string {
	const max = 16 * 1024
	if len(s) > max {
		return s[len(s)-max:]
	}
	return s
}

func consume(results map[string]*result, e event) {
	r := results[strings.TrimPrefix(e.Package, packagePrefix)]
	if r == nil {
		return
	}
	if e.Test == "" {
		if e.Action == "pass" || e.Action == "fail" || e.Action == "skip" {
			r.action, r.elapsed = e.Action, e.Elapsed
		}
		r.output = tail(r.output + e.Output)
		return
	}
	c := r.checks[e.Test]
	if c == nil {
		c = &check{}
		r.checks[e.Test] = c
	}
	c.output = tail(c.output + e.Output)
	if e.Action == "pass" || e.Action == "fail" || e.Action == "skip" {
		c.action = e.Action
	}
}

// Count only leaf tests: a failed subtest also fails its parent, but represents
// just one failed playback check. A skipped parent with no children still counts.
func (r *result) leaves() []string {
	var names []string
	for name := range r.checks {
		parent := false
		for other := range r.checks {
			if strings.HasPrefix(other, name+"/") {
				parent = true
				break
			}
		}
		if !parent {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (r *result) summary() (status string, passed, failed, skipped int) {
	for _, name := range r.leaves() {
		switch r.checks[name].action {
		case "pass":
			passed++
		case "skip":
			skipped++
		default:
			failed++ // Interrupted tests are not successful checks.
		}
	}
	switch {
	case r.action == "fail" || failed > 0:
		status = "FAIL"
	case skipped > 0 || r.action == "skip":
		status = "INCOMPLETE"
	case r.action != "pass" || passed == 0:
		status = "ERROR"
	default:
		status = "PASS"
	}
	return
}

var urls = regexp.MustCompile(`https?://[^\s"<>]+`)

func diagnostic(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "===") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "PASS") || strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "browser:") {
			continue
		}
		lines = append(lines, line)
	}
	text := strings.Join(lines, " ")
	// Live diagnostics can contain signed media URLs. Keep those out of reports.
	text = urls.ReplaceAllString(text, "[URL]")
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	if text == "" {
		return "No completed check or diagnostic; inspect the runner error."
	}
	return strings.NewReplacer("|", "\\|", "`", "'", "<", "&lt;", ">", "&gt;").Replace(text)
}

func report(w io.Writer, results map[string]*result, started time.Time, runErr error) bool {
	fmt.Fprintf(w, "# Provider smoke tests\n\nStarted: %s\n\n", started.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "Live catalogue, stream resolution, and media checks. No watch progress is written.")
	fmt.Fprintln(w, "\n| Provider | Result | Passed | Failed | Skipped | Seconds |")
	fmt.Fprintln(w, "| --- | --- | ---: | ---: | ---: | ---: |")
	ok := runErr == nil
	for _, name := range providers {
		r := results[name]
		status, passed, failed, skipped := r.summary()
		ok = ok && status == "PASS"
		fmt.Fprintf(w, "| %s | %s | %d | %d | %d | %.1f |\n", name, status, passed, failed, skipped, r.elapsed)
	}
	if !ok {
		fmt.Fprintln(w, "\n## Failures and skipped checks")
		for _, name := range providers {
			r := results[name]
			status, _, _, _ := r.summary()
			if status == "PASS" {
				continue
			}
			shown := false
			for _, test := range r.leaves() {
				c := r.checks[test]
				if c.action != "pass" {
					fmt.Fprintf(w, "\n- **%s / %s:** %s\n", name, test, diagnostic(c.output))
					shown = true
				}
			}
			if !shown {
				fmt.Fprintf(w, "\n- **%s:** %s\n", name, diagnostic(r.output))
			}
		}
		if runErr != nil {
			fmt.Fprintf(w, "\nRunner: %s\n", diagnostic(runErr.Error()))
		}
	}
	return ok
}

func run() int {
	output := flag.String("output", "", "also save the Markdown report to this file")
	timeout := flag.Duration("timeout", 15*time.Minute, "Go test timeout per provider package")
	flag.Parse()
	if *timeout <= 0 || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: provider-smoke [-output report.md] [-timeout 15m]")
		return 2
	}
	started := time.Now()
	results := newResults()
	args := []string{"test", "-json", "-tags=live", "-count=1", "-p=1", "-timeout=" + timeout.String(), "-run", liveTests}
	for _, name := range providers {
		args = append(args, "./internal/provider/"+name)
	}
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "ANIME_LIVE_MEDIA=1")
	cmd.Stderr = os.Stderr
	pipe, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err == nil {
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var e event
			if decodeErr := json.Unmarshal(scanner.Bytes(), &e); decodeErr != nil {
				err = fmt.Errorf("decode Go test output: %w", decodeErr)
				break
			}
			consume(results, e)
			if e.Test == "" && e.Action == "start" {
				fmt.Fprintf(os.Stderr, "Testing %s…\n", strings.TrimPrefix(e.Package, packagePrefix))
			}
		}
		if scanner.Err() != nil {
			err = scanner.Err()
		}
		if err != nil {
			_ = cmd.Process.Kill()
		}
		if waitErr := cmd.Wait(); err == nil {
			err = waitErr
		}
	}
	var text strings.Builder
	ok := report(&text, results, started, err)
	fmt.Print(text.String())
	if *output != "" {
		if err := os.WriteFile(*output, []byte(text.String()), 0644); err != nil {
			fmt.Fprintln(os.Stderr, "save report:", err)
			return 2
		}
	}
	if !ok {
		return 1
	}
	return 0
}

func main() { os.Exit(run()) }
