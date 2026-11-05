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
	requireNoError(t, WriteBatchMagic(&b))
	for _, f := range frames {
		requireNoError(t, WriteBatchFrame(&b, f.Header, f.Body))
	}
	return b.Bytes()
}
func TestBatchCodecRejectsTruncatedFrameAndMissingEnd(t *testing.T) {
	good := batchStream(t,
		BatchFrame{Header: BatchHeader{ID: "a", Status: "ok", Name: "a", Hash: "h", Size: 3, Encoding: "identity", WireSize: 3}, Body: []byte("abc")},
		BatchFrame{Header: BatchHeader{End: true, Count: 1}},
	)
	endOnly := batchStream(t, BatchFrame{Header: BatchHeader{End: true, Count: 1}})
	missingEnd := len(good) - (len(endOnly) - len(BatchMagic)) // data frame complete, End frame absent
	for _, cut := range []int{len(BatchMagic) + 2, missingEnd, len(good) - 1} {
		d := testBatchDecoder(t, bytes.NewReader(good[:cut]), nil)
		var err error
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
	for _, tc := range []struct {
		name         string
		frames       []BatchFrame
		allowed      map[string]struct{}
		consumeFirst bool
	}{
		{"duplicate", []BatchFrame{item, item, {Header: BatchHeader{End: true, Count: 2}}}, nil, true},
		{"unknown", []BatchFrame{item, {Header: BatchHeader{End: true, Count: 1}}}, map[string]struct{}{"b": {}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testBatchDecoder(t, bytes.NewReader(batchStream(t, tc.frames...)), tc.allowed)
			if tc.consumeFirst {
				if _, err := d.Next(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.Next(); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("%s id: %v", tc.name, err)
			}
		})
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
		requireEqualf(t, b.Len(), 64, "decoder read %d body bytes before rejecting caps", 64-b.Len())
	}
}
func TestBatchCodecRejectsBadEndCountAndTrailingBytes(t *testing.T) {
	for _, suffix := range [][]byte{
		batchStream(t, BatchFrame{Header: BatchHeader{End: true, Count: 1}}),
		append(batchStream(t, BatchFrame{Header: BatchHeader{End: true, Count: 0}}), 'x'),
	} {
		d := testBatchDecoder(t, bytes.NewReader(suffix), nil)
		if _, err := d.Next(); err == nil {
			t.Fatal("accepted invalid end")
		}
	}
}
