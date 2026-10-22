package relocate

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// ParseArgs parses the explicit, offline post-renderer arguments. Help returns flag.ErrHelp.
func ParseArgs(args []string) (Options, []byte, error) {
	var o Options
	var records, recordsFile string
	fs := flag.NewFlagSet("bale-relocate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Use flag's parser for values too: contexts may themselves begin with '-'.
	seen := map[string]bool{}
	for _, option := range []struct {
		name, help string
		target     *string
	}{
		{"records-file", "destination inspection JSON file", &recordsFile},
		{"records-json", "destination inspection JSON array", &records},
		{"chart", "delivered chart repository@digest", &o.Chart},
		{"bundle", "bundle repository@digest", &o.Bundle},
		{"source", "original chart repository@digest", &o.Source},
		{"capture", "capture digest", &o.Capture},
		{"declaration", "declaration digest", &o.Declaration},
		{"context", "declaration context", &o.Context},
	} {
		fs.Func(option.name, option.help, func(value string) error {
			if seen[option.name] {
				return fmt.Errorf("duplicate option --%s", option.name)
			}
			seen[option.name] = true
			*option.target = value
			return nil
		})
	}
	if err := fs.Parse(args); err != nil {
		return o, nil, err
	}
	if fs.NArg() != 0 {
		return o, nil, fmt.Errorf("unexpected positional arguments")
	}
	if seen["records-json"] && seen["records-file"] {
		return o, nil, fmt.Errorf("--records-json and --records-file are mutually exclusive")
	}
	if records == "" && recordsFile == "" {
		return o, nil, fmt.Errorf("--records-json or --records-file is required")
	}
	if err := validateOptions(o); err != nil {
		return o, nil, err
	}
	if recordsFile != "" {
		f, err := os.Open(recordsFile)
		if err != nil {
			return o, nil, fmt.Errorf("open records: %w", err)
		}
		defer func() { _ = f.Close() }()
		raw, err := io.ReadAll(io.LimitReader(f, MaxRecordsFileBytes+1))
		if err != nil {
			return o, nil, fmt.Errorf("read records: %w", err)
		}
		if len(raw) > MaxRecordsFileBytes {
			return o, nil, fmt.Errorf("records exceed %d bytes", MaxRecordsFileBytes)
		}
		return o, raw, nil
	}
	if len(records) > MaxRecordsBytes {
		return o, nil, fmt.Errorf("records exceed %d bytes", MaxRecordsBytes)
	}
	return o, []byte(records), nil
}
