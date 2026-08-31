package mpv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlayerRequiresAndUsesIPC(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(dir, "config")
	script := filepath.Join(dir, "mpv")
	content := "#!/bin/sh\nexec \"$MPV_TEST_BINARY\" -test.run=TestMPVHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args")
	commandsFile := filepath.Join(dir, "commands")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MPV_TEST_BINARY", os.Args[0])
	t.Setenv("MPV_HELPER", "1")
	t.Setenv("MPV_ARGS_FILE", argsFile)
	t.Setenv("MPV_COMMANDS_FILE", commandsFile)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	player := New([]string{"--profile=anime"}, log.New(io.Discard, "", 0))
	player.timeout = 2 * time.Second
	session, err := player.Play(t.Context(), Stream{
		URL: "https://video.test/master.m3u8", Headers: map[string]string{"Referer": "https://mkissa.to/"},
		Subtitle: "https://sub.test/en.ass",
	}, "Frieren - Episode 4", 15)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Load(t.Context(), Stream{URL: "https://video.test/next.m3u8"}, "Frieren - Episode 5", 0); err != nil {
		t.Fatal(err)
	}
	if err := session.AddSubtitle(t.Context(), "https://sub.test/next.ass"); err != nil {
		t.Fatal(err)
	}
	if err := session.ShowText(t.Context(), "Episode 5"); err != nil {
		t.Fatal(err)
	}
	session.Close()
	concrete := session.(*Session)
	select {
	case <-concrete.exited:
	default:
		t.Fatal("Close returned before mpv exited")
	}
	if concrete.cmd.ProcessState == nil {
		t.Fatal("Close returned before mpv was reaped")
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(args)
	for _, expected := range []string{
		"--config=yes",
		"--config-dir=" + filepath.Join(configHome, "mpv"),
		"--profile=anime",
		"--idle=yes",
		"--force-window=yes",
		"--start=15.000",
		"--input-ipc-server=",
		"--http-header-fields=Referer: https://mkissa.to/",
		"--sub-file=https://sub.test/en.ass",
		"https://video.test/master.m3u8",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("mpv args %q do not contain %q", joined, expected)
		}
	}
	commands, err := os.ReadFile(commandsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"mpv-version", "define-section", "Shift+N", "Shift+P", "Shift+S", "loadfile", "next.m3u8", "sub-add", "show-text"} {
		if !strings.Contains(string(commands), expected) {
			t.Errorf("mpv commands %q do not contain %q", commands, expected)
		}
	}
}

func TestSessionMapsIPCEvents(t *testing.T) {
	session, server := protocolSession(t)
	encoder := json.NewEncoder(server)
	tests := []struct {
		name    string
		message map[string]any
		want    Event
	}{
		{name: "file loaded", message: map[string]any{"event": "file-loaded"}, want: Event{Type: EventFileLoaded}},
		{name: "end file", message: map[string]any{"event": "end-file", "reason": "eof"}, want: Event{Type: EventEndFile, Reason: "eof"}},
		{name: "shutdown", message: map[string]any{"event": "shutdown"}, want: Event{Type: EventShutdown}},
		{name: "next", message: map[string]any{"event": "client-message", "args": []string{"tarragon-next"}}, want: Event{Type: EventNext}},
		{name: "previous", message: map[string]any{"event": "client-message", "args": []string{"tarragon-previous"}}, want: Event{Type: EventPrevious}},
		{name: "skip", message: map[string]any{"event": "client-message", "args": []string{"tarragon-skip"}}, want: Event{Type: EventSkip}},
		{name: "position", message: map[string]any{"event": "property-change", "name": "time-pos", "data": 12.5}, want: Event{Type: EventPosition, Value: 12.5}},
		{name: "duration", message: map[string]any{"event": "property-change", "name": "duration", "data": 24.5}, want: Event{Type: EventDuration, Value: 24.5}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := encoder.Encode(test.message); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-session.Events():
				if got != test.want {
					t.Fatalf("event = %#v, want %#v", got, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for event")
			}
		})
	}
}

func TestSessionPreservesTelemetryBeforeEOF(t *testing.T) {
	session, server := protocolSession(t)
	eventsWritten := make(chan error, 1)
	protocolDone := make(chan error, 1)
	go func() {
		encoder := json.NewEncoder(server)
		for _, message := range []map[string]any{
			{"event": "property-change", "name": "time-pos", "data": 12.5},
			{"event": "property-change", "name": "duration", "data": 24.5},
			{"event": "end-file", "reason": "eof"},
		} {
			if err := encoder.Encode(message); err != nil {
				eventsWritten <- err
				return
			}
		}
		eventsWritten <- nil

		scanner := bufio.NewScanner(server)
		if !scanner.Scan() {
			protocolDone <- scanner.Err()
			return
		}
		var request struct {
			RequestID int `json:"request_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			protocolDone <- err
			return
		}
		protocolDone <- encoder.Encode(map[string]any{"request_id": request.RequestID, "error": "success"})
	}()

	if err := <-eventsWritten; err != nil {
		t.Fatal(err)
	}
	if _, err := session.command(t.Context(), "get_property", "path"); err != nil {
		t.Fatal(err)
	}
	if err := <-protocolDone; err != nil {
		t.Fatal(err)
	}

	want := []Event{
		{Type: EventPosition, Value: 12.5},
		{Type: EventDuration, Value: 24.5},
		{Type: EventEndFile, Reason: "eof"},
	}
	for _, expected := range want {
		select {
		case got := <-session.Events():
			if got != expected {
				t.Fatalf("event = %#v, want %#v", got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %#v", expected)
		}
	}
}

func TestSessionPreservesControlEventsUnderTelemetryPressure(t *testing.T) {
	session, server := protocolSession(t)
	eventsWritten := make(chan error, 1)
	protocolDone := make(chan error, 1)
	go func() {
		encoder := json.NewEncoder(server)
		for index := 0; index < 2_000; index++ {
			name := "time-pos"
			if index%2 == 1 {
				name = "duration"
			}
			if err := encoder.Encode(map[string]any{"event": "property-change", "name": name, "data": index}); err != nil {
				eventsWritten <- err
				return
			}
		}
		for _, message := range []map[string]any{
			{"event": "file-loaded"},
			{"event": "end-file", "reason": "eof"},
			{"event": "shutdown"},
			{"event": "client-message", "args": []string{"tarragon-next"}},
			{"event": "client-message", "args": []string{"tarragon-previous"}},
			{"event": "client-message", "args": []string{"tarragon-skip"}},
		} {
			if err := encoder.Encode(message); err != nil {
				eventsWritten <- err
				return
			}
		}
		eventsWritten <- nil

		scanner := bufio.NewScanner(server)
		if !scanner.Scan() {
			protocolDone <- scanner.Err()
			return
		}
		var request struct {
			RequestID int `json:"request_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			protocolDone <- err
			return
		}
		protocolDone <- encoder.Encode(map[string]any{"request_id": request.RequestID, "error": "success"})
	}()

	select {
	case err := <-eventsWritten:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("IPC reader blocked by event delivery")
	}
	if _, err := session.command(t.Context(), "get_property", "path"); err != nil {
		t.Fatal(err)
	}
	if err := <-protocolDone; err != nil {
		t.Fatal(err)
	}

	want := []EventType{EventFileLoaded, EventEndFile, EventShutdown, EventNext, EventPrevious, EventSkip}
	var got []EventType
	deadline := time.After(time.Second)
	for len(got) < len(want) {
		select {
		case event := <-session.Events():
			if !isTelemetry(event) {
				got = append(got, event.Type)
			}
		case <-deadline:
			t.Fatalf("control events = %v, want %v", got, want)
		}
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("control events = %v, want %v", got, want)
		}
	}

	session.eventMu.Lock()
	queuedTelemetry := 0
	for _, event := range session.eventQueue {
		if isTelemetry(event) {
			queuedTelemetry++
		}
	}
	session.eventMu.Unlock()
	if queuedTelemetry > 2 {
		t.Fatalf("telemetry queue length = %d, want at most 2", queuedTelemetry)
	}
}

func TestSessionCleansPendingCommands(t *testing.T) {
	session, server := protocolSession(t)
	requestRead := make(chan struct{}, 2)
	closeServer := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(server)
		for range 2 {
			if !scanner.Scan() {
				return
			}
			requestRead <- struct{}{}
		}
		<-closeServer
		_ = server.Close()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := session.command(ctx, "get_property", "path")
		first <- err
	}()
	<-requestRead
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command error = %v", err)
	}
	assertNoPending(t, session)

	second := make(chan error, 1)
	go func() {
		_, err := session.command(t.Context(), "get_property", "duration")
		second <- err
	}()
	<-requestRead
	close(closeServer)
	if err := <-second; err == nil || !strings.Contains(err.Error(), "mpv IPC closed") {
		t.Fatalf("closed IPC command error = %v", err)
	}
	assertNoPending(t, session)
}

func TestSessionDeliversQueuedEventsAfterIPCEnds(t *testing.T) {
	session, server := protocolSession(t)
	encoder := json.NewEncoder(server)
	for _, message := range []map[string]any{
		{"event": "property-change", "name": "time-pos", "data": 42.5},
		{"event": "end-file", "reason": "eof"},
		{"event": "shutdown"},
	} {
		if err := encoder.Encode(message); err != nil {
			t.Fatal(err)
		}
	}
	// The consumer is still reading, so ending IPC must not discard events
	// that mpv already reported.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	want := []Event{
		{Type: EventPosition, Value: 42.5},
		{Type: EventEndFile, Reason: "eof"},
		{Type: EventShutdown},
	}
	for _, expected := range want {
		select {
		case got, ok := <-session.Events():
			if !ok {
				t.Fatalf("event channel closed before %#v", expected)
			}
			if got != expected {
				t.Fatalf("event = %#v, want %#v", got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %#v", expected)
		}
	}
	select {
	case _, ok := <-session.Events():
		if ok {
			t.Fatal("event channel produced an unexpected event")
		}
	case <-time.After(time.Second):
		t.Fatal("event channel was not closed after IPC ended")
	}
}

func TestReliableEventOverflowTerminatesSession(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	var logs bytes.Buffer
	session := newSession(client, nil, "", log.New(&logs, "", 0))
	go session.dispatchEvents()
	go session.readIPC()

	encoder := json.NewEncoder(server)
	for range eventQueueLimit + 2 {
		if err := encoder.Encode(map[string]any{"event": "client-message", "args": []string{"tarragon-next"}}); err != nil {
			break
		}
	}
	select {
	case <-session.readerDone:
	case <-time.After(time.Second):
		t.Fatal("overflow did not terminate IPC reader")
	}
	select {
	case <-session.eventDone:
	case <-time.After(time.Second):
		t.Fatal("overflow did not terminate event dispatcher")
	}
	if !strings.Contains(logs.String(), "reliable event queue overflow") {
		t.Fatalf("overflow log = %q", logs.String())
	}
	if _, ok := <-session.Events(); ok {
		t.Fatal("Events remained open after overflow")
	}
}

func TestCommandWriteHonorsContext(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	session := newSession(client, nil, "", log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := session.command(ctx, "quit")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("command error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked socket write took %v", elapsed)
	}
	assertNoPending(t, session)
}

func TestSessionCloseKillsAndReapsAfterQuitAcknowledgement(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCloseHelperProcess$")
	cmd.Env = append(os.Environ(), "MPV_CLOSE_HELPER=1")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	client, server := net.Pipe()
	session := newSession(client, cmd, "", log.New(io.Discard, "", 0))
	session.closeTimeout = 50 * time.Millisecond
	go session.dispatchEvents()
	go session.readIPC()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	go session.wait(exited)

	quitAcknowledged := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		scanner := bufio.NewScanner(server)
		if !scanner.Scan() {
			quitAcknowledged <- scanner.Err()
			return
		}
		var request struct {
			Command   []string `json:"command"`
			RequestID int      `json:"request_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			quitAcknowledged <- err
			return
		}
		if len(request.Command) != 1 || request.Command[0] != "quit" {
			quitAcknowledged <- errors.New("expected quit command")
			return
		}
		encoder := json.NewEncoder(server)
		if err := encoder.Encode(map[string]any{"event": "end-file", "reason": "eof"}); err != nil {
			quitAcknowledged <- err
			return
		}
		if err := encoder.Encode(map[string]any{"event": "client-message", "args": []string{"tarragon-next"}}); err != nil {
			quitAcknowledged <- err
			return
		}
		quitAcknowledged <- encoder.Encode(map[string]any{"request_id": request.RequestID, "error": "success"})
		for scanner.Scan() {
		}
	}()

	session.Close()
	if err := <-quitAcknowledged; err != nil {
		t.Fatal(err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("Close did not reap process")
	}
	select {
	case <-session.exited:
	default:
		t.Fatal("Close returned before process termination")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("IPC server did not observe connection close")
	}
	if _, ok := <-session.Events(); ok {
		t.Fatal("Close returned before Events closed")
	}
	select {
	case <-session.readerDone:
	default:
		t.Fatal("Close returned before IPC reader stopped")
	}
	select {
	case <-session.eventDone:
	default:
		t.Fatal("Close returned before event dispatcher stopped")
	}

	started := time.Now()
	session.Close()
	if elapsed := time.Since(started); elapsed > 25*time.Millisecond {
		t.Fatalf("second Close took %v", elapsed)
	}
}

func TestCloseHelperProcess(t *testing.T) {
	if os.Getenv("MPV_CLOSE_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func protocolSession(t *testing.T) (*Session, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	session := newSession(client, nil, "", log.New(io.Discard, "", 0))
	go session.dispatchEvents()
	go session.readIPC()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
		session.stopEvents()
		select {
		case <-session.readerDone:
		case <-time.After(time.Second):
			t.Error("IPC reader did not stop")
		}
		select {
		case <-session.eventDone:
		case <-time.After(time.Second):
			t.Error("event dispatcher did not stop")
		}
	})
	return session, server
}

func assertNoPending(t *testing.T, session *Session) {
	t.Helper()
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.pending) != 0 {
		t.Fatalf("pending commands = %d, want 0", len(session.pending))
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
	encoder := json.NewEncoder(conn)
	for scanner.Scan() {
		var request struct {
			Command   []any `json:"command"`
			RequestID int   `json:"request_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(5)
		}
		file, _ := os.OpenFile(os.Getenv("MPV_COMMANDS_FILE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = file.Write(append(append([]byte(nil), scanner.Bytes()...), '\n'))
		_ = file.Close()
		_ = encoder.Encode(map[string]any{"request_id": request.RequestID, "error": "success", "data": "ok"})
		if len(request.Command) > 0 && request.Command[0] == "quit" {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	_ = conn.Close()
	_ = listener.Close()
	os.Exit(0)
}
