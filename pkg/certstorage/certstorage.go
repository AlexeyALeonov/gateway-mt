// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

// Package certstorage provides implementations of certmagic's Storage interface.
package certstorage

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/gateway-mt/pkg/gcslock"
	"storj.io/gateway-mt/pkg/gcslock/gcsops"
)

var (
	// Error is the error class for this package.
	Error = errs.Class("certstorage")
	mon   = monkit.Package()
)

// GCS implements certmagic's Storage interface on top of Google Cloud Storage.
type GCS struct {
	logger *zap.Logger
	client *gcsops.Client

	bucket string
	prefix string

	locks map[string]*gcslock.Mutex
	mu    sync.Mutex
}

// NewGCS returns initialized GCS.
func NewGCS(ctx context.Context, logger *zap.Logger, jsonKey []byte, path string) (_ *GCS, err error) {
	bucket, prefix, found := strings.Cut(path, "/")
	if found {
		// Make sure prefix has trailing slash
		prefix = strings.TrimSuffix(prefix, "/") + "/"
	}

	gcs := &GCS{
		logger: logger,
		bucket: bucket,
		prefix: prefix,
		locks:  make(map[string]*gcslock.Mutex),
	}

	gcs.client, err = gcsops.NewClient(ctx, jsonKey)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	if err = gcs.client.TestPermissions(ctx, bucket); err != nil {
		return nil, Error.Wrap(err)
	}

	return gcs, nil
}

var _ certmagic.Storage = (*GCS)(nil) // make sure GCS implements certmagic.Storage

// Lock implements certmagics's Storage interface.
func (gcs *GCS) Lock(ctx context.Context, name string) (err error) {
	defer mon.Task()(&ctx)(&err)

	gcs.mu.Lock()
	lock, ok := gcs.locks[name]
	if !ok {
		m, err := gcslock.NewMutex(ctx, gcslock.Options{
			Name:   gcs.prefix + name,
			Bucket: gcs.bucket,
			Logger: gcs.logger.Named("distributed lock/" + name).Sugar(),
			Client: gcs.client,
		})
		if err != nil {
			gcs.mu.Unlock()
			return Error.Wrap(err)
		}
		gcs.locks[name], lock = m, m
	}
	gcs.mu.Unlock()
	mon.Event("certstorage_lockcache", monkit.NewSeriesTag("hit", strconv.FormatBool(ok)))
	return Error.Wrap(lock.Lock(ctx))
}

// Unlock implements certmagics's Storage interface.
func (gcs *GCS) Unlock(ctx context.Context, name string) (err error) {
	defer mon.Task()(&ctx)(&err)

	gcs.mu.Lock()
	lock, ok := gcs.locks[name]
	if !ok {
		gcs.mu.Unlock()
		mon.Event("certstorage_mutex_not_exists")
		return Error.New("mutex for %s not exists", name)
	}
	gcs.mu.Unlock()
	return Error.Wrap(lock.Unlock(ctx))
}

// Store implements certmagics's Storage interface.
func (gcs *GCS) Store(ctx context.Context, key string, value []byte) error {
	k := gcs.prefix + key
	gcs.logger.Debug("store", zap.String("bucket", gcs.bucket), zap.String("key", k))

	return Error.Wrap(gcs.client.Upload(ctx, nil, gcs.bucket, k, bytes.NewReader(value)))
}

// Load implements certmagics's Storage interface.
func (gcs *GCS) Load(ctx context.Context, key string) (_ []byte, err error) {
	defer mon.Task()(&ctx)(&err)
	k := gcs.prefix + key

	gcs.logger.Debug("load", zap.String("bucket", gcs.bucket), zap.String("key", k))

	rc, err := gcs.client.Download(ctx, gcs.bucket, k)
	if err != nil {
		if errs.Is(err, gcsops.ErrNotFound) {
			return nil, Error.Wrap(fs.ErrNotExist)
		}
		return nil, Error.Wrap(err)
	}
	defer func() { err = Error.Wrap(errs.Combine(err, rc.Close())) }()

	return io.ReadAll(rc)
}

// Delete implements certmagics's Storage interface.
func (gcs *GCS) Delete(ctx context.Context, key string) (err error) {
	defer mon.Task()(&ctx)(&err)
	k := gcs.prefix + key

	gcs.logger.Debug("delete", zap.String("bucket", gcs.bucket), zap.String("key", k))

	err = gcs.client.Delete(ctx, nil, gcs.bucket, k)
	if errs.Is(err, gcsops.ErrNotFound) {
		return Error.Wrap(fs.ErrNotExist)
	}
	return Error.Wrap(err)
}

// Exists implements certmagics's Storage interface.
func (gcs *GCS) Exists(ctx context.Context, key string) bool {
	var err error
	k := gcs.prefix + key

	defer mon.Task()(&ctx)(&err)

	_, err = gcs.client.Stat(ctx, gcs.bucket, k)
	return err == nil
}

// List implements certmagics's Storage interface.
func (gcs *GCS) List(ctx context.Context, prefix string, recursive bool) (_ []string, err error) {
	defer mon.Task()(&ctx)(&err)
	p := gcs.prefix + prefix

	gcs.logger.Debug("list", zap.String("bucket", gcs.bucket), zap.String("prefix", p), zap.Bool("recursive", recursive))

	r, err := gcs.client.List(ctx, gcs.bucket, p, recursive)
	return r, Error.Wrap(err)
}

// Stat implements certmagics's Storage interface.
func (gcs *GCS) Stat(ctx context.Context, key string) (_ certmagic.KeyInfo, err error) {
	defer mon.Task()(&ctx)(&err)
	k := gcs.prefix + key

	var keyInfo certmagic.KeyInfo

	gcs.logger.Debug("stat", zap.String("bucket", gcs.bucket), zap.String("key", k))

	headers, err := gcs.client.Stat(ctx, gcs.bucket, k)
	if err != nil {
		if errs.Is(err, gcsops.ErrNotFound) {
			return keyInfo, Error.Wrap(fs.ErrNotExist)
		}
		return keyInfo, Error.Wrap(err)
	}

	keyInfo.Key = k
	keyInfo.IsTerminal = true // GCS returns 404 if querying prefix

	keyInfo.Modified, err = time.Parse(time.RFC1123, headers.Get("last-modified"))
	if err != nil {
		return keyInfo, Error.Wrap(err)
	}
	keyInfo.Size, err = strconv.ParseInt(headers.Get("content-length"), 10, 64)
	if err != nil {
		return keyInfo, Error.Wrap(err)
	}

	return keyInfo, nil
}
