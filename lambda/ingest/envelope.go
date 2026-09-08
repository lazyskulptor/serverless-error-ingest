// Package main implements the Sentry-compatible ingest Lambda handler.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	maxEnvelopeItems = 100
	maxHeaderBytes   = 16 << 10
	maxItemBytes     = 1 << 20
)

// EnvelopeHeader is the first line of an envelope: compact JSON.
// Only fields this service cares about are typed; the rest is preserved.
type EnvelopeHeader struct {
	DSN string          `json:"dsn"`
	Raw json.RawMessage `json:"-"`
}

// ItemHeader is a single envelope item header line: compact JSON.
// `length` is the exact byte length of the following payload when present.
type ItemHeader struct {
	Type   string          `json:"type"`
	Length *int64          `json:"length"`
	Raw    json.RawMessage `json:"-"`
}

// Item is one envelope item: header + raw payload bytes.
type Item struct {
	Header  ItemHeader
	Payload []byte
}

// Envelope is a parsed envelope: header line + items.
type Envelope struct {
	Header EnvelopeHeader
	Items  []Item
}

// readLine reads bytes up to (not including) the next '\n'.
//
// Per the exact wire contract, '\n' (ASCII 10) is the only newline; a '\r'
// immediately before '\n' is NOT a newline and belongs to the line content.
//
// Returns:
//   - line: content without the trailing '\n'
//   - terminatedByNewline: true if the line ended with '\n' (vs EOF)
//   - err: nil normally; io.EOF only when no bytes were read at all
func readLine(br *bufio.Reader) (line []byte, terminatedByNewline bool, err error) {
	line, err = br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, false, errors.New("header or implicit payload line exceeds maximum size")
	}
	if len(line) == 0 {
		if err != nil {
			return nil, false, err // io.EOF (clean end) or real error
		}
		return nil, false, nil
	}
	if line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
		terminatedByNewline = true
	}
	if err == io.EOF {
		err = nil // content was read; EOF just means no trailing newline
	}
	return line, terminatedByNewline, err
}

// ParseEnvelope parses an envelope body using the exact grammar:
//
//	Envelope = Headers { "\n" Item } [ "\n" ] ;
//	Item     = Headers "\n" Payload ;
//
// When an item header specifies `length`, exactly that many bytes are read for
// the payload (embedded '\n' bytes belong to the payload); the next byte must
// be '\n' or EOF. When `length` is absent, the payload is the next line
// (terminated by '\n' or EOF). This never approximates with a naive
// newline-split.
func ParseEnvelope(r io.Reader) (*Envelope, error) {
	br := bufio.NewReaderSize(r, maxHeaderBytes+1)

	// Envelope header line: exactly one line of compact JSON.
	headerLine, _, err := readLine(br)
	if err != nil {
		return nil, fmt.Errorf("reading envelope header: %w", err)
	}
	if len(headerLine) == 0 {
		return nil, errors.New("envelope header line is empty")
	}
	var envHeader EnvelopeHeader
	if err := json.Unmarshal(headerLine, &envHeader); err != nil {
		return nil, fmt.Errorf("parsing envelope header: %w", err)
	}
	envHeader.Raw = append(json.RawMessage(nil), headerLine...)

	env := &Envelope{Header: envHeader}

	for {
		if len(env.Items) >= maxEnvelopeItems {
			return nil, fmt.Errorf("envelope exceeds maximum item count of %d", maxEnvelopeItems)
		}
		itemHeaderLine, _, err := readLine(br)
		if errors.Is(err, io.EOF) {
			break // clean end of envelope (may have zero items)
		}
		if err != nil {
			return nil, fmt.Errorf("reading item header: %w", err)
		}
		if len(itemHeaderLine) == 0 {
			// Grammar: Envelope = Headers { "\n" Item } [ "\n" ]. An empty
			// line is only valid as the optional trailing newline after the
			// last item; if any content follows, it is a malformed empty
			// item header.
			next, _, err := readLine(br)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading item header: %w", err)
			}
			if len(next) == 0 {
				return nil, errors.New("empty item header line")
			}
			return nil, errors.New("empty item header line before further content")
		}

		var ih ItemHeader
		if err := json.Unmarshal(itemHeaderLine, &ih); err != nil {
			return nil, fmt.Errorf("parsing item header: %w", err)
		}
		ih.Raw = append(json.RawMessage(nil), itemHeaderLine...)

		var payload []byte
		if ih.Length != nil {
			n := *ih.Length
			if n < 0 {
				return nil, errors.New("item header has negative length")
			}
			if n > maxItemBytes {
				return nil, fmt.Errorf("item payload exceeds maximum size of %d bytes", maxItemBytes)
			}
			payload = make([]byte, n)
			if _, err := io.ReadFull(br, payload); err != nil {
				return nil, fmt.Errorf("reading %d-byte payload: %w", n, err)
			}
			// The next byte must be '\n' or EOF; anything else is malformed.
			next, err := br.ReadByte()
			if err == nil && next != '\n' {
				return nil, fmt.Errorf("payload of length %d not followed by newline (got byte %q)", n, next)
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("after length-prefixed payload: %w", err)
			}
		} else {
			// Implicit termination: payload is the next line (up to '\n' or EOF).
			payload, _, err = readLine(br)
			if err != nil {
				return nil, fmt.Errorf("reading implicit-length payload: %w", err)
			}
			if len(payload) > maxItemBytes {
				return nil, fmt.Errorf("item payload exceeds maximum size of %d bytes", maxItemBytes)
			}
		}

		env.Items = append(env.Items, Item{Header: ih, Payload: payload})
	}

	return env, nil
}
