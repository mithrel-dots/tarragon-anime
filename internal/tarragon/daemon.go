package tarragon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
)

const maxNDJSONLineSize = 1 << 20

type Handler interface {
	Request(context.Context, string, string) Payload
	Select(context.Context, Message) (bool, string)
}

type Daemon struct {
	endpoint string
	name     string
	handler  Handler
	logger   *log.Logger

	writeMu sync.Mutex
}

func NewDaemon(endpoint, name string, handler Handler, logger *log.Logger) *Daemon {
	return &Daemon{endpoint: endpoint, name: name, handler: handler, logger: logger}
}

func (d *Daemon) Run(ctx context.Context) error {
	conn, err := net.Dial("unix", d.endpoint)
	if err != nil {
		return fmt.Errorf("connect Tarragon endpoint %s: %w", d.endpoint, err)
	}
	defer conn.Close()
	if err := d.write(conn, Message{Type: "hello", Name: d.name}); err != nil {
		return fmt.Errorf("send Tarragon hello: %w", err)
	}
	d.logger.Printf("connected to %s", d.endpoint)

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), maxNDJSONLineSize)
	var requests sync.WaitGroup
	defer requests.Wait()
	for scanner.Scan() {
		var message Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			d.logger.Printf("invalid Tarragon message: %v", err)
			continue
		}
		switch message.Type {
		case "request":
			if message.QueryID == "" {
				continue
			}
			requests.Add(1)
			go func() {
				defer requests.Done()
				d.logger.Printf("request qid=%s: %s", message.QueryID, message.Text)
				payload := d.handler.Request(ctx, message.QueryID, message.Text)
				if err := d.write(conn, response{Type: "response", QueryID: message.QueryID, Data: payload}); err != nil && ctx.Err() == nil {
					d.logger.Printf("write Tarragon response: %v", err)
				} else if ctx.Err() == nil {
					d.logger.Printf("response sent qid=%s", message.QueryID)
				}
			}()
		case "select":
			success, text := d.handler.Select(ctx, message)
			if err := d.write(conn, selectResponse{Type: "select_response", Success: success, Message: text}); err != nil {
				return fmt.Errorf("write Tarragon selection response: %w", err)
			}
		default:
			d.logger.Printf("unsupported Tarragon message type=%q", message.Type)
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("read Tarragon messages: %w", err)
	}
	return nil
}

func (d *Daemon) write(conn net.Conn, message any) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return json.NewEncoder(conn).Encode(message)
}
