package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"time"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	graphqlws "github.com/graph-gophers/graphql-transport-ws"
)

var (
	//go:embed index.html
	graphiqlHTML []byte

	//go:embed schema.graphql
	schemaSDL string
)

type resolver struct{}

type queryResolver struct{}

type subscriptionResolver struct{}

type tickResolver struct {
	at     string
	number int32
}

func (*resolver) Query() *queryResolver               { return &queryResolver{} }
func (*resolver) Subscription() *subscriptionResolver { return &subscriptionResolver{} }

func (*queryResolver) Hello() string { return "Hello from graphql-transport-ws!" }

func (*subscriptionResolver) Ticks(ctx context.Context, args struct{ Count *int32 }) <-chan *tickResolver {
	limit := int32(10)
	if args.Count != nil {
		limit = max(limit, *args.Count)
	}
	ch := make(chan *tickResolver)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var n int32
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				n++
				ch <- &tickResolver{at: t.UTC().Format(time.RFC3339), number: n}
				if n >= limit {
					return
				}
			}
		}
	}()
	return ch
}

func (r *tickResolver) At() string    { return r.at }
func (r *tickResolver) Number() int32 { return r.number }

func main() {
	schema := graphql.MustParseSchema(schemaSDL, &resolver{}, graphql.UseStringDescriptions())
	mux := http.NewServeMux()

	// GraphiQL UI
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(graphiqlHTML)
	})

	// GraphQL endpoint — handles both HTTP POST and WebSocket (graphql-transport-ws)
	mux.Handle("/graphql", graphqlws.NewHandlerFunc(schema, &relay.Handler{Schema: schema}))

	addr := ":8080"
	log.Printf("GraphQL  -> http://localhost%s/graphql", addr)
	log.Printf("GraphiQL -> http://localhost%s/", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
