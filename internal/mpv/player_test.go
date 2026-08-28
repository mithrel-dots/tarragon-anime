package mpv

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlayerRequiresAndUsesIPC(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "mpv")
	content := "#!/bin/sh\nexec \"$MPV_TEST_BINARY\" -test.run=TestMPVHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MPV_TEST_BINARY", os.Args[0])
	t.Setenv("MPV_HELPER", "1")
	t.Setenv("MPV_ARGS_FILE", argsFile)

	player := New([]string{"--profile=anime"}, log.New(io.Discard, "", 0))
	player.timeout = 2 * time.Second
	err := player.Play(t.Context(), Stream{
		URL: "https://video.test/master.m3u8", Headers: map[string]string{"Referer": "https://mkissa.to/"},
		Subtitle: "https://sub.test/en.ass",
	}, "Frieren - Episode 4")
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(args)
	for _, expected := range []string{"--input-ipc-server=", "--http-header-fields=Referer: https://mkissa.to/", "--sub-file=https://sub.test/en.ass", "https://video.test/master.m3u8"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("mpv args %q do not contain %q", joined, expected)
		}
	}
}

func TestMPVHelperProcess(t *testing.T) {
	if os.Getenv("MPV_HELPER") != "1" {
		return
	}
	separator := 0
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index + 1
			break
		}
	}
	args := os.Args[separator:]
	_ = os.WriteFile(os.Getenv("MPV_ARGS_FILE"), []byte(strings.Join(args, "\n")), 0o600)
	socket := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "--input-ipc-server=") {
			socket = strings.TrimPrefix(arg, "--input-ipc-server=")
		}
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(2)
	}
	conn, err := listener.Accept()
	if err != nil {
		os.Exit(3)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		os.Exit(4)
	}
	var request map[string]any
	if json.Unmarshal(scanner.Bytes(), &request) != nil {
		os.Exit(5)
	}
	encoder := json.NewEncoder(conn)
	_ = encoder.Encode(map[string]any{"request_id": 1, "error": "success", "data": "https://video.test/master.m3u8"})
	_ = encoder.Encode(map[string]any{"event": "file-loaded"})
	time.Sleep(50 * time.Millisecond)
	_ = conn.Close()
	_ = listener.Close()
	os.Exit(0)
}
