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

func TestEncodeTextUsesGzipForRelay(t *testing.T) {
	plain := strings.Repeat("sharedir sync text payload\n", 3000)
	var wire bytes.Buffer
	encoding, plainSize, wireSize, hash, err := EncodeBody(&wire, strings.NewReader(plain), "notes.txt", true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if encoding != "gzip" || plainSize != int64(len(plain)) || wireSize != int64(wire.Len()) || wireSize >= plainSize {
		t.Fatalf("unexpected encoded result: encoding=%q plain=%d wire=%d", encoding, plainSize, wireSize)
	}
	if hash != sha256.Sum256([]byte(plain)) {
		t.Fatal("encoder returned a hash other than the plain body hash")
	}
}

func TestEncodeJPEGUsesIdentity(t *testing.T) {
	plain := bytes.Repeat([]byte{0x91, 0x2a, 0x47, 0xe3}, 4096)
	var wire bytes.Buffer
	encoding, plainSize, wireSize, _, err := EncodeBody(&wire, bytes.NewReader(plain), "photo.JPEG", true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if encoding != "identity" || plainSize != int64(len(plain)) || wireSize != plainSize || !bytes.Equal(wire.Bytes(), plain) {
		t.Fatalf("JPEG was not sent unchanged: encoding=%q plain=%d wire=%d", encoding, plainSize, wireSize)
	}
}

func TestDecodeRejectsBombAndTrailingGzip(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(zw, strings.Repeat("x", 4096))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var decoded bytes.Buffer
	if _, _, err := DecodeBody(&decoded, bytes.NewReader(compressed.Bytes()), "gzip", 1024); err == nil {
		t.Fatal("expected decoded size limit error")
	}
	if decoded.Len() != 1024 {
		t.Fatalf("decoder wrote %d bytes beyond/under the 1024-byte limit", decoded.Len())
	}

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
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var decoded bytes.Buffer
	size, hash, err := DecodeBody(&decoded, bytes.NewReader(wire.Bytes()), "gzip", int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
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
	if _, _, err := DecodeBody(io.Discard, bytes.NewReader(corrupt), "gzip", 100); err == nil {
		t.Fatal("expected corrupt gzip checksum error")
	}
	if _, _, err := DecodeBody(io.Discard, strings.NewReader("body"), "br", 100); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("unexpected unsupported encoding error: %v", err)
	}
}
