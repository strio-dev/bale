package relocate

import (
	"errors"
	"flag"
	"testing"
)

func TestParseArgs(t *testing.T) {
	o, raw, err := ParseArgs([]string{"--chart", chart, "--records-json", "[]", "--context=production"})
	if err != nil || o.Chart != chart || o.Context != "production" || string(raw) != "[]" {
		t.Fatalf("%+v %s %v", o, raw, err)
	}
	if selected, _, err := ParseArgs([]string{"--chart", chart, "--records-json", "[]", "--context", "--capture"}); err != nil || selected.Context != "--capture" {
		t.Fatalf("flag-like context: %+v %v", selected, err)
	}
	if _, _, err := ParseArgs([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
	for _, args := range [][]string{{}, {"--chart", chart}, {"--chart", chart, "--records-json", "[]", "extra"}, {"--chart", chart, "--chart", chart, "--records-json", "[]"}, {"--chart", chart, "--records-json", "[]", "--unknown"}} {
		if _, _, err := ParseArgs(args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}
