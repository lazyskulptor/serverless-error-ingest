package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zlibBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func rawFlateBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func brBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecompressAllEncodings(t *testing.T) {
	original := []byte("{\"event_id\":\"abc\",\"message\":\"hello world\"}")

	cases := []struct {
		name string
		enc  string
		body []byte
	}{
		{"identity", "", original},
		{"identity-explicit", "identity", original},
		{"gzip", "gzip", gzipBytes(t, original)},
		{"deflate-zlib", "deflate", zlibBytes(t, original)},
		{"deflate-raw", "deflate", rawFlateBytes(t, original)},
		{"br", "br", brBytes(t, original)},
		{"zstd", "zstd", zstdBytes(t, original)},
		{"case-insensitive", "GZIP", gzipBytes(t, original)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decompressBody(c.enc, c.body)
			if err != nil {
				t.Fatalf("decompressBody: %v", err)
			}
			if !bytes.Equal(got, original) {
				t.Errorf("got %q, want %q", got, original)
			}
		})
	}
}

func TestDecompressUnknownEncoding(t *testing.T) {
	if _, err := decompressBody("lz4", []byte("data")); err == nil {
		t.Fatal("expected error for unknown encoding")
	}
}

func TestDecompressGzipCorrupt(t *testing.T) {
	if _, err := decompressBody("gzip", []byte("not-gzip")); err == nil {
		t.Fatal("expected error for corrupt gzip")
	}
}
