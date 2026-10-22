package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// writeLayoutResponse serves bytes already read from a test's OCI layout.
// Each caller owns request validation, failure injection and read accounting.
func writeLayoutResponse(w http.ResponseWriter, r *http.Request, raw []byte, digest string) {
	mediaType := "application/octet-stream"
	var envelope struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.MediaType != "" {
		mediaType = envelope.MediaType
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
	w.Header().Set("Docker-Content-Digest", digest)
	if r.Method == http.MethodGet {
		_, _ = w.Write(raw)
	}
}
