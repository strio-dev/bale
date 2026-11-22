// Package store manages local Bale bundles by name or immutable ID in one OCI
// content store. The supplied Bale client owns registry authentication and
// transport policy. Directory layouts remain explicit import/export details.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/strio-dev/bale"
	"github.com/strio-dev/bale/internal/localstore"
	"oras.land/oras-go/v2/content/oci"
)

type Options struct{ Path string }
type CreateOptions struct {
	Tags           []string
	ExportDelivery bool
	DryRun         bool
}
type PullOptions struct {
	Tags         []string
	Mode         bale.PullMode
	AllowPlugins bool
}
type Entry = localstore.Entry
type PruneResult = localstore.PruneResult

type Store struct {
	client *bale.Client
	local  *localstore.Store
}

func DefaultPath() (string, error) { return localstore.DefaultPath() }
func Open(client *bale.Client, options Options) (*Store, error) {
	return OpenContext(context.Background(), client, options)
}

// OpenContext initializes managed metadata with cancellable lock waits.
func OpenContext(ctx context.Context, client *bale.Client, options Options) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("a Bale client is required")
	}
	local, err := localstore.OpenContext(ctx, options.Path)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, local: local}, nil
}
func (s *Store) Path() string { return s.local.Path() }
func validateTags(names []string) error {
	for _, name := range names {
		if _, err := localstore.NormalizeTag(name); err != nil {
			return err
		}
	}
	return nil
}

// Create resolves and retains metadata without pulling ordinary payloads. Tags
// are local aliases; a creation without tags is retained by its bundle digest.
func (s *Store) Create(ctx context.Context, recipe bale.Recipe, options CreateOptions) (result bale.CreateResult, retErr error) {
	if err := validateTags(options.Tags); err != nil {
		return result, err
	}
	retErr = s.local.With(ctx, func(session *localstore.Session) error {
		created, state, err := s.client.CreateInto(ctx, recipe, session.Storage(), bale.CreateOptions{DryRun: options.DryRun, ExportDelivery: options.ExportDelivery})
		result = created
		if err != nil || options.DryRun {
			return err
		}
		if err := session.Publish(ctx, result.Descriptor, state, options.Tags); err != nil {
			return err
		}
		record, err := session.Resolve(ctx, result.Descriptor.Digest.String())
		if err != nil {
			return err
		}
		result.MetadataOnly = record.MetadataOnly
		return nil
	})
	return
}

// Pull stores the complete graph. Tags are optional aliases; no name is inferred
// from a remote reference, and later local operations never search a registry.
func (s *Store) Pull(ctx context.Context, reference string, options PullOptions) (result bale.PullResult, retErr error) {
	if err := validateTags(options.Tags); err != nil {
		return result, err
	}
	mode := options.Mode
	if mode == "" {
		mode = bale.PullLocked
	}
	retErr = s.local.With(ctx, func(session *localstore.Session) error {
		var err error
		result, err = s.client.PullInto(ctx, reference, session.Storage(), mode, bale.PullOptions{AllowPlugins: options.AllowPlugins})
		if err != nil {
			return err
		}
		return session.Publish(ctx, result.Descriptor, nil, options.Tags)
	})
	return
}

func withRecord[T any](ctx context.Context, s *Store, reference string, fn func(*localstore.Session, localstore.Record) (T, error)) (result T, retErr error) {
	retErr = s.local.With(ctx, func(session *localstore.Session) error {
		record, err := session.Resolve(ctx, reference)
		if err != nil {
			return err
		}
		result, err = fn(session, record)
		return err
	})
	return
}
func recordRoot(session *localstore.Session, record localstore.Record) bale.Root {
	return bale.Root{Descriptor: record.Descriptor, Store: session.Storage()}
}

// Complete records may retain useful source captures, but ordinary reads and
// deliveries must not gain online fallback merely because that capture exists.
func missingCapture(record localstore.Record) *bale.CaptureState {
	if record.MetadataOnly {
		return record.Capture
	}
	return nil
}

// Materialize fetches missing pinned bytes into the shared CAS. It preserves the
// bundle digest, does not refresh selectors and never executes plugins.
func (s *Store) Materialize(ctx context.Context, reference string) (bale.PullResult, error) {
	return withRecord(ctx, s, reference, func(session *localstore.Session, record localstore.Record) (bale.PullResult, error) {
		return s.client.MaterializeInto(ctx, recordRoot(session, record), record.Capture, session.Storage())
	})
}

// Save first completes the stored graph, then writes a portable verified archive.
// Existing CAS bytes are reused, including bytes downloaded for another bundle.
func (s *Store) Save(ctx context.Context, reference, archivePath string) (bale.ArchiveResult, error) {
	return s.save(ctx, reference, archivePath, nil)
}

// SaveTo completes the stored graph and writes its verified OCI archive to out.
// It leaves out open; failed writes may leave a partial archive in the stream.
func (s *Store) SaveTo(ctx context.Context, reference string, out io.Writer) (bale.ArchiveResult, error) {
	if out == nil {
		return bale.ArchiveResult{}, fmt.Errorf("archive writer is required")
	}
	return s.save(ctx, reference, "", out)
}

func (s *Store) save(ctx context.Context, reference, archivePath string, out io.Writer) (bale.ArchiveResult, error) {
	return withRecord(ctx, s, reference, func(session *localstore.Session, record localstore.Record) (bale.ArchiveResult, error) {
		// Check the output before downloading anything. Client.Save repeats the
		// no-replacement check when committing the finished archive.
		if out == nil {
			if archivePath == "" {
				return bale.ArchiveResult{}, fmt.Errorf("archive output is required")
			}
			if _, err := os.Lstat(archivePath); err == nil {
				return bale.ArchiveResult{}, fmt.Errorf("archive path already exists: %s", archivePath)
			} else if !os.IsNotExist(err) {
				return bale.ArchiveResult{}, err
			}
		}
		if _, err := s.client.MaterializeInto(ctx, recordRoot(session, record), record.Capture, session.Storage()); err != nil {
			return bale.ArchiveResult{}, err
		}
		var result bale.ArchiveResult
		var err error
		if out != nil {
			result, err = s.client.SaveRootTo(ctx, recordRoot(session, record), out)
		} else {
			result, err = s.client.SaveRoot(ctx, recordRoot(session, record), archivePath)
		}
		result.LayoutPath = ""
		return result, err
	})
}

// Load validates an archive before retaining its root. Its temporary extraction
// is private and is removed after shared-CAS import; roots share their blobs.
func (s *Store) Load(ctx context.Context, archivePath string, tags []string) (result bale.ArchiveResult, retErr error) {
	return s.load(ctx, archivePath, nil, tags)
}

// LoadFrom verifies an archive stream before retaining its root and optional
// names. It leaves the reader open and removes its private temporary state.
func (s *Store) LoadFrom(ctx context.Context, in io.Reader, tags []string) (bale.ArchiveResult, error) {
	if in == nil {
		return bale.ArchiveResult{}, fmt.Errorf("archive reader is required")
	}
	return s.load(ctx, "", in, tags)
}

func (s *Store) load(ctx context.Context, archivePath string, in io.Reader, tags []string) (result bale.ArchiveResult, retErr error) {
	if err := validateTags(tags); err != nil {
		return result, err
	}
	retErr = s.local.With(ctx, func(session *localstore.Session) (operationErr error) {
		stage, err := os.MkdirTemp(s.Path(), ".load-")
		if err != nil {
			return err
		}
		defer func() { operationErr = errors.Join(operationErr, os.RemoveAll(stage)) }()
		layout := filepath.Join(stage, "layout")
		if in != nil {
			result, err = s.client.LoadFrom(ctx, in, layout)
		} else {
			result, err = s.client.Load(ctx, archivePath, layout)
		}
		result.LayoutPath = ""
		if err != nil {
			return err
		}
		source, err := oci.NewStorage(layout)
		if err != nil {
			return err
		}
		if _, err := s.client.MaterializeInto(ctx, bale.Root{Descriptor: result.Descriptor, Store: source}, nil, session.Storage()); err != nil {
			return err
		}
		result.LayoutPath = ""
		return session.Publish(ctx, result.Descriptor, nil, tags)
	})
	return
}

func (s *Store) Inspect(ctx context.Context, reference string) (bale.InspectResult, error) {
	return withRecord(ctx, s, reference, func(session *localstore.Session, record localstore.Record) (bale.InspectResult, error) {
		root := recordRoot(session, record)
		if record.MetadataOnly {
			return s.client.InspectMetadataRoot(ctx, root)
		}
		return s.client.InspectRoot(ctx, root)
	})
}
func (s *Store) List(ctx context.Context) (result []Entry, retErr error) {
	retErr = s.local.With(ctx, func(session *localstore.Session) error { var err error; result, err = session.List(ctx); return err })
	return
}
func (s *Store) Tag(ctx context.Context, reference string, names []string) error {
	return s.local.With(ctx, func(session *localstore.Session) error { return session.Tag(ctx, reference, names) })
}
func (s *Store) Remove(ctx context.Context, references []string) error {
	return s.local.With(ctx, func(session *localstore.Session) error { return session.Remove(ctx, references) })
}
func (s *Store) Prune(ctx context.Context, dryRun bool) (PruneResult, error) {
	return s.local.Prune(ctx, dryRun)
}
func (s *Store) ExportDelivery(ctx context.Context, reference string) (bale.DeliveryFile, error) {
	return withRecord(ctx, s, reference, func(session *localstore.Session, record localstore.Record) (bale.DeliveryFile, error) {
		root := recordRoot(session, record)
		if record.MetadataOnly {
			return s.client.ExportDeliveryMetadataRoot(ctx, root)
		}
		return s.client.ExportDeliveryRoot(ctx, root)
	})
}

func withPair[T any](ctx context.Context, s *Store, before, after string, fn func(context.Context, bale.Root, bale.Root) (T, error)) (result T, retErr error) {
	retErr = s.local.With(ctx, func(session *localstore.Session) error {
		left, err := session.Resolve(ctx, before)
		if err != nil {
			return err
		}
		right, err := session.Resolve(ctx, after)
		if err != nil {
			return err
		}
		result, err = fn(ctx, recordRoot(session, left), recordRoot(session, right))
		return err
	})
	return
}
func (s *Store) Diff(ctx context.Context, before, after string) (bale.DiffResult, error) {
	return withPair(ctx, s, before, after, s.client.DiffRoots)
}
func (s *Store) UpdateDelivery(ctx context.Context, before, after string, file bale.DeliveryFile) (bale.DeliveryUpdateResult, error) {
	return withPair(ctx, s, before, after, func(ctx context.Context, a, b bale.Root) (bale.DeliveryUpdateResult, error) {
		return s.client.UpdateDeliveryRoots(ctx, a, b, file)
	})
}

// PlanPush returns an inspection view, not a reloadable executable plan. It pins
// this lookup and never contacts registries. Push pins its own lookup and executes
// its sealed transport plan within the same activity lease.
func (s *Store) PlanPush(ctx context.Context, reference, destination string, options bale.PushOptions) (bale.DeliveryPlan, error) {
	return withRecord(ctx, s, reference, func(session *localstore.Session, record localstore.Record) (bale.DeliveryPlan, error) {
		result, err := s.client.PlanPushRoot(ctx, recordRoot(session, record), missingCapture(record), reference, destination, options)
		result.LayoutPath = reference
		return result, err
	})
}
func (s *Store) Push(ctx context.Context, reference, destination string, options bale.PushOptions) (bale.PushResult, error) {
	return withRecord(ctx, s, reference, func(session *localstore.Session, record localstore.Record) (bale.PushResult, error) {
		root, capture := recordRoot(session, record), missingCapture(record)
		plan, err := s.client.PlanPushRoot(ctx, root, capture, reference, destination, options)
		if err != nil {
			return bale.PushResult{}, err
		}
		return s.client.PushRoot(ctx, root, capture, plan)
	})
}
