package graphqlws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type mockGraphQLService struct {
	mock.Mock
}

func (s *mockGraphQLService) Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]interface{}) (<-chan interface{}, error) {
	args := s.Called(ctx, document, operationName, variableValues)
	return args.Get(0).(<-chan interface{}), args.Error(1)
}

type mockHTTPHandler struct {
	mock.Mock
}

func (h *mockHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Called(w, r)
	w.WriteHeader(http.StatusOK)
}

type testMocker struct {
	handler  http.Handler
	mockSvc  *mockGraphQLService
	mockHTTP *mockHTTPHandler
	done     chan bool
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
		args             Args
		mockExpectations func(m testMocker)
		setup            func() testMocker
		want             Want
	}{
		"legacy protocol ok": {
			args: Args{
				isWebSocketTest: true,
				subprotocols:    []string{ProtocolGraphQLWS},
			},
			mockExpectations: func(m testMocker) {
				c := make(chan interface{})
				close(c)

				m.mockSvc.On("Subscribe", mock.AnythingOfType("*context.valueCtx"), "subscription{}", "", (map[string]interface{})(nil)).Return((<-chan interface{})(c), nil).Run(func(args mock.Arguments) {
					m.done <- true
				}).Once()
			},
			setup: func() testMocker {
				mockSvc := new(mockGraphQLService)
				return testMocker{
					handler: NewHandlerFunc(mockSvc, nil),
					mockSvc: mockSvc,
					done:    make(chan bool, 1),
				}
			},
			want: Want{
				expectedSubprotocol: ProtocolGraphQLWS,
				assertion: func(t *testing.T, conn *websocket.Conn) {
					err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"connection_init"}`))
					require.NoError(t, err)

					err = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","type":"start","payload":{"query":"subscription{}"}}`))
					require.NoError(t, err)

					_, _, err = conn.ReadMessage()
					require.NoError(t, err)
				},
			},
		},
		"graphql-transport-ws protocol unsupported ": {
			args: Args{
				isWebSocketTest: true,
				subprotocols:    []string{ProtocolGraphQLTransportWS},
			},
			setup: func() testMocker {
				return testMocker{handler: NewHandlerFunc(nil, nil)}
			},
			want: Want{
				expectedSubprotocol: ProtocolGraphQLTransportWS,
				assertion: func(t *testing.T, conn *websocket.Conn) {
					var closeError *websocket.CloseError

					_, _, err := conn.ReadMessage()
					assert.ErrorAs(t, err, &closeError, "Expected server to close connection for placeholder")
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
					assert.Equal(t, "", conn.Subprotocol(), "Expected no subprotocol to be selected")

					var closeError *websocket.CloseError

					_, _, err := conn.ReadMessage()
					assert.ErrorAs(t, err, &closeError, "Expected server to close connection for unsupported protocol")
				},
			},
		},
		"HTTP fallback ok": {
			mockExpectations: func(m testMocker) {
				m.mockHTTP.On("ServeHTTP", mock.Anything, mock.MatchedBy(func(r *http.Request) bool { return r.Method == http.MethodGet })).Run(func(args mock.Arguments) {
					m.done <- true
				}).Once()
			},
			setup: func() testMocker {
				mockHTTP := new(mockHTTPHandler)
				return testMocker{
					handler:  NewHandlerFunc(nil, mockHTTP),
					mockHTTP: mockHTTP,
					done:     make(chan bool, 1),
				}
			},
			want: Want{
				assertion: func(t *testing.T, conn *websocket.Conn) {},
			},
		},
	}

	for name, tt := range testTable {
		tt := tt

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mocker := tt.setup()
			if tt.mockExpectations != nil {
				tt.mockExpectations(mocker)
			}

			server := httptest.NewServer(mocker.handler)
			defer server.Close()

			if !tt.args.isWebSocketTest {
				_, err := http.Get(server.URL)
				require.NoError(t, err)

				select {
				case <-mocker.done:
				case <-time.After(1 * time.Second):
					t.Fatal("timed out waiting for ServeHTTP mock to be called")
				}

				mocker.mockHTTP.AssertExpectations(t)
				return
			}

			wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
			dialer := websocket.Dialer{Subprotocols: tt.args.subprotocols}
			conn, _, err := dialer.Dial(wsURL, nil)

			if tt.want.checkError != nil {
				tt.want.checkError(t, err)
				return
			}

			require.NoError(t, err)
			defer conn.Close()

			assert.Equal(t, tt.want.expectedSubprotocol, conn.Subprotocol())

			if tt.want.assertion != nil {
				tt.want.assertion(t, conn)
			}

			if mocker.done != nil {
				select {
				case <-mocker.done:
				case <-time.After(1 * time.Second):
					t.Fatal("timed out waiting for mock expectation to be met")
				}
			}

			if mocker.mockSvc != nil {
				mocker.mockSvc.AssertExpectations(t)
			}

			if mocker.mockHTTP != nil {
				mocker.mockHTTP.AssertExpectations(t)
			}
		})
	}
}

func TestContextGenerators(t *testing.T) {
	contextBuilderFunc := func() ContextGeneratorFunc {
		return func(ctx context.Context, r *http.Request) (context.Context, error) {
			return context.WithValue(ctx, "testKey", "test value"), nil
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
			Want: want{Context: context.WithValue(context.Background(), "testKey", "test value")},
		},
		"With_context_generator_error": {
			Args: args{Generator: []ContextGenerator{contextBuilderErrorFunc()}},
			Want: want{Context: nil, Error: "unexpected error generating context"},
		},
	}

	for name, tt := range testTable {
		tt := tt
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest("GET", "/graphql", nil)
			require.NoError(t, err, "Failed to create request")

			ctx, err := buildContext(req, tt.Args.Generator)

			if tt.Want.Error != "" {
				assert.EqualError(t, err, tt.Want.Error, "Expected error")
				return
			}
			assert.Equal(t, tt.Want.Context, ctx, "New context generated")
			require.NoError(t, err, "Error generating context")
		})
	}
}
