package mirror

import "net/http"

// HeaderAlias repeats a single response header under another name, for
// clients written against a service that spelled the mirror's headers differently.
type HeaderAlias struct {
	From, To string
}

// aliasWriter copies each aliased header just before the status goes out.
type aliasWriter struct {
	http.ResponseWriter
	aliases []HeaderAlias
	done    bool
}

func (a *aliasWriter) copyAliases() {
	if a.done {
		return
	}
	a.done = true
	h := a.ResponseWriter.Header()
	for _, al := range a.aliases {
		if v := h.Values(al.From); len(v) > 0 {
			h[http.CanonicalHeaderKey(al.To)] = v
		}
	}
}

func (a *aliasWriter) WriteHeader(status int) {
	a.copyAliases()
	a.ResponseWriter.WriteHeader(status)
}

func (a *aliasWriter) Write(b []byte) (int, error) {
	a.copyAliases()
	return a.ResponseWriter.Write(b)
}

// Flush keeps a streamed answer streaming through the wrapper.
func (a *aliasWriter) Flush() {
	a.copyAliases()
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (a *aliasWriter) Unwrap() http.ResponseWriter { return a.ResponseWriter }
