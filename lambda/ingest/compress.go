package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// decompressBody decodes the request body according to the Content-Encoding
// header. Supported encodings (per the wire contract §6): gzip, deflate, br
// (Brotli), zstd; empty/"identity" pass through. Any other value is an error
// (caller responds 400).
func decompressBody(contentEncoding string, body []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
		return decompressGzip(body)
	case "deflate":
		return decompressDeflate(body)
	case "br":
		return decompressBrotli(body)
	case "zstd":
		return decompressZstd(body)
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", contentEncoding)
	}
}

func decompressGzip(body []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

func decompressDeflate(body []byte) ([]byte, error) {
	// HTTP "deflate" is ambiguous: the zlib wrapper (RFC 1950) is the standard
	// interpretation, but some clients send raw DEFLATE (RFC 1951). Detect by
	// the zlib header (first byte 0x78) and fall back to raw flate.
	if len(body) >= 2 && body[0] == 0x78 {
		r, err := zlib.NewReader(bytes.NewReader(body))
		if err == nil {
			defer r.Close()
			return io.ReadAll(r)
		}
	}
	fr := flate.NewReader(bytes.NewReader(body))
	defer fr.Close()
	out, err := io.ReadAll(fr)
	if err != nil {
		return nil, fmt.Errorf("deflate: %w", err)
	}
	return out, nil
}

func decompressBrotli(body []byte) ([]byte, error) {
	out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
	if err != nil {
		return nil, fmt.Errorf("br: %w", err)
	}
	return out, nil
}

func decompressZstd(body []byte) ([]byte, error) {
	r, err := zstd.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("zstd: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("zstd: %w", err)
	}
	return out, nil
}
