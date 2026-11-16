// host_principal.go holds the caller that an embedding host names.

package mirror

import (
	"context"
	"net/http"
)

type hostPrincipalKey struct{}

// WithHostPrincipal names the caller of a request the host authenticated. The
// engine then asks the upstream nothing about who asks.
func WithHostPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, hostPrincipalKey{}, principal)
}

func hostPrincipal(ctx context.Context) string {
	p, _ := ctx.Value(hostPrincipalKey{}).(string)
	return p
}

// caller names who asks: the host's principal when it gave one, else the
// spec's identity rules. It reports false when it answered the request.
func (e *Engine) caller(rec *recorder, r *http.Request) (*http.Request, string, bool) {
	if p := hostPrincipal(r.Context()); p != "" {
		return r, p, true
	}
	asserting := e.assertingRoute(r)
	if asserting {
		r = withBearerAssertion(r)
	}
	principal, refused := e.ids.resolve(r.Context(), r)
	if refused != nil && asserting {
		// The upstream decides a bearer the mirror could not verify.
		e.passthrough(rec, r, PassNoIdentity)
		return r, "", false
	}
	if refused != nil {
		rec.note(DispDenied, e.shapeOf(r), "", "identity")
		e.writeMessage(rec, refused.status, refused.message)
		return r, "", false
	}
	return r, principal, true
}
