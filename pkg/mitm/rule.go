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

// CompiledRoute is a ReactorRoute with a pre-compiled CEL program ready for evaluation.
type CompiledRoute struct {
	CEL     string
	Program cel.Program
	Action  *bridgev1.ReactorAction
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
		"request": map[string]any{
			"method":  req.Method,
			"host":    host,
			"path":    path,
			"headers": headers,
			"query":   query,
			"body":    body,
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

// CompiledReactor is a Reactor with pre-compiled routes and the host glob pattern.
type CompiledReactor struct {
	Host   string
	Routes []*CompiledRoute
}

// CompileReactor compiles a Reactor's CEL expressions into a CompiledReactor.
func CompileReactor(reactor *bridgev1.Reactor) (*CompiledReactor, error) {
	cr := &CompiledReactor{
		Host: reactor.GetHost(),
	}

	for i, route := range reactor.GetRoutes() {
		compiled, err := compileRoute(route)
		if err != nil {
			return nil, fmt.Errorf("route %d: %w", i, err)
		}
		cr.Routes = append(cr.Routes, compiled)
	}
	return cr, nil
}

func compileRoute(route *bridgev1.ReactorRoute) (*CompiledRoute, error) {
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

// newCELEnv creates a CEL environment with a `request` variable matching ReactorHttpRequest.
func newCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)),
	)
}
