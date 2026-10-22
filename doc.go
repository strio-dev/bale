// Package bale relocates immutable OCI artifact graphs and captures portable
// Bale bundles. Callers may supply registry references or their own ORAS content
// stores. The package preserves artifact bytes and delegates application-specific
// dependency discovery and reference rewriting to the calling application.
//
// NewClient uses only explicitly supplied credentials and network options.
// Plan performs inspection without publishing; Execute returns confirmed
// publication locations, including partial progress when execution fails. Progress
// callbacks run synchronously and are serialized per client.
// Inspect, InspectLayout and InspectRoot verify existing bundles and expose
// captured inventory, dependency resolutions and selected declarations without
// refreshing selectors. Inspection reads the complete captured content graph.
//
// Attach publishes a caller-prepared OCI manifest or index into its existing
// subject's repository and verifies referrer discovery. ListReferrers validates
// remote envelopes before returning them. Attachment schemas remain caller-owned.
// Registry publication through Attach, Create, Execute and Push serializes per
// client; waiting callers may cancel. Read-only registry operations and dry runs
// do not acquire the publication permit. Callers coordinate overlapping writes
// through other clients or processes.
package bale
