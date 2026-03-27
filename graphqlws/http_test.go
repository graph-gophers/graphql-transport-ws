package graphqlws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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
	mockSvc  *fakeGraphQLService
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
		"legacy protocol ok": {
			args: Args{
				isWebSocketTest: true,
				subprotocols:    []string{ProtocolGraphQLWS},
			},
			setup: func() testMocker {
				mockSvc := &fakeGraphQLService{}
				mockSvc.subscribeFn = func(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error) {
					c := make(chan any)
					close(c)
					return c, nil
				}
				return testMocker{
					handler: NewHandlerFunc(mockSvc, nil),
					mockSvc: mockSvc,
				}
			},
			want: Want{
				expectedSubprotocol: ProtocolGraphQLWS,
				assertion: func(t *testing.T, conn *websocket.Conn) {
					err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"connection_init"}`))
					if err != nil {
						t.Fatalf("failed to write connection_init: %v", err)
					}

					err = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","type":"start","payload":{"query":"subscription{}"}}`))
					if err != nil {
						t.Fatalf("failed to write start message: %v", err)
					}

					_, _, err = conn.ReadMessage()
					if err != nil {
						t.Fatalf("failed to read server message: %v", err)
					}
				},
			},
		},
		"graphql-transport-ws protocol ok ": {
			args: Args{
				isWebSocketTest: true,
				subprotocols:    []string{ProtocolGraphQLTransportWS},
			},
			setup: func() testMocker {
				mockSvc := &fakeGraphQLService{}
				return testMocker{handler: NewHandlerFunc(mockSvc, nil)}
			},
			want: Want{
				expectedSubprotocol: ProtocolGraphQLTransportWS,
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
				return testMocker{handler: NewHandlerFunc(nil, nil)}
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
					handler:  NewHandlerFunc(nil, mockHTTP),
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

			if name == "legacy protocol ok" {
				calls := mocker.mockSvc.getCalls()
				if len(calls) != 1 {
					t.Fatalf("expected exactly 1 subscribe call, got %d", len(calls))
				}
				if calls[0].document != "subscription{}" {
					t.Fatalf("unexpected subscribe document: %q", calls[0].document)
				}
				if calls[0].operationName != "" {
					t.Fatalf("unexpected subscribe operationName: %q", calls[0].operationName)
				}
				if calls[0].variables != nil {
					t.Fatalf("expected nil variables, got %#v", calls[0].variables)
				}
			}
		})
	}
}

func TestContextGenerators(t *testing.T) {
	contextBuilderFunc := func() ContextGeneratorFunc {
		return func(ctx context.Context, r *http.Request) (context.Context, error) {
			return context.WithValue(ctx, contextKey("testKey"), "test value"), nil
		}
	}

	contextBuilderErrorFunc := func() ContextGeneratorFunc {
		return func(ctx context.Context, r *http.Request) (context.Context, error) {
			return nil, errors.New("unexpected error generating context")
		}
	}

	type args struct {
		Generator []ContextGenerator
	}
	type want struct {
		Context context.Context
		Error   string
	}

	testTable := map[string]struct {
		Args args
		Want want
	}{
		"No_options": {
			Want: want{Context: context.Background()},
		},
		"With_context_generators": {
			Args: args{Generator: []ContextGenerator{contextBuilderFunc()}},
			Want: want{Context: context.WithValue(context.Background(), contextKey("testKey"), "test value")},
		},
		"With_context_generator_error": {
			Args: args{Generator: []ContextGenerator{contextBuilderErrorFunc()}},
			Want: want{Context: nil, Error: "unexpected error generating context"},
		},
	}

	for name, tt := range testTable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest("GET", "/graphql", nil)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}

			ctx, err := buildContext(req, tt.Args.Generator)

			if tt.Want.Error != "" {
				if err == nil || err.Error() != tt.Want.Error {
					t.Fatalf("expected error %q, got %v", tt.Want.Error, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error generating context: %v", err)
			}
			if !reflect.DeepEqual(tt.Want.Context, ctx) {
				t.Fatalf("unexpected context generated")
			}
		})
	}
}
