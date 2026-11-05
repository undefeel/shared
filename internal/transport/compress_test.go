package transport

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestEncodeBodyFormats(t *testing.T) {
	for _, tc := range []struct {
		name, filename, encoding string
		plain                    []byte
	}{
		{"text", "notes.txt", "gzip", []byte(strings.Repeat("sharedir sync text payload\n", 3000))},
		{"jpeg", "photo.JPEG", "identity", bytes.Repeat([]byte{0x91, 0x2a, 0x47, 0xe3}, 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			encoding, plainSize, wireSize, hash, err := EncodeBody(&wire, bytes.NewReader(tc.plain), tc.filename, true, 1<<20)
			requireNoError(t, err)
			if encoding != tc.encoding || plainSize != int64(len(tc.plain)) || wireSize != int64(wire.Len()) {
				t.Fatalf("unexpected encoded result: encoding=%q plain=%d wire=%d", encoding, plainSize, wireSize)
			}
			if tc.encoding == "gzip" {
				if wireSize >= plainSize {
					t.Fatal("text did not compress")
				}
				requireEqual(t, hash, sha256.Sum256(tc.plain), "encoder returned a hash other than the plain body hash")
			} else if wireSize != plainSize || !bytes.Equal(wire.Bytes(), tc.plain) {
				t.Fatal("JPEG was not sent unchanged")
			}
		})
	}
}

func TestDecodeRejectsBombAndTrailingGzip(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(zw, strings.Repeat("x", 4096))
	requireNoError(t, zw.Close())

	var decoded bytes.Buffer
	if _, _, err := DecodeBody(&decoded, bytes.NewReader(compressed.Bytes()), "gzip", 1024); err == nil {
		t.Fatal("expected decoded size limit error")
	}
	requireEqualf(t, decoded.Len(), 1024, "decoder wrote %d bytes beyond/under the 1024-byte limit", decoded.Len())

	trailing := append(append([]byte(nil), compressed.Bytes()...), 0x42)
	decoded.Reset()
	if _, _, err := DecodeBody(&decoded, bytes.NewReader(trailing), "gzip", 8192); err == nil {
		t.Fatal("expected trailing gzip bytes error")
	}
}

func TestDecodeReturnsPlainHash(t *testing.T) {
	plain := []byte(strings.Repeat("hash the decoded bytes\n", 200))
	var wire bytes.Buffer
	zw := gzip.NewWriter(&wire)
	mustWrite(t, zw, plain)
	requireNoError(t, zw.Close())
	var decoded bytes.Buffer
	size, hash, err := DecodeBody(&decoded, bytes.NewReader(wire.Bytes()), "gzip", int64(len(plain)))
	requireNoError(t, err)
	if size != int64(len(plain)) || hash != sha256.Sum256(plain) || !bytes.Equal(decoded.Bytes(), plain) {
		t.Fatal("decoded bytes, size, or hash do not match the plain payload")
	}
}

func TestDecodeRejectsCorruptGzipAndUnsupportedEncoding(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte("body"))
	_ = zw.Close()
	corrupt := append([]byte(nil), compressed.Bytes()...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, _, err := DecodeBody(io.Discard, bytes.NewReader(corrupt), "gzip", 4); !errors.Is(err, gzip.ErrChecksum) {
		t.Fatalf("expected corrupt gzip checksum error at size boundary, got %v", err)
	}
	if _, _, err := DecodeBody(io.Discard, strings.NewReader("body"), "br", 100); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("unexpected unsupported encoding error: %v", err)
	}
}

func TestEncodePropagatesReadErrorAtSizeBoundary(t *testing.T) {
	want := errors.New("source read failed")
	source := &errorAfterBytesReader{data: bytes.Repeat([]byte("x"), 64<<10), err: want}
	_, _, _, _, err := EncodeBody(io.Discard, source, "body.txt", true, 64<<10)
	if !errors.Is(err, want) {
		t.Fatalf("expected source error, got %v", err)
	}
}

type errorAfterBytesReader struct {
	data []byte
	err  error
}

func (r *errorAfterBytesReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
