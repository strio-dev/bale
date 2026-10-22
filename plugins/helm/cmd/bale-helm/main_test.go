package main

import (
	"bytes"
	"strings"
	"testing"

	helmplugin "github.com/strio-dev/bale/plugins/helm"
)

func TestRunErrorsDoNotWrite(t *testing.T) {
	for _, input := range []string{"{}", strings.Repeat(" ", helmplugin.MaxRequestBytes+1)} {
		var output bytes.Buffer
		if err := run(strings.NewReader(input), &output); err == nil {
			t.Fatal("expected error")
		}
		if output.Len() != 0 {
			t.Fatalf("unexpected output %s", output.String())
		}
	}
}
