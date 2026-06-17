package s3compat

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// maxObjectRanges caps the number of byte ranges honoured in a single
// multi-range request. A multipart/byteranges response repeats a part
// header (~80 bytes) per range, so an unbounded range count would let a
// client amplify a tiny object into an arbitrarily large response. A
// request beyond this cap is served as the whole object (200), which is
// how Apache's default MaxRanges policy degrades.
const maxObjectRanges = 100

// parseObjectRanges parses the request's Range header against the known
// object size and classifies it into exactly one disposition:
//
//   - err != nil       : the header is present but malformed or
//     unsatisfiable — the caller responds 416.
//   - single != nil    : one satisfiable range — 206 + Content-Range.
//   - multi  != nil    : several satisfiable ranges worth a
//     multipart/byteranges response — 206.
//   - all nil, err nil : no Range header, OR a multi-range request whose
//     combined span meets/exceeds the object (or
//     exceeds maxObjectRanges) — serve the whole
//     object (200), matching net/http's ServeContent.
//
// At most one of single / multi is non-nil. The single range is returned
// exactly as parseHTTPRange produced it (an open-ended "bytes=start-"
// keeps End == -1) so the single-range read paths are unchanged. The
// multi ranges are resolved to concrete, in-bounds Start/End so callers
// can slice and format Content-Range without further bounds work.
func parseObjectRanges(r *http.Request, size int64) (single *providers.ByteRange, multi []providers.ByteRange, err error) {
	hdr := r.Header.Get("Range")
	if hdr == "" {
		return nil, nil, nil
	}
	if !strings.Contains(hdr, ",") {
		rng, perr := parseHTTPRange(hdr, size)
		if perr != nil {
			return nil, nil, perr
		}
		return rng, nil, nil
	}
	ranges, perr := parseHTTPRangeList(hdr, size)
	if perr != nil {
		return nil, nil, perr
	}
	// A degenerate one-element list ("bytes=0-9,") collapses to a
	// single-range response.
	if len(ranges) == 1 {
		rng := ranges[0]
		return &rng, nil, nil
	}
	// Guard against response amplification: too many ranges, or a
	// combined span that meets/exceeds the object, is served whole.
	if len(ranges) > maxObjectRanges || sumRangeSizes(ranges) >= size {
		return nil, nil, nil
	}
	return nil, ranges, nil
}

// parseHTTPRangeList parses a multi-range "bytes=a-b,c-d,…" header into
// resolved, in-bounds ranges. Every segment must be individually
// satisfiable; an empty or malformed segment fails the whole header (the
// caller responds 416), matching the single-range parser's strictness.
// At least one range is returned on success.
func parseHTTPRangeList(h string, size int64) ([]providers.ByteRange, error) {
	if !strings.HasPrefix(h, "bytes=") {
		return nil, fmt.Errorf("invalid range header %q", h)
	}
	spec := strings.TrimPrefix(h, "bytes=")
	var out []providers.ByteRange
	for _, seg := range strings.Split(spec, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		rng, err := parseHTTPRange("bytes="+seg, size)
		if err != nil {
			return nil, err
		}
		resolveRangeEnd(rng, size)
		out = append(out, *rng)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no satisfiable ranges in %q", h)
	}
	return out, nil
}

// resolveRangeEnd fills an open-ended range ("bytes=start-") with the
// object's final byte so downstream slicing and Content-Range formatting
// see a concrete endpoint.
func resolveRangeEnd(r *providers.ByteRange, size int64) {
	if r.End < 0 || r.End >= size {
		r.End = size - 1
	}
}

func sumRangeSizes(ranges []providers.ByteRange) int64 {
	var n int64
	for _, r := range ranges {
		n += r.End - r.Start + 1
	}
	return n
}

// newRangeBoundary returns a fixed-length, ASCII-safe multipart boundary.
// The length is constant, so multipartByteRangesLength yields the same
// Content-Length for a GET and a matching HEAD even though each response
// mints a fresh boundary value.
func newRangeBoundary() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// rangePartHeader is the MIME part preamble for one byte range in a
// multipart/byteranges body: the boundary delimiter followed by the
// object's Content-Type and the part's Content-Range, terminated by the
// blank line that separates headers from the body.
func rangePartHeader(boundary, contentType string, rng providers.ByteRange, total int64) string {
	return "--" + boundary + "\r\n" +
		"Content-Type: " + contentType + "\r\n" +
		"Content-Range: bytes " + strconv.FormatInt(rng.Start, 10) + "-" +
		strconv.FormatInt(rng.End, 10) + "/" + strconv.FormatInt(total, 10) + "\r\n\r\n"
}

// multipartByteRangesLength computes the exact Content-Length of the
// multipart/byteranges body for the given ranges, so the response can be
// streamed without buffering. Each part contributes its preamble, its
// payload (End-Start+1 bytes) and a trailing CRLF; the body ends with
// the closing boundary delimiter.
func multipartByteRangesLength(ranges []providers.ByteRange, total int64, boundary, contentType string) int64 {
	var n int64
	for _, rng := range ranges {
		n += int64(len(rangePartHeader(boundary, contentType, rng, total)))
		n += rng.End - rng.Start + 1
		n += 2 // CRLF after each part body
	}
	n += int64(len("--" + boundary + "--\r\n"))
	return n
}

// setMultipartByteRangesHeaders sets the multipart/byteranges Content-Type
// and the analytic Content-Length without writing a body. HEAD uses it so
// a pre-flight probe sees exactly the headers the matching GET would emit.
func setMultipartByteRangesHeaders(w http.ResponseWriter, ranges []providers.ByteRange, total int64, objectContentType string) {
	boundary := newRangeBoundary()
	w.Header().Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	w.Header().Set("Content-Length", strconv.FormatInt(multipartByteRangesLength(ranges, total, boundary, objectContentType), 10))
}

// writeMultipartByteRanges streams a 206 multipart/byteranges response.
// Each part carries the object's Content-Type and a Content-Range; the
// bodies are produced by part(rng), which returns a reader for the
// resolved range. The first range is fetched before the status line is
// committed so an unreachable backend surfaces as a clean caller-written
// error rather than a truncated 206 — the returned committed flag reports
// whether the status was already sent when err is non-nil. Content-Length
// is computed analytically, so the response is never buffered. The
// returned count is payload bytes (excluding MIME framing) for billing.
func writeMultipartByteRanges(
	w http.ResponseWriter,
	ranges []providers.ByteRange,
	total int64,
	objectContentType string,
	part func(rng providers.ByteRange) (io.ReadCloser, error),
) (payload int64, committed bool, err error) {
	first, ferr := part(ranges[0])
	if ferr != nil {
		return 0, false, ferr
	}
	boundary := newRangeBoundary()
	w.Header().Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	w.Header().Set("Content-Length", strconv.FormatInt(multipartByteRangesLength(ranges, total, boundary, objectContentType), 10))
	w.WriteHeader(http.StatusPartialContent)

	for i := range ranges {
		rng := ranges[i]
		rc := first
		if i > 0 {
			rc, err = part(rng)
			if err != nil {
				return payload, true, err
			}
		}
		if _, werr := io.WriteString(w, rangePartHeader(boundary, objectContentType, rng, total)); werr != nil {
			_ = rc.Close()
			return payload, true, werr
		}
		n, cerr := io.Copy(w, rc)
		_ = rc.Close()
		payload += n
		if cerr != nil {
			return payload, true, cerr
		}
		if _, werr := io.WriteString(w, "\r\n"); werr != nil {
			return payload, true, werr
		}
	}
	if _, werr := io.WriteString(w, "--"+boundary+"--\r\n"); werr != nil {
		return payload, true, werr
	}
	return payload, true, nil
}

// writeMultipartByteRangesFromBuffer serves a multipart/byteranges
// response whose parts are sub-slices of an already-materialised buffer
// (the erasure-coded, multipart and gateway-decrypted read paths all hold
// the full plaintext in memory). parseObjectRanges resolves the ranges
// against the manifest's recorded ObjectSize, so a buffer that came back
// shorter than the manifest claims — a reconstruction / decryption size
// mismatch or a corrupt manifest — would otherwise panic on the slice.
// Each range is therefore re-checked against the actual buffer length
// before any byte is written; a range that runs past the buffer returns
// an error WITHOUT committing a response so the caller can fail the GET
// as a 502, mirroring the bounds clamp the single-range buffer paths
// apply before slicing. Once the ranges are validated the in-memory
// reader never errors.
func writeMultipartByteRangesFromBuffer(w http.ResponseWriter, ranges []providers.ByteRange, total int64, objectContentType string, buf []byte) (int64, error) {
	bufLen := int64(len(buf))
	for _, rng := range ranges {
		if rng.Start < 0 || rng.End < rng.Start || rng.End >= bufLen {
			return 0, fmt.Errorf("range %d-%d out of bounds for %d-byte buffer", rng.Start, rng.End, bufLen)
		}
	}
	payload, _, _ := writeMultipartByteRanges(w, ranges, total, objectContentType, func(rng providers.ByteRange) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf[rng.Start : rng.End+1])), nil
	})
	return payload, nil
}
