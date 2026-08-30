package tarragon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type daemonHandler struct{}

func (daemonHandler) Request(_ context.Context, _, text string) Payload {
	return Payload{Results: []Result{{ID: "result", Label: text}}}
}

func (daemonHandler) Select(context.Context, Message) (bool, string) {
	return true, "Started playback"
}

func TestDaemonNDJSONContract(t *testing.T) {
	endpoint := filepath.Join(t.TempDir(), "plugins.sock")
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(t.Context())
	daemon := NewDaemon(endpoint, "anime", daemonHandler{}, log.New(io.Discard, "", 0))
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("missing hello")
	}
	var hello Message
	if err := json.Unmarshal(scanner.Bytes(), &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Type != "hello" || hello.Name != "anime" {
		t.Fatalf("hello = %#v", hello)
	}

	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(Message{Type: "request", QueryID: "q1", Text: "frieren"}); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() {
		t.Fatal("missing response")
	}
	var response struct {
		Type    string  `json:"type"`
		QueryID string  `json:"query_id"`
		Data    Payload `json:"data"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "response" || response.QueryID != "q1" || response.Data.Results[0].Label != "frieren" {
		t.Fatalf("response = %#v", response)
	}

	if err := encoder.Encode(Message{Type: "select", QueryID: "q1", ResultID: "result", Action: "play"}); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() {
		t.Fatal("missing select_response")
	}
	var selected selectResponse
	if err := json.Unmarshal(scanner.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	if selected.Type != "select_response" || !selected.Success {
		t.Fatalf("select response = %#v", selected)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v", err)
	}
}

const daemonTestTimeout = 5 * time.Second

type daemonTestClient struct {
	conn    net.Conn
	scanner *bufio.Scanner
	encoder *json.Encoder
}

type daemonWireMessage struct {
	Type    string  `json:"type"`
	QueryID string  `json:"query_id"`
	Data    Payload `json:"data"`
	Success bool    `json:"success"`
}

func startDaemonTestClient(t *testing.T, handler Handler) *daemonTestClient {
	t.Helper()
	endpoint := filepath.Join(t.TempDir(), "plugins.sock")
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	daemon := NewDaemon(endpoint, "anime", handler, log.New(io.Discard, "", 0))
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	conn, err := listener.Accept()
	if err != nil {
		cancel()
		listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		cancel()
		conn.Close()
		t.Fatal(err)
	}
	client := &daemonTestClient{conn: conn, scanner: bufio.NewScanner(conn), encoder: json.NewEncoder(conn)}
	if hello := readDaemonMessage(t, client); hello.Type != "hello" {
		t.Fatalf("hello type = %q", hello.Type)
	}
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() = %v", err)
			}
		case <-time.After(daemonTestTimeout):
			t.Error("Run() did not stop")
		}
	})
	return client
}

func sendDaemonMessage(t *testing.T, client *daemonTestClient, message Message) {
	t.Helper()
	if err := client.encoder.Encode(message); err != nil {
		t.Fatal(err)
	}
}

func readDaemonMessage(t *testing.T, client *daemonTestClient) daemonWireMessage {
	t.Helper()
	if err := client.conn.SetReadDeadline(time.Now().Add(daemonTestTimeout)); err != nil {
		t.Fatal(err)
	}
	if !client.scanner.Scan() {
		t.Fatalf("read daemon message: %v", client.scanner.Err())
	}
	var message daemonWireMessage
	if err := json.Unmarshal(client.scanner.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(daemonTestTimeout):
		t.Fatal("timed out waiting for synchronization")
		var zero T
		return zero
	}
}

type rapidRequestHandler struct {
	started       chan string
	canceled      chan string
	releaseLatest chan struct{}
}

func (h *rapidRequestHandler) Request(ctx context.Context, _, text string) Payload {
	h.started <- text
	if text == "latest" {
		<-h.releaseLatest
	} else {
		<-ctx.Done()
		h.canceled <- text
	}
	return Payload{Results: []Result{{ID: text, Label: text}}}
}

func (*rapidRequestHandler) Select(context.Context, Message) (bool, string) {
	return true, "selected"
}

func TestDaemonRapidRequestsCancelSupersededQuery(t *testing.T) {
	const requestCount = 20
	handler := &rapidRequestHandler{
		started:       make(chan string, requestCount),
		canceled:      make(chan string, requestCount),
		releaseLatest: make(chan struct{}),
	}
	client := startDaemonTestClient(t, handler)

	sendDaemonMessage(t, client, Message{Type: "request", QueryID: "query-0", Text: "first"})
	if started := receive(t, handler.started); started != "first" {
		t.Fatalf("first started request = %q", started)
	}
	for i := 1; i < requestCount-1; i++ {
		sendDaemonMessage(t, client, Message{Type: "request", QueryID: fmt.Sprintf("query-%d", i), Text: fmt.Sprintf("middle-%d", i)})
	}
	sendDaemonMessage(t, client, Message{Type: "request", QueryID: "query-latest", Text: "latest"})

	startedOld := 1
	for {
		started := receive(t, handler.started)
		if started == "latest" {
			break
		}
		startedOld++
	}
	for range startedOld {
		if canceled := receive(t, handler.canceled); canceled == "latest" {
			t.Fatal("latest request was canceled")
		}
	}
	close(handler.releaseLatest)
	response := readDaemonMessage(t, client)
	if response.Type != "response" || response.QueryID != "query-latest" || response.Data.Results[0].Label != "latest" {
		t.Fatalf("response = %#v", response)
	}

	sendDaemonMessage(t, client, Message{Type: "select", QueryID: "query-latest", ResultID: "latest"})
	if selected := readDaemonMessage(t, client); selected.Type != "select_response" || !selected.Success {
		t.Fatalf("message after latest response = %#v", selected)
	}
}

type boundedRequestHandler struct {
	active  atomic.Int32
	maximum atomic.Int32
	started chan struct{}
	release <-chan struct{}
	exited  chan string
}

func (h *boundedRequestHandler) Request(_ context.Context, _, text string) Payload {
	active := h.active.Add(1)
	for maximum := h.maximum.Load(); active > maximum && !h.maximum.CompareAndSwap(maximum, active); maximum = h.maximum.Load() {
	}
	h.started <- struct{}{}
	<-h.release
	h.active.Add(-1)
	h.exited <- text
	return Payload{Results: []Result{{ID: text, Label: text}}}
}

func (*boundedRequestHandler) Select(context.Context, Message) (bool, string) {
	return true, "selected"
}

func TestDaemonBoundsActiveRequests(t *testing.T) {
	const requestCount = maxConcurrentRequests * 3
	release := make(chan struct{})
	handler := &boundedRequestHandler{
		started: make(chan struct{}, requestCount),
		release: release,
		exited:  make(chan string, requestCount),
	}
	client := startDaemonTestClient(t, handler)
	for i := range maxConcurrentRequests {
		text := fmt.Sprintf("request-%d", i)
		sendDaemonMessage(t, client, Message{Type: "request", QueryID: text, Text: text})
		receive(t, handler.started)
	}
	for i := maxConcurrentRequests; i < requestCount; i++ {
		text := fmt.Sprintf("request-%d", i)
		sendDaemonMessage(t, client, Message{Type: "request", QueryID: text, Text: text})
	}
	select {
	case <-handler.started:
		t.Fatalf("more than %d request handlers started", maxConcurrentRequests)
	default:
	}
	if active := handler.active.Load(); active != maxConcurrentRequests {
		t.Fatalf("active handlers = %d, want %d", active, maxConcurrentRequests)
	}
	if maximum := handler.maximum.Load(); maximum != maxConcurrentRequests {
		t.Fatalf("maximum active handlers = %d, want %d", maximum, maxConcurrentRequests)
	}
	close(release)
	for {
		if exited := receive(t, handler.exited); exited == fmt.Sprintf("request-%d", requestCount-1) {
			break
		}
	}
	if maximum := handler.maximum.Load(); maximum > maxConcurrentRequests {
		t.Fatalf("maximum active handlers = %d, limit %d", maximum, maxConcurrentRequests)
	}
}

func TestDaemonKeepsReadingWhileWorkersAreSaturated(t *testing.T) {
	const floodCount = maxConcurrentRequests * 8
	release := make(chan struct{})
	handler := &boundedRequestHandler{
		started: make(chan struct{}, floodCount),
		release: release,
		exited:  make(chan string, floodCount),
	}
	client := startDaemonTestClient(t, handler)
	for i := range maxConcurrentRequests {
		text := fmt.Sprintf("blocked-%d", i)
		sendDaemonMessage(t, client, Message{Type: "request", QueryID: text, Text: text})
		receive(t, handler.started)
	}
	// Every worker is now stuck inside a handler, so further requests must not
	// back up into the socket reader.
	for i := range floodCount {
		text := fmt.Sprintf("flood-%d", i)
		sendDaemonMessage(t, client, Message{Type: "request", QueryID: text, Text: text})
	}

	sendDaemonMessage(t, client, Message{Type: "select", QueryID: "flood-0", ResultID: "result"})
	if selection := readDaemonMessage(t, client); selection.Type != "select_response" || !selection.Success {
		t.Fatalf("select response = %#v", selection)
	}

	if err := client.conn.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
}

type orderedRequestHandler struct {
	started chan string
	first   chan struct{}
	second  chan struct{}
	exited  chan string
}

func (h *orderedRequestHandler) Request(_ context.Context, queryID, _ string) Payload {
	h.started <- queryID
	if queryID == "first" {
		<-h.first
	} else {
		<-h.second
	}
	h.exited <- queryID
	return Payload{Results: []Result{{ID: queryID, Label: queryID}}}
}

func (*orderedRequestHandler) Select(context.Context, Message) (bool, string) {
	return true, "selected"
}

func TestDaemonDropsSupersededOutOfOrderResponse(t *testing.T) {
	handler := &orderedRequestHandler{
		started: make(chan string, 2),
		first:   make(chan struct{}),
		second:  make(chan struct{}),
		exited:  make(chan string, 2),
	}
	client := startDaemonTestClient(t, handler)
	sendDaemonMessage(t, client, Message{Type: "request", QueryID: "first"})
	if started := receive(t, handler.started); started != "first" {
		t.Fatalf("first started request = %q", started)
	}
	sendDaemonMessage(t, client, Message{Type: "request", QueryID: "second"})
	if started := receive(t, handler.started); started != "second" {
		t.Fatalf("second started request = %q", started)
	}

	close(handler.second)
	if response := readDaemonMessage(t, client); response.Type != "response" || response.QueryID != "second" {
		t.Fatalf("latest response = %#v", response)
	}
	close(handler.first)
	for range 2 {
		receive(t, handler.exited)
	}
	sendDaemonMessage(t, client, Message{Type: "select", QueryID: "second", ResultID: "second"})
	if response := readDaemonMessage(t, client); response.Type != "select_response" {
		t.Fatalf("message after stale completion = %#v", response)
	}
}

type concurrentResponseHandler struct {
	requestStarted chan struct{}
	selectStarted  chan struct{}
	release        chan struct{}
}

func (h *concurrentResponseHandler) Request(context.Context, string, string) Payload {
	close(h.requestStarted)
	<-h.release
	return Payload{Results: []Result{{ID: "result", Label: "result"}}}
}

func (h *concurrentResponseHandler) Select(context.Context, Message) (bool, string) {
	close(h.selectStarted)
	<-h.release
	return true, "selected"
}

func TestDaemonSerializesConcurrentResponses(t *testing.T) {
	handler := &concurrentResponseHandler{
		requestStarted: make(chan struct{}),
		selectStarted:  make(chan struct{}),
		release:        make(chan struct{}),
	}
	client := startDaemonTestClient(t, handler)
	sendDaemonMessage(t, client, Message{Type: "request", QueryID: "query"})
	receive(t, handler.requestStarted)
	sendDaemonMessage(t, client, Message{Type: "select", QueryID: "query", ResultID: "result"})
	receive(t, handler.selectStarted)
	close(handler.release)

	types := make(map[string]bool, 2)
	for range 2 {
		types[readDaemonMessage(t, client).Type] = true
	}
	if !types["response"] || !types["select_response"] {
		t.Fatalf("response types = %#v", types)
	}
}

type synchronousSelectHandler struct {
	selectStarted  chan struct{}
	releaseSelect  chan struct{}
	requestStarted chan struct{}
}

func (h *synchronousSelectHandler) Request(context.Context, string, string) Payload {
	close(h.requestStarted)
	return Payload{Results: []Result{}}
}

func (h *synchronousSelectHandler) Select(context.Context, Message) (bool, string) {
	close(h.selectStarted)
	<-h.releaseSelect
	return true, "selected"
}

func TestDaemonSelectionRemainsSynchronous(t *testing.T) {
	handler := &synchronousSelectHandler{
		selectStarted:  make(chan struct{}),
		releaseSelect:  make(chan struct{}),
		requestStarted: make(chan struct{}),
	}
	client := startDaemonTestClient(t, handler)
	sendDaemonMessage(t, client, Message{Type: "select", QueryID: "query", ResultID: "result"})
	receive(t, handler.selectStarted)
	sendDaemonMessage(t, client, Message{Type: "request", QueryID: "next"})
	select {
	case <-handler.requestStarted:
		t.Fatal("request started while selection was still running")
	default:
	}
	close(handler.releaseSelect)
	if response := readDaemonMessage(t, client); response.Type != "select_response" {
		t.Fatalf("selection response = %#v", response)
	}
	receive(t, handler.requestStarted)
	if response := readDaemonMessage(t, client); response.Type != "response" || response.QueryID != "next" {
		t.Fatalf("request response = %#v", response)
	}
}

type shutdownHandler struct {
	started  chan struct{}
	canceled chan struct{}
	release  <-chan struct{}
	exited   chan struct{}
}

func (h *shutdownHandler) Request(ctx context.Context, _, _ string) Payload {
	close(h.started)
	if h.release == nil {
		<-ctx.Done()
		close(h.canceled)
	} else {
		<-h.release
	}
	close(h.exited)
	return Payload{Results: []Result{}}
}

func (*shutdownHandler) Select(context.Context, Message) (bool, string) {
	return true, "selected"
}

func TestDaemonShutdownCancelsRequestsWithoutWaiting(t *testing.T) {
	t.Run("cancellable handler", func(t *testing.T) {
		handler := &shutdownHandler{
			started:  make(chan struct{}),
			canceled: make(chan struct{}),
			exited:   make(chan struct{}),
		}
		client, done := startShutdownDaemon(t, handler)
		sendDaemonMessage(t, client, Message{Type: "request", QueryID: "query"})
		receive(t, handler.started)
		if err := client.conn.Close(); err != nil {
			t.Fatal(err)
		}
		receive(t, handler.canceled)
		receive(t, handler.exited)
		if err := receive(t, done); err != nil {
			t.Fatalf("Run() = %v", err)
		}
	})

	t.Run("non-cooperative handler", func(t *testing.T) {
		release := make(chan struct{})
		handler := &shutdownHandler{
			started: make(chan struct{}),
			release: release,
			exited:  make(chan struct{}),
		}
		client, done := startShutdownDaemon(t, handler)
		sendDaemonMessage(t, client, Message{Type: "request", QueryID: "query"})
		receive(t, handler.started)
		if err := client.conn.Close(); err != nil {
			t.Fatal(err)
		}
		if err := receive(t, done); err != nil {
			t.Fatalf("Run() = %v", err)
		}
		close(release)
		receive(t, handler.exited)
	})
}

func startShutdownDaemon(t *testing.T, handler Handler) (*daemonTestClient, <-chan error) {
	t.Helper()
	endpoint := filepath.Join(t.TempDir(), "plugins.sock")
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	daemon := NewDaemon(endpoint, "anime", handler, log.New(io.Discard, "", 0))
	done := make(chan error, 1)
	go func() { done <- daemon.Run(context.Background()) }()
	conn, err := listener.Accept()
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	client := &daemonTestClient{conn: conn, scanner: bufio.NewScanner(conn), encoder: json.NewEncoder(conn)}
	if hello := readDaemonMessage(t, client); hello.Type != "hello" {
		t.Fatalf("hello type = %q", hello.Type)
	}
	return client, done
}
