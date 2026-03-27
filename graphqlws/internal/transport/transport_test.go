package transport

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type mockGraphQLService struct {
	mock.Mock
}

// Subscribe is a mock implementation of a GraphQL subscription operation.
// It records the call with the provided context and GraphQL parameters,
// then returns a pre-configured channel and error based on the test setup.
func (s *mockGraphQLService) Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
	args := s.Called(ctx, document, operationName, variableValues)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(<-chan any), args.Error(1)
}

type mockConnection struct {
	in           chan json.RawMessage
	out          chan json.RawMessage
	closeCalled  chan bool
	readLimit    int64
	writeTimeout time.Duration
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
	mockSvc *mockGraphQLService
}

func setupTest(t *testing.T) mocker {
	t.Helper()

	return mocker{
		conn:    newMockConnection(),
		mockSvc: new(mockGraphQLService),
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

				assert.Equal(t, tt.want.ok, gotOk, "Expected 'ok' to match")

				if tt.want.fn != nil {
					assert.NotNil(t, gotFn, "Expected function to be non-nil")
				} else {
					assert.Nil(t, gotFn, "Expected function to be nil")
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
				assert.ElementsMatch(t, wantKeys, gotKeys, "Map keys do not match after delete")
			})
		}
	})
}

func TestConnect(t *testing.T) {
	t.Parallel()

	type Args struct {
		clientMessages []string
		options        []Option
	}
	type Want struct {
		serverMessages []string
		assertClose    bool
	}

	testTable := map[string]struct {
		setup            func(t *testing.T) mocker
		mockExpectations func(h mocker)
		args             Args
		want             Want
	}{
		"Successful subscription": {
			setup: setupTest,
			mockExpectations: func(h mocker) {
				c := make(chan any, 1)
				c <- json.RawMessage(`{"data":{"foo":"bar"}}`)

				close(c)

				h.mockSvc.On("Subscribe", mock.Anything, "sub { hello }", "MySub", mock.Anything).Return((<-chan any)(c), nil).Once()
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }","operationName":"MySub","variables":{}}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"id":"1","type":"next","payload":{"data":{"foo":"bar"}}}`, `{"id":"1","type":"complete"}`},
				assertClose:    false,
			},
		},
		"Error if subscribe sent first": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"id":"1","type":"subscribe","payload":{}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"error","payload":{"message":"connection_init message not received"}}`},
				assertClose:    true,
			},
		},
		"Error if connection_init sent twice": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"type":"connection_init"}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"type":"error","payload":{"message":"connection_init sent twice"}}`},
				assertClose:    true,
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
		},
		"Error on subscribe with empty ID": {
			setup: setupTest,
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"","type":"subscribe","payload":{}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"type":"error","payload":{"message":"missing ID for subscribe operation"}}`},
				assertClose:    false,
			},
		},
		"Error on subscribe with duplicate ID": {
			setup: setupTest,
			mockExpectations: func(h mocker) {
				c := make(chan any)
				h.mockSvc.On("Subscribe", mock.Anything, "sub { hello }", "", mock.Anything).Return((<-chan any)(c), nil).Once()
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }"}}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }"}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"id":"1","type":"error","payload":{"message":"duplicate operation ID"}}`},
				assertClose:    false,
			},
		},
		"Complete message stops subscription": {
			setup: setupTest,
			mockExpectations: func(h mocker) {
				c := make(chan any)
				h.mockSvc.On("Subscribe", mock.Anything, "sub { hello }", "", mock.Anything).Return((<-chan any)(c), nil).Once()
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1","type":"subscribe","payload":{"query":"sub { hello }"}}`, `{"id":"1","type":"complete"}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`},
				assertClose:    false,
			},
		},
		"Subscription error": {
			setup: setupTest,
			mockExpectations: func(h mocker) {
				h.mockSvc.On("Subscribe", mock.Anything, "sub { hello }", "", mock.Anything).Return((<-chan any)(nil), errors.New("test sub error")).Once()
			},
			args: Args{
				clientMessages: []string{`{"type":"connection_init"}`, `{"id":"1s","type":"subscribe","payload":{"query":"sub { hello }"}}`},
			},
			want: Want{
				serverMessages: []string{`{"type":"connection_ack"}`, `{"id":"1s","type":"error","payload":{"message":"test sub error"}}`},
				assertClose:    false,
			},
		},
		"Connection init timeout": {
			setup: setupTest,
			args: Args{
				options: []Option{WriteTimeout(50 * time.Millisecond)},
			},
			want: Want{
				serverMessages: []string{`{"type":"error","payload":{"message":"connection initialisation timeout"}}`},
				assertClose:    true,
			},
		},
	}

	for name, tt := range testTable {

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := tt.setup(t)

			if tt.mockExpectations != nil {
				tt.mockExpectations(h)
			}

			go Connect(context.Background(), h.conn, h.mockSvc, tt.args.options...)

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

			assert.Equal(t, len(tt.want.serverMessages), len(receivedMessages), "Unexpected number of messages received")

			for i, expectedMsg := range tt.want.serverMessages {
				if i >= len(receivedMessages) {
					break
				}
				requireEqualJSON(t, expectedMsg, receivedMessages[i], "Message %d mismatch", i)
			}

			if tt.want.assertClose {
				select {
				case <-h.conn.closeCalled:
					// Server closed connection as expected
				case <-time.After(1 * time.Second):
					t.Fatal("timed out waiting for server to close connection")
				}
			}

			h.mockSvc.AssertExpectations(t)
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

func requireEqualJSON(t *testing.T, expected string, actual json.RawMessage, msgAndArgs ...any) {
	t.Helper()

	var expJSON, actJSON any

	err := json.Unmarshal([]byte(expected), &expJSON)
	require.NoError(t, err, "Failed to unmarshal expected JSON")

	err = json.Unmarshal(actual, &actJSON)
	require.NoError(t, err, "Failed to unmarshal actual JSON")

	assert.Equal(t, expJSON, actJSON, msgAndArgs...)
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
