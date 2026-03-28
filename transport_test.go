package graphqlws

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type transportSubscribeCall struct {
	ctx           context.Context
	document      string
	operationName string
	variables     map[string]any
}

type fakeTransportService struct {
	mu          sync.Mutex
	calls       []transportSubscribeCall
	subscribeFn func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error)
}

func (s *fakeTransportService) Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
	s.mu.Lock()
	s.calls = append(s.calls, transportSubscribeCall{ctx: ctx, document: document, operationName: operationName, variables: variableValues})
	s.mu.Unlock()

	if s.subscribeFn == nil {
		c := make(chan any)
		close(c)
		return c, nil
	}

	return s.subscribeFn(ctx, document, operationName, variableValues)
}

func (s *fakeTransportService) getCalls() []transportSubscribeCall {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]transportSubscribeCall, len(s.calls))
	copy(out, s.calls)
	return out
}

type mockConnection struct {
	in           chan json.RawMessage
	out          chan json.RawMessage
	closeCalled  chan bool
	readLimit    int64
	writeTimeout time.Duration
	closeCode    int
	closeReason  string
	mtx          sync.Mutex
	isClosed     bool
}

func newMockConnection() *mockConnection {
	return &mockConnection{
		in:          make(chan json.RawMessage, 10),
		out:         make(chan json.RawMessage, 10),
		closeCalled: make(chan bool, 1),
	}
}

// ReadJSON reads the next JSON-formatted message from the connection and unmarshals it
func (ws *mockConnection) ReadJSON(v any) error {
	msg, ok := <-ws.in
	if !ok {
		return errors.New("connection closed")
	}

	return json.Unmarshal(msg, v)
}

// WriteJSON marshals the value v to JSON and sends it as a message over the connection
func (ws *mockConnection) WriteJSON(v any) error {
	ws.mtx.Lock()
	defer ws.mtx.Unlock()

	if ws.isClosed {
		return errors.New("writing to closed connection")
	}

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	ws.out <- data
	return nil
}

func (ws *mockConnection) SetReadLimit(limit int64) {
	ws.readLimit = limit
}

func (ws *mockConnection) SetWriteDeadline(t time.Time) error {
	ws.writeTimeout = time.Until(t)
	return nil
}

func (ws *mockConnection) WriteControl(messageType int, data []byte, deadline time.Time) error {
	if messageType != websocket.CloseMessage {
		return nil
	}

	ws.mtx.Lock()
	defer ws.mtx.Unlock()

	if len(data) >= 2 {
		ws.closeCode = int(binary.BigEndian.Uint16(data[:2]))
		ws.closeReason = string(data[2:])
	}

	return nil
}

func (ws *mockConnection) Close() error {
	ws.mtx.Lock()
	defer ws.mtx.Unlock()

	if !ws.isClosed {
		ws.isClosed = true

		close(ws.closeCalled)

		close(ws.out)
	}

	return nil
}

type mocker struct {
	conn    *mockConnection
	mockSvc *fakeTransportService
}

func setupTest(t *testing.T) mocker {
	t.Helper()

	return mocker{
		conn:    newMockConnection(),
		mockSvc: &fakeTransportService{},
	}
}

func TestOperationMap(t *testing.T) {
	t.Parallel()
	fn1 := func() {}

	type args struct {
		key      string
		fn       func()
		startMap map[string]func()
	}
	type want struct {
		fn func()
		ok bool
	}

	t.Run("add_and_get", func(t *testing.T) {
		t.Parallel()
		testTable := map[string]struct {
			args args
			want want
		}{
			"add_and_get_existing_key": {
				args: args{key: "key1", fn: fn1, startMap: map[string]func(){}},
				want: want{fn: fn1, ok: true},
			},
			"get_non_existent_key": {
				args: args{key: "key2", startMap: map[string]func(){}},
				want: want{fn: nil, ok: false},
			},
		}

		for name, tt := range testTable {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				om := newOperationMap()
				om.ops = tt.args.startMap

				if tt.args.fn != nil {
					om.add(tt.args.key, tt.args.fn)
				}

				gotFn, gotOk := om.get(tt.args.key)

				if gotOk != tt.want.ok {
					t.Fatalf("expected ok=%t, got %t", tt.want.ok, gotOk)
				}

				if tt.want.fn != nil {
					if gotFn == nil {
						t.Fatalf("expected function to be non-nil")
					}
				} else {
					if gotFn != nil {
						t.Fatalf("expected function to be nil")
					}
				}
			})
		}
	})

	t.Run("delete", func(t *testing.T) {
		t.Parallel()
		testTable := map[string]struct {
			args args
			want map[string]func()
		}{
			"delete_existing_key": {
				args: args{key: "key1", startMap: map[string]func(){"key1": fn1, "key2": fn1}},
				want: map[string]func(){"key2": fn1},
			},
			"delete_non_existent_key": {
				args: args{key: "key3", startMap: map[string]func(){"key1": fn1, "key2": fn1}},
				want: map[string]func(){"key1": fn1, "key2": fn1},
			},
			"delete_from_empty_map": {
				args: args{key: "key1", startMap: map[string]func(){}},
				want: map[string]func(){},
			},
		}

		for name, tt := range testTable {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				om := newOperationMap()
				om.ops = tt.args.startMap

				om.delete(tt.args.key)

				gotKeys := getMapKeys(om.ops)
				wantKeys := getMapKeys(tt.want)
				if !reflect.DeepEqual(wantKeys, gotKeys) {
					t.Fatalf("map keys do not match after delete: want=%v got=%v", wantKeys, gotKeys)
				}
			})
		}
	})
}

func TestConnect(t *testing.T) {
	t.Parallel()

	type Args struct {
		clientMessages []string
		options        []transportOption
	}
	type Want struct {
		serverMessages []string
		assertClose    bool
		closeCode      int
	}

	testTable := map[string]struct {
		setup        func(t *testing.T) mocker
		setupService func(h mocker)
		args         Args
		want         Want
		verifyCalls  func(t *testing.T, calls []transportSubscribeCall)
	}{
		"Successful subscription": {
			setup: setupTest,
			setupService: func(h mocker) {
				c := make(chan any, 1)
				c <- json.RawMessage(`{"data":{"foo":"bar"}}`)

				close(c)

				h.mockSvc.subscribeFn = func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
					return c, nil
				}
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }","operationName":"MySub","variables":{}}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"id":"1","type":"next","payload":{"data":{"foo":"bar"}}}`, `{"id":"1","type":"complete"}`},
				assertClose:    false,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 1 {
					t.Fatalf("expected 1 Subscribe call, got %d", len(calls))
				}
				if calls[0].document != "sub { hello }" || calls[0].operationName != "MySub" {
					t.Fatalf("unexpected Subscribe call: document=%q operationName=%q", calls[0].document, calls[0].operationName)
				}
			},
		},
		"Error if subscribe sent first": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"id":"1","type":"subscribe","payload":{}}`},
			},
			want: Want{
				serverMessages: []string{},
				assertClose:    true,
				closeCode:      closeCodeUnauthorized,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Error if connection_init sent twice": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"type":"connection_init"}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    true,
				closeCode:      closeCodeTooManyInitialisationReqs,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Error on invalid connection_init payload": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init","payload":"invalid"}`},
			},
			want: Want{
				serverMessages: []string{},
				assertClose:    true,
				closeCode:      closeCodeBadRequest,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Ping/Pong": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"type":"ping","payload":{"key":"val"}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"type":"pong","payload":{"key":"val"}}`},
				assertClose:    false,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Unsolicited pong ignored": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"type":"pong","payload":{"hb":true}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    false,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Error on subscribe with empty ID": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"","type":"subscribe","payload":{}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    true,
				closeCode:      closeCodeBadRequest,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Error on subscribe with duplicate ID": {
			setup: setupTest,
			setupService: func(h mocker) {
				c := make(chan any)
				h.mockSvc.subscribeFn = func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
					return c, nil
				}
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }"}}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }"}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    true,
				closeCode:      closeCodeSubscriberAlreadyExists,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 1 {
					t.Fatalf("expected 1 Subscribe call, got %d", len(calls))
				}
			},
		},
		"Error on invalid subscribe payload": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1","type":"subscribe","payload":"bad"}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    true,
				closeCode:      closeCodeBadRequest,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Complete message stops subscription": {
			setup: setupTest,
			setupService: func(h mocker) {
				c := make(chan any)
				h.mockSvc.subscribeFn = func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
					return c, nil
				}
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }"}}`, `{"id":"1","type":"complete"}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    false,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 1 {
					t.Fatalf("expected 1 Subscribe call, got %d", len(calls))
				}
			},
		},
		"Error on complete with missing ID": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"type":"complete"}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    true,
				closeCode:      closeCodeBadRequest,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Subscription error": {
			setup: setupTest,
			setupService: func(h mocker) {
				h.mockSvc.subscribeFn = func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
					return nil, errors.New("test sub error")
				}
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1s","type":"subscribe","payload":{"query":"sub { hello }"}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"id":"1s","type":"error","payload":[{"message":"test sub error"}]}`},
				assertClose:    false,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 1 {
					t.Fatalf("expected 1 Subscribe call, got %d", len(calls))
				}
			},
		},
		"Connection init timeout": {
			setup: setupTest,
			args: Args{
				options: []transportOption{transportWriteTimeout(50 * time.Millisecond)},
			},
			want: Want{
				serverMessages: []string{},
				assertClose:    true,
				closeCode:      closeCodeConnectionInitTimeout,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Max operations exceeded": {
			setup: setupTest,
			setupService: func(h mocker) {
				// Block the subscription channel to keep operations alive
				c := make(chan any)
				h.mockSvc.subscribeFn = func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
					return c, nil
				}
			},
			args: Args{
				// Limit to 1 concurrent operation
				options: []transportOption{transportMaxOperations(1)},
				clientMessages: []string{
					`{"type":"connection_init"}`,
					`{"id":"1","type":"subscribe","payload":{"query":"sub { hello }"}}`,
					`{"id":"2","type":"subscribe","payload":{"query":"sub { hello }"}}`,
				},
			},
			want: Want{
				serverMessages: []string{
					`{"type":"connection_ack"}`,
					`{"id":"2","type":"error","payload":[{"message":"too many concurrent subscriptions"}]}`,
				},
				assertClose: false,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				// Only the first subscribe should invoke the Subscribe method
				if len(calls) != 1 {
					t.Fatalf("expected 1 Subscribe call, got %d", len(calls))
				}
			},
		},
		"Unknown message type closes socket": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"type":"banana"}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    true,
				closeCode:      closeCodeBadRequest,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
		"Malformed JSON closes socket": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":`},
			},
			want: Want{
				serverMessages: []string{},
				assertClose:    true,
				closeCode:      closeCodeBadRequest,
			},
			verifyCalls: func(t *testing.T, calls []transportSubscribeCall) {
				if len(calls) != 0 {
					t.Fatalf("expected 0 Subscribe calls, got %d", len(calls))
				}
			},
		},
	}

	for name, tt := range testTable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := tt.setup(t)

			if tt.setupService != nil {
				tt.setupService(h)
			}

			go connectTransport(context.Background(), h.conn, h.mockSvc, tt.args.options...)

			go func() {
				for _, msg := range tt.args.clientMessages {
					h.conn.in <- json.RawMessage(msg)
				}

				if !tt.want.assertClose {
					time.Sleep(100 * time.Millisecond)
					close(h.conn.in)
				}
			}()

			receivedMessages := receiveTestMessages(t, h)

			if len(tt.want.serverMessages) != len(receivedMessages) {
				t.Fatalf("unexpected number of messages received: want=%d got=%d", len(tt.want.serverMessages), len(receivedMessages))
			}

			for i, expectedMsg := range tt.want.serverMessages {
				if i >= len(receivedMessages) {
					break
				}
				requireEqualJSON(t, expectedMsg, receivedMessages[i], fmt.Sprintf("Message %d mismatch", i))
			}

			if tt.want.assertClose {
				select {
				case <-h.conn.closeCalled:
					// Server closed connection as expected
					if tt.want.closeCode != 0 && h.conn.closeCode != tt.want.closeCode {
						t.Fatalf("unexpected close code: want=%d got=%d", tt.want.closeCode, h.conn.closeCode)
					}
				case <-time.After(1 * time.Second):
					t.Fatal("timed out waiting for server to close connection")
				}
			}

			if tt.verifyCalls != nil {
				tt.verifyCalls(t, h.mockSvc.getCalls())
			}
		})
	}
}

func getMapKeys(m map[string]func()) []string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	return keys
}

func requireEqualJSON(t *testing.T, expected string, actual json.RawMessage, msg string) {
	t.Helper()

	var expJSON, actJSON any

	err := json.Unmarshal([]byte(expected), &expJSON)
	if err != nil {
		t.Fatalf("failed to unmarshal expected JSON: %v", err)
	}

	err = json.Unmarshal(actual, &actJSON)
	if err != nil {
		t.Fatalf("failed to unmarshal actual JSON: %v", err)
	}

	if !reflect.DeepEqual(expJSON, actJSON) {
		if msg == "" {
			t.Fatalf("expected JSON %v got %v", expJSON, actJSON)
		}
		t.Fatalf("%s: expected %v got %v", msg, expJSON, actJSON)
	}
}

// receiveTestMessages handles the logic of listening for server messages with a timeout
func receiveTestMessages(t *testing.T, h mocker) []json.RawMessage {
	timeout := time.After(1 * time.Second)

	var receivedMessages []json.RawMessage

	for {
		select {
		case actualMsg, ok := <-h.conn.out:
			if !ok {
				// Channel is closed
				// Return the collected messages.
				return receivedMessages
			}
			receivedMessages = append(receivedMessages, actualMsg)
		case <-timeout:
			t.Fatalf("timed out waiting for server messages")
			return nil
		}
	}
}
