package chi_test

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func ExampleURLBuilder_Build() {
	r := chi.NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := chi.NewURLBuilder(r)

	// A value containing a slash is encoded as a single path segment.
	path, err := b.Build("/files/{name}", map[string]string{"name": "a/b"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(path)
	// Output: /files/a%2Fb
}

func ExampleURLBuilder_Build_regexp() {
	r := chi.NewRouter()
	r.Get("/files/{id:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {})

	b := chi.NewURLBuilder(r)

	// A value that does not match the regexp is rejected.
	_, err := b.Build("/files/{id:[0-9]+}", map[string]string{"id": "abc"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	// Output: error: chi: parameter "id" value "abc" does not match regexp "^[0-9]+$"
}

func ExampleURLBuilder_Build_wildcard() {
	r := chi.NewRouter()
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {})

	b := chi.NewURLBuilder(r)

	// A wildcard value may contain slashes, which are kept as path separators.
	path, err := b.Build("/files/*", map[string]string{"*": "a/b/c"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(path)
	// Output: /files/a/b/c
}

func ExampleURLBuilder_Build_missingParams() {
	r := chi.NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := chi.NewURLBuilder(r)

	// A missing parameter results in an error.
	_, err := b.Build("/files/{name}", map[string]string{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	// Output: error: chi: missing parameter "name" for route "/files/{name}"
}

func ExampleURLBuilder_Build_extraParams() {
	r := chi.NewRouter()
	r.Get("/files/{name}", func(w http.ResponseWriter, r *http.Request) {})

	b := chi.NewURLBuilder(r)

	// An extra parameter results in an error.
	_, err := b.Build("/files/{name}", map[string]string{"name": "a", "extra": "b"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	// Output: error: chi: extra parameters [extra] for route "/files/{name}"
}
