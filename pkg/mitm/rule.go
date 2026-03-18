package mitm

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

// CompiledRoute is a ServerFacadeRoute with a pre-compiled CEL program ready for evaluation.
type CompiledRoute struct {
	CEL     string
	Program cel.Program
	Action  *bridgev1.ServerFacadeAction
}

// Match evaluates the CEL expression against the given HTTP request.
func (r *CompiledRoute) Match(req *http.Request) (bool, error) {
	var body string
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err == nil {
			body = string(b)
			req.Body = io.NopCloser(bytes.NewReader(b))
		}
	}

	query := make(map[string]string)
	for k, v := range req.URL.Query() {
		if len(v) > 0 {
			query[k] = v[0]
		}
	}

	headers := make(map[string]string)
	for k, v := range req.Header {
		if len(v) > 0 {
			headers[http.CanonicalHeaderKey(k)] = v[0]
		}
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	path := req.URL.Path
	if path == "" {
		path = "/"
	}

	out, _, err := r.Program.Eval(map[string]any{
		"request": &bridgev1.ServerFacadeHttpRequest{
			Method:  req.Method,
			Host:    host,
			Path:    path,
			Headers: headers,
			Query:   query,
			Body:    body,
		},
	})
	if err != nil {
		return false, fmt.Errorf("CEL eval: %w", err)
	}
	if out.Type() != types.BoolType {
		return false, fmt.Errorf("CEL expression returned %s, want bool", out.Type())
	}
	return out.Value().(bool), nil
}

// CompiledFacade is a ServerFacade with pre-compiled routes and the host glob pattern.
type CompiledFacade struct {
	Host   string
	Routes []*CompiledRoute
}

// CompileFacade compiles a ServerFacade's CEL expressions into a CompiledFacade.
func CompileFacade(facade *bridgev1.ServerFacade) (*CompiledFacade, error) {
	cf := &CompiledFacade{
		Host: facade.GetHost(),
	}

	for i, route := range facade.GetRoutes() {
		compiled, err := compileRoute(route)
		if err != nil {
			return nil, fmt.Errorf("route %d: %w", i, err)
		}
		cf.Routes = append(cf.Routes, compiled)
	}
	return cf, nil
}

func compileRoute(route *bridgev1.ServerFacadeRoute) (*CompiledRoute, error) {
	env, err := newCELEnv()
	if err != nil {
		return nil, fmt.Errorf("create CEL env: %w", err)
	}

	celExpr := route.GetMatch().GetCel()
	ast, issues := env.Compile(celExpr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile CEL: %w", issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program CEL: %w", err)
	}

	return &CompiledRoute{
		CEL:     celExpr,
		Program: prg,
		Action:  route.GetAction(),
	}, nil
}

// newCELEnv creates a CEL environment with a `request` variable of type ServerFacadeHttpRequest.
func newCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Types(&bridgev1.ServerFacadeHttpRequest{}),
		cel.Variable("request", cel.ObjectType("bridge.v1.ServerFacadeHttpRequest")),
	)
}
