package transport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func batchStream(t *testing.T, frames ...BatchFrame) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := WriteBatchMagic(&b); err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		if err := WriteBatchFrame(&b, f.Header, f.Body); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}
func TestBatchCodecRoundTripAndEnd(t *testing.T) {
	input := batchStream(t,
		BatchFrame{Header: BatchHeader{ID: "a", Status: "ok", Name: "a.txt", Hash: "h", Size: 3, Encoding: "identity", WireSize: 3}, Body: []byte("abc")},
		BatchFrame{Header: BatchHeader{End: true, Count: 1}},
	)
	d, err := NewBatchDecoder(bytes.NewReader(input), map[string]struct{}{"a": {}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := d.Next()
	if err != nil || string(f.Body) != "abc" || f.Header.ID != "a" {
		t.Fatalf("frame %+v, %v", f, err)
	}
	if _, err := d.Next(); err != nil {
		t.Fatalf("end: %v", err)
	}
	if _, err := d.Next(); err == nil {
		t.Fatal("expected EOF after end")
	}
}
func TestBatchCodecRejectsTruncatedFrameAndMissingEnd(t *testing.T) {
	good := batchStream(t,
		BatchFrame{Header: BatchHeader{ID: "a", Status: "ok", Name: "a", Hash: "h", Size: 3, Encoding: "identity", WireSize: 3}, Body: []byte("abc")},
		BatchFrame{Header: BatchHeader{End: true, Count: 1}},
	)
	endOnly := batchStream(t, BatchFrame{Header: BatchHeader{End: true, Count: 1}})
	missingEnd := len(good) - (len(endOnly) - len(BatchMagic)) // data frame complete, End frame absent
	for _, cut := range []int{len(BatchMagic) + 2, missingEnd, len(good) - 1} {
		d, err := NewBatchDecoder(bytes.NewReader(good[:cut]), nil)
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, err = d.Next()
			if err != nil {
				break
			}
		}
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("truncated stream at %d: want a non-EOF error, got %v", cut, err)
		}
	}
}
func TestBatchCodecRejectsDuplicateAndUnknownIDs(t *testing.T) {
	item := BatchFrame{Header: BatchHeader{ID: "a", Status: "not_found"}}
	input := batchStream(t, item, item, BatchFrame{Header: BatchHeader{End: true, Count: 2}})
	d, _ := NewBatchDecoder(bytes.NewReader(input), nil)
	if _, err := d.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Next(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate id: %v", err)
	}
	d, _ = NewBatchDecoder(bytes.NewReader(batchStream(t, item, BatchFrame{Header: BatchHeader{End: true, Count: 1}})), map[string]struct{}{"b": {}})
	if _, err := d.Next(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown id: %v", err)
	}
}
func TestBatchCodecEnforcesWireAndPlainCapsBeforeBodyRead(t *testing.T) {
	for _, h := range []BatchHeader{
		{ID: "a", Status: "ok", Name: "a", Hash: "h", Size: 0, Encoding: "gzip", WireSize: BatchMaxWire + 1},
		{ID: "a", Status: "ok", Name: "a", Hash: "h", Size: BatchMaxPlain + 1, Encoding: "gzip", WireSize: 1},
	} {
		var b bytes.Buffer
		_ = WriteBatchMagic(&b)
		encoded, _ := json.Marshal(h)
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(encoded)))
		b.Write(prefix[:])
		b.Write(encoded)
		b.Write(bytes.Repeat([]byte("x"), 64)) // body bytes present: rejection must come from the caps, not EOF
		d, _ := NewBatchDecoder(&b, nil)
		if _, err := d.Next(); err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("oversized header %+v: want a limit error, got %v", h, err)
		}
		if b.Len() != 64 {
			t.Fatalf("decoder read %d body bytes before rejecting caps", 64-b.Len())
		}
	}
}
func TestBatchCodecRejectsBadEndCountAndTrailingBytes(t *testing.T) {
	for _, suffix := range [][]byte{
		batchStream(t, BatchFrame{Header: BatchHeader{End: true, Count: 1}}),
		append(batchStream(t, BatchFrame{Header: BatchHeader{End: true, Count: 0}}), 'x'),
	} {
		d, err := NewBatchDecoder(bytes.NewReader(suffix), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Next(); err == nil {
			t.Fatal("accepted invalid end")
		}
	}
}
