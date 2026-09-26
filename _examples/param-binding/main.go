// Command param-binding demonstrates the reusable URL parameter binder in
// middleware. It parses and validates path parameters once, before the
// business handler runs, and exposes the typed values through the request
// context.
package main

import (
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseUUID(raw string) (string, error) {
	if !uuidPattern.MatchString(raw) {
		return "", errors.New("invalid uuid")
	}
	return raw, nil
}

func main() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Two parameters are declared on the route. The middleware parses them
	// after the final route (including nested/mounted routers) has matched,
	// caches the parsed values per request and answers 400 before the handler
	// when a parameter is missing or invalid.
	itemParams := middleware.BindParams(
		middleware.RequiredURLParam("userID", middleware.IntURLParam),
		middleware.RequiredURLParam("itemID", parseUUID),
	)
	r.With(itemParams).Get("/users/{userID}/items/{itemID}", getItem)

	// Optional parameters stay unprovided when absent; no zero value is stored.
	listParams := middleware.BindParams(
		middleware.RequiredURLParam("userID", middleware.IntURLParam),
		middleware.OptionalURLParam("page", middleware.IntURLParam),
	)
	r.With(listParams).Get("/users/{userID}/pages/{page}", listItems)

	log.Println("listening on :3333")
	log.Println("try:")
	log.Println("  curl -s http://localhost:3333/users/7/items/11111111-2222-3333-4444-555555555555")
	log.Println("  curl -s http://localhost:3333/users/7/pages/2")
	log.Println("  curl -s http://localhost:3333/users/abc (400, raw value not echoed)")
	log.Fatal(http.ListenAndServe(":3333", r))
}

func getItem(w http.ResponseWriter, r *http.Request) {
	userID, _ := middleware.URLParam[int](r, "userID")
	itemID, _ := middleware.URLParam[string](r, "itemID")

	// Repeated reads do not invoke the parsers again.
	userIDAgain, _ := middleware.URLParam[int](r, "userID")
	_ = userIDAgain

	w.Write([]byte("user " + strconv.Itoa(userID) + " item " + itemID + "\n"))
}

func listItems(w http.ResponseWriter, r *http.Request) {
	userID, _ := middleware.URLParam[int](r, "userID")
	page, ok := middleware.URLParam[int](r, "page")
	if !ok {
		w.Write([]byte("listing for user " + strconv.Itoa(userID) + " without a page\n"))
		return
	}
	w.Write([]byte("listing for user " + strconv.Itoa(userID) + " page " + strconv.Itoa(page) + "\n"))
}
