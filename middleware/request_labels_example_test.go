package middleware_test

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
)

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+labels[k])
	}
	return "{" + strings.Join(pairs, " ") + "}"
}

// Three chained layers each merge labels into the request context. Inner
// layers override duplicate keys of outer layers, and every read returns an
// independent snapshot.
func ExampleWithRequestLabels() {
	outer := middleware.WithRequestLabels(context.Background(), map[string]string{
		"tenant": "A",
		"region": "us",
	})
	middle := middleware.WithRequestLabels(outer, map[string]string{
		"tenant": "B",
	})
	inner := middleware.WithRequestLabels(middle, map[string]string{
		"tenant": "C",
		"job":    "1",
	})

	fmt.Println("outer :", formatLabels(middleware.GetRequestLabels(outer)))
	fmt.Println("middle:", formatLabels(middleware.GetRequestLabels(middle)))
	fmt.Println("inner :", formatLabels(middleware.GetRequestLabels(inner)))

	// Output:
	// outer : {region=us tenant=A}
	// middle: {region=us tenant=B}
	// inner : {job=1 region=us tenant=C}
}
