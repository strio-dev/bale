package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/strio-dev/bale/store"
)

func writeBundleList(out io.Writer, entries []store.Entry, quiet, noTrunc bool) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if !quiet {
		if _, err := fmt.Fprintln(table, "NAMES\tID\tCONTENT"); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		id := entry.Descriptor.Digest.String()
		if !noTrunc {
			id = entry.Descriptor.Digest.Encoded()
			if len(id) > 12 {
				id = id[:12]
			}
			if entry.Descriptor.Digest.Algorithm() != "sha256" {
				id = entry.Descriptor.Digest.Algorithm().String() + ":" + id
			}
		}
		if quiet {
			if _, err := fmt.Fprintln(table, id); err != nil {
				return err
			}
			continue
		}
		names := strings.Join(entry.Names, ", ")
		if names == "" {
			names = "<none>"
		}
		content := "complete"
		if entry.MetadataOnly {
			content = "metadata"
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\n", names, id, content); err != nil {
			return err
		}
	}
	return table.Flush()
}
