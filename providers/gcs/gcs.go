// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

// Package gcs implements common object storage abstractions against Google Cloud Storage.
package gcs

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/go-kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/common/version"
	"github.com/thanos-io/objstore"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"gopkg.in/yaml.v2"
)

// DirDelim is the delimiter used to model a directory structure in an object store bucket.
const DirDelim = "/"

// Config stores the configuration for gcs bucket.
type Config struct {
	Bucket         string `yaml:"bucket"`
	ServiceAccount string `yaml:"service_account"`
}

// Bucket implements the store.Bucket and shipper.Bucket interfaces against GCS.
type Bucket struct {
	logger log.Logger
	bkt    *storage.BucketHandle
	name   string

	closer io.Closer
}

// NewBucket returns a new Bucket against the given bucket handle.
func NewBucket(ctx context.Context, logger log.Logger, conf []byte, component string) (*Bucket, error) {
	var gc Config
	if err := yaml.Unmarshal(conf, &gc); err != nil {
		return nil, err
	}

	return NewBucketWithConfig(ctx, logger, gc, component)
}

// NewBucketWithConfig returns a new Bucket with gcs Config struct.
func NewBucketWithConfig(ctx context.Context, logger log.Logger, gc Config, component string) (*Bucket, error) {
	if gc.Bucket == "" {
		return nil, errors.New("missing Google Cloud Storage bucket name for stored blocks")
	}

	var opts []option.ClientOption

	// If ServiceAccount is provided, use them in GCS client, otherwise fallback to Google default logic.
	if gc.ServiceAccount != "" {
		credentials, err := google.CredentialsFromJSON(ctx, []byte(gc.ServiceAccount), storage.ScopeFullControl)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create credentials from JSON")
		}
		opts = append(opts, option.WithCredentials(credentials))
	}

	opts = append(opts,
		option.WithUserAgent(fmt.Sprintf("thanos-%s/%s (%s)", component, version.Version, runtime.Version())),
	)

	gcsClient, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	bkt := &Bucket{
		logger: logger,
		bkt:    gcsClient.Bucket(gc.Bucket),
		closer: gcsClient,
		name:   gc.Bucket,
	}
	return bkt, nil
}

// Name returns the bucket name for gcs.
func (b *Bucket) Name() string {
	return b.name
}

func (b *Bucket) SupportedIterOptions() []objstore.IterOptionType {
	return []objstore.IterOptionType{objstore.Recursive, objstore.UpdatedAt}
}

func (b *Bucket) IterWithAttributes(ctx context.Context, dir string, f func(attrs objstore.IterObjectAttributes) error, options ...objstore.IterOption) error {
	for _, opt := range options {
		if !slices.Contains(b.SupportedIterOptions(), opt.Type) {
			return fmt.Errorf("%w: %v", objstore.ErrOptionNotSupported, opt.Type)
		}
	}

	// Ensure the object name actually ends with a dir suffix. Otherwise we'll just iterate the
	// object itself as one prefix item.
	if dir != "" {
		dir = strings.TrimSuffix(dir, DirDelim) + DirDelim
	}

	appliedOpts := objstore.ApplyIterOptions(options...)

	// If recursive iteration is enabled we should pass an empty delimiter.
	delimiter := DirDelim
	if appliedOpts.Recursive {
		delimiter = ""
	}

	it := b.bkt.Objects(ctx, &storage.Query{
		Prefix:    dir,
		Delimiter: delimiter,
	})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		attrs, err := it.Next()
		if err == iterator.Done {
			return nil
		}
		if err != nil {
			return err
		}

		objAttrs := objstore.IterObjectAttributes{Name: attrs.Prefix + attrs.Name}
		if appliedOpts.LastModified {
			objAttrs.SetLastModified(attrs.Updated)
		}
		if err := f(objAttrs); err != nil {
			return err
		}
	}
}

// Iter calls f for each entry in the given directory. The argument to f is the full
// object name including the prefix of the inspected directory.
func (b *Bucket) Iter(ctx context.Context, dir string, f func(string) error, opts ...objstore.IterOption) error {
	// Only include recursive option since attributes are not used in this method.
	var filteredOpts []objstore.IterOption
	for _, opt := range opts {
		if opt.Type == objstore.Recursive {
			filteredOpts = append(filteredOpts, opt)
			break
		}
	}

	return b.IterWithAttributes(ctx, dir, func(attrs objstore.IterObjectAttributes) error {
		return f(attrs.Name)
	}, filteredOpts...)
}

// Get returns a reader for the given object name.
func (b *Bucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	return b.bkt.Object(name).NewReader(ctx)
}

// GetRange returns a new range reader for the given object name and range.
func (b *Bucket) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	return b.bkt.Object(name).NewRangeReader(ctx, off, length)
}

// Attributes returns information about the specified object.
func (b *Bucket) Attributes(ctx context.Context, name string) (objstore.ObjectAttributes, error) {
	attrs, err := b.bkt.Object(name).Attrs(ctx)
	if err != nil {
		return objstore.ObjectAttributes{}, err
	}

	return objstore.ObjectAttributes{
		Size:         attrs.Size,
		LastModified: attrs.Updated,
	}, nil
}

// Handle returns the underlying GCS bucket handle.
// Used for testing purposes (we return handle, so it is not instrumented).
func (b *Bucket) Handle() *storage.BucketHandle {
	return b.bkt
}

// Exists checks if the given object exists.
func (b *Bucket) Exists(ctx context.Context, name string) (bool, error) {
	if _, err := b.bkt.Object(name).Attrs(ctx); err == nil {
		return true, nil
	} else if err != storage.ErrObjectNotExist {
		return false, err
	}
	return false, nil
}

// Upload writes the file specified in src to remote GCS location specified as target.
func (b *Bucket) Upload(ctx context.Context, name string, r io.Reader) error {
	w := b.bkt.Object(name).NewWriter(ctx)

	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	return w.Close()
}

// Delete removes the object with the given name.
func (b *Bucket) Delete(ctx context.Context, name string) error {
	return b.bkt.Object(name).Delete(ctx)
}

// IsObjNotFoundErr returns true if error means that object is not found. Relevant to Get operations.
func (b *Bucket) IsObjNotFoundErr(err error) bool {
	return errors.Is(err, storage.ErrObjectNotExist)
}

// IsCustomerManagedKeyError returns true if the permissions for key used to encrypt the object was revoked.
func (b *Bucket) IsCustomerManagedKeyError(_ error) bool {
	return false
}

func (b *Bucket) Close() error {
	return b.closer.Close()
}

// NewTestBucket creates test bkt client that before returning creates temporary bucket.
// In a close function it empties and deletes the bucket.
func NewTestBucket(t testing.TB, project string) (objstore.Bucket, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gTestConfig := Config{
		Bucket: objstore.CreateTemporaryTestBucketName(t),
	}

	bc, err := yaml.Marshal(gTestConfig)
	if err != nil {
		return nil, nil, err
	}

	b, err := NewBucket(ctx, log.NewNopLogger(), bc, "thanos-e2e-test")
	if err != nil {
		return nil, nil, err
	}

	if err = b.bkt.Create(ctx, project, nil); err != nil {
		_ = b.Close()
		return nil, nil, err
	}

	t.Log("created temporary GCS bucket for GCS tests with name", b.name, "in project", project)
	return b, func() {
		objstore.EmptyBucket(t, ctx, b)
		if err := b.bkt.Delete(ctx); err != nil {
			t.Logf("deleting bucket failed: %s", err)
		}
		if err := b.Close(); err != nil {
			t.Logf("closing bucket failed: %s", err)
		}
	}, nil
}
