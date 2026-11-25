// A tiny app for labd's tests. Build with -ldflags "-X main.mode=crash",
// "-X main.mode=unhealthy" or "-X main.mode=redirect" for broken variants.
// GET /probe reports what the sandbox lets it do (probe.go).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

var mode = "ok"

func main() {
	if mode == "crash" {
		fmt.Println("testapp: crashing on purpose")
		os.Exit(3)
	}
	port := os.Getenv("PORT")
	if port == "" {
		fmt.Println("testapp: PORT missing")
		os.Exit(2)
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "unhealthy":
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		case "redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		fmt.Fprint(w, "ok")
	})
	http.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	http.HandleFunc("/probe", probe)
	http.HandleFunc("/cookies", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("Cookie")) // what reached the app
	})
	http.HandleFunc("/impostor", impostor)
	http.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		env := map[string]string{}
		for _, k := range []string{"PORT", "DATA_DIR", "APP_ENV", "APP_NAME", "APP_VERSION", "BASE_URL"} {
			env[k] = os.Getenv(k)
		}
		json.NewEncoder(w).Encode(env)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "testapp %s %s\n", os.Getenv("APP_ENV"), os.Getenv("APP_VERSION"))
	})
	fmt.Printf("testapp listening on %s (%s)\n", port, mode)
	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		fmt.Println("testapp:", err)
		os.Exit(1)
	}
}
