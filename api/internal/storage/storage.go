// Package storage stores uploaded source files in object storage. Cloud Run instances are
// stateless, so production uses GCS; the local driver exists for development only.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
)

type PutResult struct {
	Size     int64
	Checksum string // hex SHA-256 of the stored bytes
}

type ObjectStore interface {
	Put(ctx context.Context, key string, r io.Reader, contentType string) (PutResult, error)
	Delete(ctx context.Context, key string) error
}

// hashingReader computes size and SHA-256 while the object streams to its destination.
type hashingReader struct {
	r    io.Reader
	h    hashState
	size int64
}

type hashState interface {
	io.Writer
	Sum([]byte) []byte
}

func newHashingReader(r io.Reader) *hashingReader { return &hashingReader{r: r, h: sha256.New()} }

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.h.Write(p[:n])
		h.size += int64(n)
	}
	return n, err
}

func (h *hashingReader) result() PutResult {
	return PutResult{Size: h.size, Checksum: hex.EncodeToString(h.h.Sum(nil))}
}

// ValidKey rejects keys that could escape the storage root.
func ValidKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Local writes objects below a directory. Not for production use.
type Local struct{ Root string }

func (l *Local) path(key string) (string, error) {
	if !ValidKey(key) {
		return "", errors.New("storage: invalid key")
	}
	return filepath.Join(l.Root, filepath.FromSlash(key)), nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader, _ string) (PutResult, error) {
	p, err := l.path(key)
	if err != nil {
		return PutResult{}, err
	}
	// Group-readable: the rag service reads these files and, in docker compose, runs as a
	// different user that shares the api's group (group_add in docker-compose.yml).
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return PutResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return PutResult{}, err
	}
	if err := tmp.Chmod(0o640); err != nil { // CreateTemp creates files as 0600
		tmp.Close()
		os.Remove(tmp.Name())
		return PutResult{}, err
	}
	hr := newHashingReader(r)
	if _, err := io.Copy(tmp, hr); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return PutResult{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return PutResult{}, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return PutResult{}, err
	}
	return hr.result(), nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// GCS stores objects in a Google Cloud Storage bucket using Application Default Credentials.
type GCS struct {
	client *storage.Client
	bucket string
}

func NewGCS(ctx context.Context, bucket string) (*GCS, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs client: %w", err)
	}
	return &GCS{client: c, bucket: bucket}, nil
}

func (g *GCS) Put(ctx context.Context, key string, r io.Reader, contentType string) (PutResult, error) {
	if !ValidKey(key) {
		return PutResult{}, errors.New("storage: invalid key")
	}
	w := g.client.Bucket(g.bucket).Object(key).NewWriter(ctx)
	w.ContentType = contentType
	hr := newHashingReader(r)
	if _, err := io.Copy(w, hr); err != nil {
		w.Close()
		return PutResult{}, err
	}
	if err := w.Close(); err != nil {
		return PutResult{}, err
	}
	return hr.result(), nil
}

func (g *GCS) Delete(ctx context.Context, key string) error {
	err := g.client.Bucket(g.bucket).Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}

func (g *GCS) Close() error { return g.client.Close() }
