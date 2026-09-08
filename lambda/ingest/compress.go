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
var errDecompressedBodyTooLarge = fmt.Errorf("decompressed payload exceeds maximum size")

func decompressBody(contentEncoding string, body []byte, maxBytes int64) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		if maxBytes > 0 && int64(len(body)) > maxBytes {
			return nil, errDecompressedBodyTooLarge
		}
		return body, nil
	case "gzip":
		return decompressGzip(body, maxBytes)
	case "deflate":
		return decompressDeflate(body, maxBytes)
	case "br":
		return decompressBrotli(body, maxBytes)
	case "zstd":
		return decompressZstd(body, maxBytes)
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", contentEncoding)
	}
}

func readLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(r)
	}
	out, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxBytes {
		return nil, errDecompressedBodyTooLarge
	}
	return out, nil
}

func decompressGzip(body []byte, maxBytes int64) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer r.Close()
	return readLimited(r, maxBytes)
}

func decompressDeflate(body []byte, maxBytes int64) ([]byte, error) {
	// HTTP "deflate" is ambiguous: the zlib wrapper (RFC 1950) is the standard
	// interpretation, but some clients send raw DEFLATE (RFC 1951). Detect by
	// the zlib header (first byte 0x78) and fall back to raw flate.
	if len(body) >= 2 && body[0] == 0x78 {
		r, err := zlib.NewReader(bytes.NewReader(body))
		if err == nil {
			defer r.Close()
			return readLimited(r, maxBytes)
		}
	}
	fr := flate.NewReader(bytes.NewReader(body))
	defer fr.Close()
	out, err := readLimited(fr, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("deflate: %w", err)
	}
	return out, nil
}

func decompressBrotli(body []byte, maxBytes int64) ([]byte, error) {
	out, err := readLimited(brotli.NewReader(bytes.NewReader(body)), maxBytes)
	if err != nil {
		return nil, fmt.Errorf("br: %w", err)
	}
	return out, nil
}

func decompressZstd(body []byte, maxBytes int64) ([]byte, error) {
	r, err := zstd.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("zstd: %w", err)
	}
	defer r.Close()
	out, err := readLimited(r, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("zstd: %w", err)
	}
	return out, nil
}
