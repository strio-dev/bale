package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	// CLI templates produce plain text, never HTML.
	// nosemgrep: cache.semgrep-rules.go.lang.security.audit.xss.import-text-template
	"text/template"

	"github.com/strio-dev/bale"
)

type outputFormatter struct {
	json     bool
	template *template.Template
}

func newOutputFormatter(format string, defaultJSON bool) (*outputFormatter, error) {
	f := &outputFormatter{}
	trimmed := strings.TrimSpace(format)
	// Podman also recognizes the legacy JSON shortcuts without an argument.
	jsonShortcut := trimmed == "json"
	if inner, ok := strings.CutPrefix(trimmed, "{{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}}"); ok {
			inner = strings.TrimSpace(inner)
			jsonShortcut = inner == "json" || inner == "json."
		}
	}
	if (format == "" && defaultJSON) || jsonShortcut {
		f.json = true
		return f, nil
	}
	if format == "" {
		return f, nil
	}
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	var err error
	f.template, err = template.New("output").Option("missingkey=error").Funcs(template.FuncMap{
		"json": func(value any) (string, error) {
			raw, err := json.Marshal(value)
			return string(raw), err
		},
		"join": strings.Join, "split": strings.Split,
		"lower": strings.ToLower, "upper": strings.ToUpper,
	}).Parse(format)
	if err != nil {
		return nil, fmt.Errorf("invalid --format: %w", err)
	}
	return f, nil
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func (f *outputFormatter) writeResult(out io.Writer, result any, digest string) error {
	if f.json {
		return writeJSON(out, result)
	}
	if f.template != nil {
		var buf bytes.Buffer
		if err := f.template.Execute(&buf, result); err != nil {
			return err
		}
		_, err := buf.WriteTo(out)
		return err
	}
	_, err := fmt.Fprintln(out, digest)
	return err
}

func (f *outputFormatter) writeInspections(out io.Writer, results []bale.InspectResult) error {
	if f.json {
		return writeJSON(out, results)
	}
	var buf bytes.Buffer
	for _, result := range results {
		if err := f.template.Execute(&buf, result); err != nil {
			return err
		}
	}
	_, err := buf.WriteTo(out)
	return err
}
