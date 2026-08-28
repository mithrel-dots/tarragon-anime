package tarragon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"path/filepath"
	"testing"
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
