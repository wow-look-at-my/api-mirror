// Command fixtureserver answers each GET path from one JSON file. A test
// can stand up a fake truth and a fake mirror that disagree in known ways.
// Each "{self}" in a fixture becomes the server's own base URL.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type routes map[string]string

func (r routes) String() string { return fmt.Sprint(map[string]string(r)) }

func (r routes) Set(v string) error {
	path, file, ok := strings.Cut(v, "=")
	if !ok || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("want /path=file.json, got %q", v)
	}
	r[path] = file
	return nil
}

func main() {
	rs := routes{}
	flag.Var(rs, "route", "a route as /path=file.json; repeatable")
	listen := flag.String("listen", "127.0.0.1:8099", "listen address")
	flag.Parse()
	self := "http://" + *listen
	err := http.ListenAndServe(*listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := rs[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		b, err := os.ReadFile(file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Link", "<"+self+r.URL.Path+"?page=2>; rel=\"next\"")
		_, _ = w.Write([]byte(strings.ReplaceAll(string(b), "{self}", self)))
	}))
	fmt.Fprintln(os.Stderr, "fixtureserver:", err)
	os.Exit(1)
}
