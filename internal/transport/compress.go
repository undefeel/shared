package transport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
)

const compressionSampleSize = 32 << 10

// EncodeBody streams a body to dst, using gzip only for relay traffic when the
// first sample shows at least 12.5% savings. maxPlain bounds bytes read from src.
// The returned hash is always over the uncompressed body.
func EncodeBody(dst io.Writer, src io.Reader, name string, relay bool, maxPlain int64) (encoding string, plainSize, wireSize int64, hash [32]byte, err error) {
	if maxPlain < 0 {
		return "", 0, 0, hash, errors.New("negative plain size limit")
	}
	sampleLimit := int64(compressionSampleSize)
	if maxPlain < sampleLimit {
		sampleLimit = maxPlain + 1
	}
	sample := make([]byte, int(sampleLimit))
	n, readErr := io.ReadFull(src, sample)
	sample = sample[:n]
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", 0, 0, hash, readErr
	}
	if int64(n) > maxPlain {
		return "", 0, 0, hash, errors.New("plain body exceeds size limit")
	}

	encoding = "identity"
	if relay && n >= 1024 && !knownIncompressible(name) {
		var probe bytes.Buffer
		zw, zerr := gzip.NewWriterLevel(&probe, gzip.BestSpeed)
		if zerr != nil {
			return "", 0, 0, hash, zerr
		}
		if _, zerr = zw.Write(sample); zerr == nil {
			zerr = zw.Close()
		} else {
			_ = zw.Close()
		}
		if zerr != nil {
			return "", 0, 0, hash, zerr
		}
		if int64(probe.Len())*8 <= int64(n)*7 {
			encoding = "gzip"
		}
	}

	plainHash := sha256.New()
	plain := io.MultiReader(bytes.NewReader(sample), src)
	limited := io.LimitReader(plain, maxPlain)
	counted := &countWriter{w: dst}
	var out io.Writer = counted
	var zw *gzip.Writer
	if encoding == "gzip" {
		zw, err = gzip.NewWriterLevel(counted, gzip.BestSpeed)
		if err != nil {
			return "", 0, 0, hash, err
		}
		out = zw
	}
	plainCount, copyErr := io.Copy(io.MultiWriter(out, plainHash), limited)
	if zw != nil {
		if closeErr := zw.Close(); copyErr == nil {
			copyErr = closeErr
		}
	}
	if copyErr != nil {
		return "", plainCount, counted.n, hash, copyErr
	}
	if plainCount == maxPlain {
		var extra [1]byte
		if extraN, extraErr := plain.Read(extra[:]); extraN != 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF)) {
			return "", plainCount, counted.n, hash, errors.New("plain body exceeds size limit")
		}
	}
	if plainCount > maxPlain {
		return "", plainCount, counted.n, hash, errors.New("plain body exceeds size limit")
	}
	plainSize, wireSize = plainCount, counted.n
	copy(hash[:], plainHash.Sum(nil))
	return encoding, plainSize, wireSize, hash, nil
}

// DecodeBody streams identity or gzip data to dst, rejecting excess decoded
// bytes, corrupt gzip trailers, concatenated members, and trailing bytes.
func DecodeBody(dst io.Writer, src io.Reader, encoding string, maxPlain int64) (plainSize int64, hash [32]byte, err error) {
	if maxPlain < 0 {
		return 0, hash, errors.New("negative plain size limit")
	}
	var body io.Reader = src
	var br *bufio.Reader
	switch encoding {
	case "identity":
	case "gzip":
		br = bufio.NewReader(src)
		zr, zerr := gzip.NewReader(br)
		if zerr != nil {
			return 0, hash, fmt.Errorf("open gzip body: %w", zerr)
		}
		zr.Multistream(false)
		defer zr.Close()
		body = zr
	default:
		return 0, hash, fmt.Errorf("unsupported body encoding %q", encoding)
	}

	digest := sha256.New()
	counted := &countWriter{w: dst}
	limited := io.LimitReader(body, maxPlain)
	n, copyErr := io.Copy(io.MultiWriter(counted, digest), limited)
	if copyErr != nil {
		return n, hash, copyErr
	}
	if n == maxPlain {
		var extra [1]byte
		if extraN, extraErr := body.Read(extra[:]); extraN != 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF)) {
			return n, hash, errors.New("decoded body exceeds size limit")
		}
	}
	if br != nil {
		if _, tailErr := br.ReadByte(); !errors.Is(tailErr, io.EOF) {
			if tailErr == nil {
				return n, hash, errors.New("trailing bytes after gzip body")
			}
			return n, hash, tailErr
		}
	}
	plainSize = n
	copy(hash[:], digest.Sum(nil))
	return plainSize, hash, nil
}

func knownIncompressible(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".zip", ".gz", ".mp4":
		return true
	}
	// MIME detection is useful for names with uncommon JPEG extensions, without
	// treating arbitrary application/octet-stream data as incompressible.
	mediaType, _, _ := mime.ParseMediaType(mime.TypeByExtension(ext))
	return mediaType == "image/jpeg" || mediaType == "image/png" || mediaType == "video/mp4" || mediaType == "application/zip" || mediaType == "application/gzip"
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}
