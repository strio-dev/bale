package main

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry"
)

type createPreviewRow struct {
	roles, selectors, requiredBy map[string]bool
}

// writeCreatePreview describes the captured envelopes without fetching payloads
// or interpreting their media types. Origins already distinguish envelopes from
// opaque configs and layers, including configs whose media type resembles one.
func writeCreatePreview(out io.Writer, result bale.CreateResult) error {
	rows := map[string]*createPreviewRow{}
	add := func(repository, digest, role, selector, requiredBy string) error {
		if repository == "" || digest == "" {
			return fmt.Errorf("create preview: missing artifact repository or digest")
		}
		key, err := previewReference(repository, digest)
		if err != nil {
			return err
		}
		row := rows[key]
		if row == nil {
			row = &createPreviewRow{roles: map[string]bool{}, selectors: map[string]bool{}, requiredBy: map[string]bool{}}
			rows[key] = row
		}
		if role != "" {
			row.roles[role] = true
		}
		if selector != "" {
			row.selectors[selector] = true
		}
		if requiredBy != "" {
			row.requiredBy[requiredBy] = true
		}
		return nil
	}
	inputName := func(index int) string {
		name := fmt.Sprintf("include[%d]", index)
		if index >= 0 && index < len(result.Metadata.Recipe.Include) {
			if alias := result.Metadata.Recipe.Include[index].As; alias != "" {
				name += " (as " + strconv.Quote(alias) + ")"
			}
		}
		return name
	}
	for _, origin := range result.Metadata.Origins {
		if err := add(origin.Repository, origin.Descriptor.Digest.String(), "", "", ""); err != nil {
			return err
		}
	}
	for _, root := range result.Metadata.Roots {
		selector, err := previewReference(root.Repository, root.Reference)
		if err != nil {
			return err
		}
		if err := add(root.Repository, root.Descriptor.Digest.String(), "input", selector, inputName(root.Input)); err != nil {
			return err
		}
	}
	for _, resolution := range result.Metadata.Dependencies {
		subject, err := previewReference(resolution.Repository, resolution.Subject.Digest.String())
		if err != nil {
			return err
		}
		if resolution.Declaration != nil {
			if err := add(resolution.Repository, resolution.Declaration.Digest.String(), "attachment", "", subject); err != nil {
				return err
			}
		}
		for _, dependency := range resolution.Dependencies {
			repository, selector, err := previewSelection(dependency.Ref)
			if err != nil {
				return err
			}
			if err := add(repository, dependency.Descriptor.Digest.String(), "dependency", selector, subject); err != nil {
				return err
			}
		}
	}
	for _, plugin := range result.Metadata.Plugins {
		requiredBy := inputName(plugin.Input)
		repository, selector, err := previewSelection(plugin.Ref)
		if err != nil {
			return err
		}
		if err := add(repository, plugin.Descriptor.Digest.String(), "plugin", selector, requiredBy); err != nil {
			return err
		}
		if err := add(plugin.Repository, plugin.Declaration.Digest.String(), "attachment", "", requiredBy); err != nil {
			return err
		}
	}
	for _, attachment := range result.Metadata.Referrers {
		subject, err := previewReference(attachment.Repository, attachment.Subject.Digest.String())
		if err != nil {
			return err
		}
		if err := add(attachment.Repository, attachment.Descriptor.Digest.String(), "attachment", "", subject); err != nil {
			return err
		}
	}
	counts := map[string]int{}
	keys := make([]string, 0, len(rows))
	for key, row := range rows {
		keys = append(keys, key)
		if len(row.roles) == 0 {
			row.roles["content"] = true
			row.requiredBy["OCI graph"] = true
		}
		for role := range row.roles {
			counts[role]++
		}
	}
	sort.Strings(keys)
	var buffer bytes.Buffer
	fmt.Fprintln(&buffer, "Dry run: nothing published.")
	fmt.Fprintf(&buffer, "Bundle: %s\n", result.Descriptor.Digest)
	if result.MetadataOnly {
		fmt.Fprintln(&buffer, "Metadata capture: payloads remain at pinned sources until push or save.")
	}
	fmt.Fprintf(&buffer, "Artifacts: %d unique repository/digest identities\n", len(rows))
	fmt.Fprintf(&buffer, "Roles (may overlap): %d input, %d dependency, %d plugin, %d attachment, %d content\n\n",
		counts["input"], counts["dependency"], counts["plugin"], counts["attachment"], counts["content"])
	if len(rows) != 0 {
		table := tabwriter.NewWriter(&buffer, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(table, "ARTIFACT\tROLES\tSELECTORS\tREQUIRED BY"); err != nil {
			return err
		}
		for _, key := range keys {
			row := rows[key]
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", key, previewValues(row.roles), previewValues(row.selectors), previewValues(row.requiredBy)); err != nil {
				return err
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	_, err := buffer.WriteTo(out)
	return err
}

func previewValues(values map[string]bool) string {
	if len(values) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	return strings.Join(keys, "; ")
}

func previewReference(repository, reference string) (string, error) {
	ref, err := registry.ParseReference(repository)
	if err != nil {
		return "", fmt.Errorf("create preview: %w", err)
	}
	if ref.Reference != "" {
		return "", fmt.Errorf("create preview: expected repository without selector: %q", repository)
	}
	ref.Reference = reference
	if err := ref.ValidateReference(); err != nil {
		return "", fmt.Errorf("create preview: %w", err)
	}
	return ref.String(), nil
}

func previewSelection(reference string) (repository, selector string, err error) {
	ref, err := registry.ParseReference(reference)
	if err != nil {
		return "", "", fmt.Errorf("create preview: %w", err)
	}
	if ref.Reference == "" {
		return "", "", fmt.Errorf("create preview: expected tag or digest: %q", reference)
	}
	selector = ref.String()
	ref.Reference = ""
	return ref.String(), selector, nil
}
