// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package revproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/creachadair/atomicfile"
	"github.com/creachadair/scheddle"
	"github.com/creachadair/taskgroup"
	"gocloud.dev/blob"
)

// cacheLoadLocal reads cached headers and body from the local cache.
func (s *Server) cacheLoadLocal(hash string) ([]byte, http.Header, error) {
	data, err := os.ReadFile(s.makePath(hash))
	if err != nil {
		return nil, nil, err
	}
	return parseCacheObject(data)
}

// cacheStoreLocal writes the contents of body to the local cache.
//
// The file format is a plain-text section at the top recording a subset of the
// response headers, followed by "\n\n", followed by the response body.
func (s *Server) cacheStoreLocal(hash string, hdr http.Header, body []byte) error {
	path := s.makePath(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.Tx(s.makePath(hash), 0644, func(f *atomicfile.File) error {
		return writeCacheObject(f, hdr, body)
	})
}

// cacheLoadS3 reads cached headers and body from the remote S3 cache.
func (s *Server) cacheLoadS3(ctx context.Context, hash string) ([]byte, http.Header, error) {
	data, err := s.Bucket.ReadAll(ctx, s.makeKey(hash))
	if err != nil {
		return nil, nil, err
	}
	return parseCacheObject(data)
}

// cacheStoreS3 returns a task that writes the contents of body to the remote
// S3 cache.
func (s *Server) cacheStoreS3(hash string, hdr http.Header, body []byte) taskgroup.Task {
	var buf bytes.Buffer
	writeCacheObject(&buf, hdr, body)
	nb := buf.Len()
	return func() error {
		sctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()

		w, err := s.Bucket.NewWriter(sctx, s.makeKey(hash), &blob.WriterOptions{})
		if err != nil {
			s.logf("[s3] put %q failed: %v", hash, err)
			s.rspPushError.Add(1)
			return err
		}
		defer w.Close()

		_, err = io.Copy(w, &buf)
		if err != nil {
			s.logf("[s3] put %q failed: %v", hash, err)
			s.rspPushError.Add(1)
			return err
		}

		s.rspPush.Add(1)
		s.rspPushBytes.Add(int64(nb))
		return nil
	}
}

// cacheLoadMemory reads cached headers and body from the memory cache.
func (s *Server) cacheLoadMemory(hash string) ([]byte, http.Header, error) {
	e, ok := s.mcache.Get(hash)
	if !ok {
		return nil, nil, fs.ErrNotExist
	}
	return e.body, e.header, nil
}

// cacheStoreMemory writes the contents of body to the memory cache.
func (s *Server) cacheStoreMemory(hash string, maxAge time.Duration, hdr http.Header, body []byte) {
	s.mcache.Put(hash, memCacheEntry{
		header: trimCacheHeader(hdr),
		body:   body,
	})
	s.expire.After(maxAge, scheddle.Run(func() {
		s.mcache.Remove(hash)
	}))
}

var keepHeader = []string{
	"Cache-Control", "Content-Type", "Date", "Etag",
}

func trimCacheHeader(h http.Header) http.Header {
	out := make(http.Header)
	for _, name := range keepHeader {
		if v := h.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	return out
}

// parseCacheDbject parses cached object data to extract the body and headers.
func parseCacheObject(data []byte) ([]byte, http.Header, error) {
	hdr, rest, ok := bytes.Cut(data, []byte("\n\n"))
	if !ok {
		return nil, nil, errors.New("invalid cache object: missing header")
	}
	h := make(http.Header)
	for _, line := range strings.Split(string(hdr), "\n") {
		name, value, ok := strings.Cut(line, ": ")
		if ok {
			h.Add(name, value)
		}
	}
	return rest, h, nil
}

// writeCacheObject writes the specified response data into a cache object at w.
func writeCacheObject(w io.Writer, h http.Header, body []byte) error {
	hprintf(w, h, "Content-Type", "application/octet-stream")
	hprintf(w, h, "Date", "")
	hprintf(w, h, "Etag", "")
	fmt.Fprint(w, "\n")
	_, err := w.Write(body)
	return err
}

func hprintf(w io.Writer, h http.Header, name, fallback string) {
	if v := h.Get(name); v != "" {
		fmt.Fprintf(w, "%s: %s\n", name, v)
	} else if fallback != "" {
		fmt.Fprintf(w, "%s: %s\n", name, fallback)
	}
}

// setXCacheInfo adds cache-specific headers to h.
func setXCacheInfo(h http.Header, result, hash string) {
	h.Set("X-Cache", result)
	if hash != "" {
		h.Set("X-Cache-Id", hash[:12])
	}
}

// memCacheEntry is the format of entries in the memory cache.
type memCacheEntry struct {
	header http.Header
	body   []byte
}

func entrySize(e memCacheEntry) int64 { return int64(len(e.body)) }
