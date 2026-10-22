package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	version := os.Getenv("FIXTURE_VERSION")
	fmt.Println(version)
	if len(os.Args) > 1 && os.Args[1] == "once" {
		return
	}
	http.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, version) })
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}
