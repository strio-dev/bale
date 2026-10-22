package main

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry"
)

// writePushPreview renders copy roots, rather than claiming to enumerate all
// transitive manifests or bytes in their unchanged OCI graphs.
func writePushPreview(out io.Writer, plan bale.DeliveryPlan) error {
	var buffer bytes.Buffer
	fmt.Fprintln(&buffer, "Dry run: nothing published.")
	fmt.Fprintf(&buffer, "Bundle: %s\n", plan.Descriptor.Digest)
	fmt.Fprintf(&buffer, "Completion tag: %s\n", plan.Destination)
	fmt.Fprintln(&buffer, "Local captured content validated; destination availability, authentication and existing tags not checked.")
	fmt.Fprintln(&buffer, "Copy roots include complete OCI graphs. The bundle capture is also copied to the completion repository.")
	fmt.Fprintln(&buffer, "Primary relocations do not remove copies required by unchanged graph references.")

	// All rows are built independently of the caller's slices, then sorted for a
	// stable report even when a plan was assembled through the public SDK.
	writeTable := func(title, header string, rows []string) error {
		fmt.Fprintf(&buffer, "\n%s (%d):\n", title, len(rows))
		if len(rows) == 0 {
			fmt.Fprintln(&buffer, "  (none)")
			return nil
		}
		sort.Strings(rows)
		table := tabwriter.NewWriter(&buffer, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(table, header); err != nil {
			return err
		}
		for _, row := range rows {
			if _, err := fmt.Fprintln(table, row); err != nil {
				return err
			}
		}
		return table.Flush()
	}
	var relocations, roots, tags, attachments []string
	for _, relocation := range plan.Relocations {
		source, err := pushPreviewReference(relocation.Source.Repository, relocation.Source.Digest.String())
		if err != nil {
			return err
		}
		destination, err := pushPreviewReference(relocation.Destination.Repository, relocation.Destination.Digest.String())
		if err != nil {
			return err
		}
		relocations = append(relocations, source+"\t"+destination)
	}
	for _, repository := range plan.Repositories {
		for _, root := range repository.Roots {
			reference, err := pushPreviewReference(repository.Repository, root.Digest.String())
			if err != nil {
				return err
			}
			roots = append(roots, reference)
		}
	}
	for _, tag := range plan.Tags {
		reference, err := pushPreviewReference(tag.Repository, tag.Reference)
		if err != nil {
			return err
		}
		tags = append(tags, reference+"\t"+tag.Descriptor.Digest.String())
	}
	for _, attachment := range plan.Destinations {
		reference, err := pushPreviewReference(attachment.Repository, attachment.Descriptor.Digest.String())
		if err != nil {
			return err
		}
		subject, err := pushPreviewReference(attachment.Repository, attachment.Subject.Digest.String())
		if err != nil {
			return err
		}
		attachments = append(attachments, reference+"\t"+subject)
	}
	for _, section := range []struct {
		title, header string
		rows          []string
	}{
		{"Canonical relocations", "SOURCE\tDESTINATION", relocations},
		{"Graph copy roots", "DESTINATION ROOT", roots},
		{"Restored tags", "TAG\tDIGEST", tags},
		{"Generated destination attachments", "ATTACHMENT\tSUBJECT", attachments},
	} {
		if err := writeTable(section.title, section.header, section.rows); err != nil {
			return err
		}
	}
	_, err := buffer.WriteTo(out)
	return err
}

func pushPreviewReference(repository, reference string) (string, error) {
	ref, err := registry.ParseReference(repository)
	if err != nil {
		return "", fmt.Errorf("push preview: %w", err)
	}
	if ref.Reference != "" {
		return "", fmt.Errorf("push preview: expected repository without selector: %q", repository)
	}
	ref.Reference = reference
	if err := ref.ValidateReference(); err != nil {
		return "", fmt.Errorf("push preview: %w", err)
	}
	// A parsed registry/repository and validated tag/digest cannot contain table
	// separators or terminal controls. No recipe labels or contexts are printed.
	return ref.String(), nil
}
