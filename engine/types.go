// types.go holds Exchange.

package engine

import (
	"time"

	"github.com/wow-look-at-my/api-mirror/internal/mirror"
)

// Exchange is one completed outbound request, as the engine's transport saw it.
type Exchange struct {
	// Lane is why the engine sent it: fetch, probe, identity, refresh, passthrough, replay, notify or credential.
	Lane     string
	Method   string
	URL      string
	Path     string
	Status   int
	Bytes    int
	Started  time.Time
	Duration time.Duration
	// Principal is who the request was made for, where that is known.
	Principal string
	// Detail is display text from the lane, never a key.
	Detail string
	// Err is set when no response came back. Status is then zero.
	Err error
}

// Disposition is what the engine did with an inbound request.
type Disposition string

// The closed vocabulary of Request.Disposition.
const (
	DispositionHit         = Disposition(mirror.DispHit)
	DispositionMiss        = Disposition(mirror.DispMiss)
	DispositionPassthrough = Disposition(mirror.DispPassthrough)
	DispositionDenied      = Disposition(mirror.DispDenied)
	DispositionRefusal     = Disposition(mirror.DispRefusal)
	DispositionError       = Disposition(mirror.DispError)
	DispositionDelivery    = Disposition(mirror.DispDelivery)
	DispositionAdmin       = Disposition(mirror.DispAdmin)
)

// Request is one inbound request as the engine answered it.
type Request struct {
	At     time.Time
	Method string
	Path   string
	// Shape is the route pattern that matched, or the path when none did.
	Shape       string
	Resource    string
	Disposition Disposition
	// Reason qualifies the disposition, such as a passthrough reason.
	Reason    string
	Status    int
	Bytes     int
	Duration  time.Duration
	Principal string
}

func exchangeOf(x mirror.Exchange) Exchange {
	return Exchange{
		Lane:      string(x.Lane),
		Method:    x.Method,
		URL:       x.URL,
		Path:      x.Path,
		Status:    x.Status,
		Bytes:     x.Bytes,
		Started:   x.Started,
		Duration:  x.Duration,
		Principal: x.Principal,
		Detail:    x.Detail,
		Err:       x.Err,
	}
}

func requestOf(r mirror.Request) Request {
	return Request{
		At:          r.At,
		Method:      r.Method,
		Path:        r.Path,
		Shape:       r.Shape,
		Resource:    r.Resource,
		Disposition: Disposition(r.Disposition),
		Reason:      r.Reason,
		Status:      r.Status,
		Bytes:       r.Bytes,
		Duration:    r.Duration,
		Principal:   r.Principal,
	}
}
