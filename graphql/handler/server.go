package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/99designs/gqlgen/graphql/handler/transport"

	"github.com/vektah/gqlparser/validator"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/ast"
	"github.com/vektah/gqlparser/gqlerror"
	"github.com/vektah/gqlparser/parser"
)

type (
	Server struct {
		es          graphql.ExecutableSchema
		transports  []transport.Transport
		middlewares []Middleware
	}

	Handler func(ctx context.Context, writer transport.Writer)

	Middleware func(next Handler) Handler

	Option func(Server)

	ResponseStream func() *graphql.Response
)

func (s *Server) AddTransport(transport transport.Transport) {
	s.transports = append(s.transports, transport)
}

func (s *Server) Use(middleware Middleware) {
	s.middlewares = append(s.middlewares, middleware)
}

func New(es graphql.ExecutableSchema) *Server {
	return &Server{
		es: es,
	}
}

func (s *Server) getTransport(r *http.Request) transport.Transport {
	for _, t := range s.transports {
		if t.Supports(r) {
			return t
		}
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	transport := s.getTransport(r)
	if transport == nil {
		sendErrorf(w, http.StatusBadRequest, "transport not supported")
		return
	}

	rc, writer := transport.Do(w, r)
	if rc == nil {
		return
	}

	handler := s.executableSchemaHandler

	for i := len(s.middlewares) - 1; i >= 0; i-- {
		handler = s.middlewares[i](handler)
	}

	ctx := graphql.WithRequestContext(r.Context(), rc)
	handler(ctx, writer)
}

// executableSchemaHandler is the inner most handler, it invokes the graph directly after all middleware
// and sends responses to the transport so it can be returned to the client
func (s *Server) executableSchemaHandler(ctx context.Context, write transport.Writer) {
	rc := graphql.GetRequestContext(ctx)

	var gerr *gqlerror.Error

	// todo: hmm... how should this work?
	if rc.Doc == nil {
		rc.Doc, gerr = s.parseOperation(ctx, rc)
		if gerr != nil {
			write(&graphql.Response{Errors: []*gqlerror.Error{gerr}})
			return
		}
	}

	ctx, op, listErr := s.validateOperation(ctx, rc)
	if len(listErr) != 0 {
		write(&graphql.Response{
			Errors: listErr,
		})
		return
	}

	vars, err := validator.VariableValues(s.es.Schema(), op, rc.Variables)
	if err != nil {
		write(&graphql.Response{
			Errors: gqlerror.List{err},
		})
		return
	}

	rc.Variables = vars

	switch op.Operation {
	case ast.Query:
		resp := s.es.Query(ctx, op)
		write(resp)
	case ast.Mutation:
		resp := s.es.Mutation(ctx, op)
		write(resp)
	case ast.Subscription:
		resp := s.es.Subscription(ctx, op)

		for w := resp(); w != nil; w = resp() {
			write(w)
		}
	default:
		write(graphql.ErrorResponse(ctx, "unsupported GraphQL operation"))
	}
}

func (s *Server) parseOperation(ctx context.Context, rc *graphql.RequestContext) (*ast.QueryDocument, *gqlerror.Error) {
	ctx = rc.Tracer.StartOperationValidation(ctx)
	defer func() { rc.Tracer.EndOperationValidation(ctx) }()

	return parser.ParseQuery(&ast.Source{Input: rc.RawQuery})
}

func (gh *Server) validateOperation(ctx context.Context, rc *graphql.RequestContext) (context.Context, *ast.OperationDefinition, gqlerror.List) {
	ctx = rc.Tracer.StartOperationValidation(ctx)
	defer func() { rc.Tracer.EndOperationValidation(ctx) }()

	listErr := validator.Validate(gh.es.Schema(), rc.Doc)
	if len(listErr) != 0 {
		return ctx, nil, listErr
	}

	op := rc.Doc.Operations.ForName(rc.OperationName)
	if op == nil {
		return ctx, nil, gqlerror.List{gqlerror.Errorf("operation %s not found", rc.OperationName)}
	}

	return ctx, op, nil
}

func sendError(w http.ResponseWriter, code int, errors ...*gqlerror.Error) {
	w.WriteHeader(code)
	b, err := json.Marshal(&graphql.Response{Errors: errors})
	if err != nil {
		panic(err)
	}
	w.Write(b)
}

func sendErrorf(w http.ResponseWriter, code int, format string, args ...interface{}) {
	sendError(w, code, &gqlerror.Error{Message: fmt.Sprintf(format, args...)})
}
