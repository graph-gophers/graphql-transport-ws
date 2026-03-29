package graphqlws_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	graphqlws "github.com/graph-gophers/graphql-transport-ws"
)

type contextKey string

type subscribeCall struct {
	ctx           context.Context
	document      string
	operationName string
	variables     map[string]any
}

type fakeGraphQLService struct {
	mu          sync.Mutex
	calls       []subscribeCall
	subscribeFn func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error)
}

func (s *fakeGraphQLService) Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
	s.mu.Lock()
	s.calls = append(s.calls, subscribeCall{ctx: ctx, document: document, operationName: operationName, variables: variableValues})
	s.mu.Unlock()

	if s.subscribeFn == nil {
		c := make(chan any)
		close(c)
		return c, nil
	}

	return s.subscribeFn(ctx, document, operationName, variableValues)
}

func (s *fakeGraphQLService) getCalls() []subscribeCall {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]subscribeCall, len(s.calls))
	copy(out, s.calls)
	return out
}

type fakeHTTPHandler struct {
	calls chan *http.Request
}

func (h *fakeHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.calls != nil {
		select {
		case h.calls <- r:
		default:
		}
	}

	w.WriteHeader(http.StatusOK)
}

type testMocker struct {
	handler  http.Handler
	mockHTTP *fakeHTTPHandler
}

func TestNewHandlerFunc(t *testing.T) {
	type Args struct {
		isWebSocketTest bool
		subprotocols    []string
	}

	type Want struct {
		assertion           func(t *testing.T, conn *websocket.Conn)
		checkError          func(t *testing.T, err error)
		expectedSubprotocol string
	}

	testTable := map[string]struct {
		args  Args
		setup func() testMocker
		want  Want
	}{
		"graphql-transport-ws protocol ok ": {
			args: Args{
				isWebSocketTest: true,
				subprotocols:    []string{graphqlws.ProtocolGraphQLTransportWS},
			},
			setup: func() testMocker {
				mockSvc := &fakeGraphQLService{}
				return testMocker{handler: graphqlws.NewHandlerFunc(mockSvc, nil)}
			},
			want: Want{
				expectedSubprotocol: graphqlws.ProtocolGraphQLTransportWS,
				assertion: func(t *testing.T, conn *websocket.Conn) {
					initMsg := `{"type":"connection_init"}`
					err := conn.WriteMessage(websocket.TextMessage, []byte(initMsg))
					if err != nil {
						t.Fatalf("failed to send connection_init: %v", err)
					}

					_, p, err := conn.ReadMessage()
					if err != nil {
						t.Fatalf("failed to read message from server: %v", err)
					}

					var msg struct {
						Type string `json:"type"`
					}

					err = json.Unmarshal(p, &msg)
					if err != nil {
						t.Fatalf("failed to unmarshal server message: %v", err)
					}

					if msg.Type != "connection_ack" {
						t.Fatalf("expected connection_ack message, got %q", msg.Type)
					}
				},
			},
		},
		"unsupported protocol error": {
			args: Args{
				isWebSocketTest: true,
				subprotocols:    []string{"unsupported-protocol"},
			},
			setup: func() testMocker {
				return testMocker{handler: graphqlws.NewHandlerFunc(nil, nil)}
			},
			want: Want{
				assertion: func(t *testing.T, conn *websocket.Conn) {
					if conn.Subprotocol() != "" {
						t.Fatalf("expected no subprotocol to be selected, got %q", conn.Subprotocol())
					}

					var closeError *websocket.CloseError

					_, _, err := conn.ReadMessage()
					if !errors.As(err, &closeError) {
						t.Fatalf("expected server to close connection for unsupported protocol, got err=%v", err)
					}
				},
			},
		},
		"HTTP fallback ok": {
			setup: func() testMocker {
				calls := make(chan *http.Request, 1)
				mockHTTP := &fakeHTTPHandler{calls: calls}
				return testMocker{
					handler:  graphqlws.NewHandlerFunc(nil, mockHTTP),
					mockHTTP: mockHTTP,
				}
			},
			want: Want{
				assertion: func(t *testing.T, conn *websocket.Conn) {},
			},
		},
	}

	for name, tt := range testTable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mocker := tt.setup()

			server := httptest.NewServer(mocker.handler)
			defer server.Close()

			if !tt.args.isWebSocketTest {
				_, err := http.Get(server.URL)
				if err != nil {
					t.Fatalf("HTTP fallback request failed: %v", err)
				}

				if mocker.mockHTTP != nil && mocker.mockHTTP.calls != nil {
					select {
					case req := <-mocker.mockHTTP.calls:
						if req.Method != http.MethodGet {
							t.Fatalf("expected HTTP method GET, got %s", req.Method)
						}
					case <-time.After(1 * time.Second):
						t.Fatal("timed out waiting for ServeHTTP to be called")
					}
				}
				return
			}

			wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
			dialer := websocket.Dialer{Subprotocols: tt.args.subprotocols}
			conn, _, err := dialer.Dial(wsURL, nil)

			if tt.want.checkError != nil {
				tt.want.checkError(t, err)
				return
			}

			if err != nil {
				t.Fatalf("websocket dial failed: %v", err)
			}
			defer conn.Close()

			if conn.Subprotocol() != tt.want.expectedSubprotocol {
				t.Fatalf("expected subprotocol %q, got %q", tt.want.expectedSubprotocol, conn.Subprotocol())
			}

			if tt.want.assertion != nil {
				tt.want.assertion(t, conn)
			}
		})
	}
}

func TestContextGenerators(t *testing.T) {
	t.Run("context value is visible to subscriber", func(t *testing.T) {
		t.Parallel()

		key := contextKey("testKey")
		mockSvc := &fakeGraphQLService{}

		handler := graphqlws.NewHandlerFunc(
			mockSvc,
			nil,
			graphqlws.WithContextGenerator(graphqlws.ContextGeneratorFunc(func(ctx context.Context, r *http.Request) (context.Context, error) {
				return context.WithValue(ctx, key, "test value"), nil
			})),
		)

		server := httptest.NewServer(handler)
		defer server.Close()

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
		dialer := websocket.Dialer{Subprotocols: []string{graphqlws.ProtocolGraphQLTransportWS}}
		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("websocket dial failed: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"connection_init"}`)); err != nil {
			t.Fatalf("failed to write connection_init: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","type":"subscribe","payload":{"query":"subscription{}"}}`)); err != nil {
			t.Fatalf("failed to write subscribe: %v", err)
		}

		deadline := time.Now().Add(1 * time.Second)
		for {
			calls := mockSvc.getCalls()
			if len(calls) > 0 {
				if got := calls[0].ctx.Value(key); got != "test value" {
					t.Fatalf("expected context value %q, got %#v", "test value", got)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for Subscribe call")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("context generator error rejects upgrade", func(t *testing.T) {
		t.Parallel()

		handler := graphqlws.NewHandlerFunc(
			&fakeGraphQLService{},
			nil,
			graphqlws.WithContextGenerator(graphqlws.ContextGeneratorFunc(func(ctx context.Context, r *http.Request) (context.Context, error) {
				return nil, errors.New("unexpected error generating context")
			})),
		)

		server := httptest.NewServer(handler)
		defer server.Close()

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
		dialer := websocket.Dialer{Subprotocols: []string{graphqlws.ProtocolGraphQLTransportWS}}
		_, resp, err := dialer.Dial(wsURL, nil)
		if err == nil {
			t.Fatal("expected websocket dial to fail")
		}
		if resp == nil {
			t.Fatal("expected HTTP response on failed websocket upgrade")
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected status %d, got %d", http.StatusForbidden, resp.StatusCode)
		}
	})
}
