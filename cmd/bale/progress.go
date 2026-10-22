package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/strio-dev/bale"
)

func writeProgress(out io.Writer, event bale.Progress) error {
	subject := event.Reference
	if subject == "" {
		subject = event.Repository
	}
	if event.Input != nil {
		subject = strings.TrimSpace(fmt.Sprintf("include[%d] %s", *event.Input, subject))
	}
	var line string
	switch event.Operation {
	case "resolving":
		line = "resolving " + subject
	case "resolving-plugin":
		line = fmt.Sprintf("resolving plugin %s for %s", event.Plugin, subject)
	case "fetching-plugin":
		line = fmt.Sprintf("fetching plugin %s for %s", event.Plugin, subject)
	case "running-plugin":
		line = fmt.Sprintf("discovering dependencies for %s using %s", subject, event.Plugin)
	case "capturing":
		line = "capturing " + subject
	case "captured":
		line = "captured bundle " + event.Descriptor.Digest.String()
	default:
		line = strings.TrimSpace(event.Operation + " " + event.Repository)
		if event.Descriptor.Digest != "" {
			line += " (" + event.Descriptor.Digest.String() + ")"
		}
	}
	_, err := fmt.Fprintln(out, line)
	return err
}
