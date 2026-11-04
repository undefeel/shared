package transport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	BatchMagic     = "SHDFIL02"
	BatchMaxItems  = 64
	BatchMaxHeader = 4096
	BatchMaxPlain  = 256 << 10
	BatchMaxWire   = BatchMaxPlain + 1024
	BatchMaxTotal  = 8 << 20
)

type BatchHeader struct {
	ID       string `json:"id,omitempty"`
	Status   string `json:"status,omitempty"`
	Name     string `json:"name,omitempty"`
	Hash     string `json:"hash,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	WireSize int64  `json:"wireSize,omitempty"`
	End      bool   `json:"end,omitempty"`
	Count    int    `json:"count,omitempty"`
}

type BatchFrame struct {
	Header BatchHeader
	Body   []byte
}

type BatchDecoder struct {
	r        io.Reader
	expected map[string]struct{}
	seen     map[string]struct{}
	count    int
	plain    int64
	wire     int64
	done     bool
}

func NewBatchDecoder(r io.Reader, expected map[string]struct{}) (*BatchDecoder, error) {
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("read batch magic: %w", err)
	}
	if string(magic[:]) != BatchMagic {
		return nil, errors.New("invalid batch magic")
	}
	d := &BatchDecoder{r: r, seen: make(map[string]struct{}), expected: expected}
	if expected != nil && len(expected) > BatchMaxItems {
		return nil, errors.New("too many expected batch items")
	}
	return d, nil
}

func (d *BatchDecoder) Next() (BatchFrame, error) {
	if d.done {
		return BatchFrame{}, io.EOF
	}
	var size [4]byte
	if _, err := io.ReadFull(d.r, size[:]); err != nil {
		if errors.Is(err, io.EOF) {
			// The stream ended before the End frame: never report a clean EOF.
			err = io.ErrUnexpectedEOF
		}
		return BatchFrame{}, fmt.Errorf("read batch frame length: %w", err)
	}
	headerLen := binary.BigEndian.Uint32(size[:])
	if headerLen == 0 || headerLen > BatchMaxHeader {
		return BatchFrame{}, errors.New("batch header exceeds size limit")
	}
	encoded := make([]byte, int(headerLen))
	if _, err := io.ReadFull(d.r, encoded); err != nil {
		return BatchFrame{}, fmt.Errorf("read batch header: %w", err)
	}
	var h BatchHeader
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return BatchFrame{}, fmt.Errorf("decode batch header: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return BatchFrame{}, errors.New("trailing batch header JSON")
	}
	if h.End {
		if h.ID != "" || h.Status != "" || h.Name != "" || h.Hash != "" || h.Size != 0 || h.Encoding != "" || h.WireSize != 0 || h.Count != d.count {
			return BatchFrame{}, errors.New("invalid batch end frame")
		}
		var trailing [1]byte
		if _, err := io.ReadFull(d.r, trailing[:]); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errors.New("trailing bytes after batch end")
			}
			return BatchFrame{}, err
		}
		if d.expected != nil && d.count != len(d.expected) {
			return BatchFrame{}, errors.New("batch item count does not match request")
		}
		d.done = true
		return BatchFrame{Header: h}, nil
	}
	if h.ID == "" || h.Size < 0 || h.Size > BatchMaxPlain || h.WireSize < 0 || h.WireSize > BatchMaxWire {
		return BatchFrame{}, errors.New("invalid batch item limits or id")
	}
	if _, ok := d.seen[h.ID]; ok {
		return BatchFrame{}, fmt.Errorf("duplicate batch id %q", h.ID)
	}
	if d.expected != nil {
		if _, ok := d.expected[h.ID]; !ok {
			return BatchFrame{}, fmt.Errorf("unknown batch id %q", h.ID)
		}
	}
	if d.count >= BatchMaxItems {
		return BatchFrame{}, errors.New("too many batch items")
	}
	switch h.Status {
	case "ok":
		if h.Name == "" || h.Hash == "" || (h.Encoding != "identity" && h.Encoding != "gzip") || h.WireSize == 0 && h.Size != 0 {
			return BatchFrame{}, errors.New("invalid successful batch header")
		}
		if h.Encoding == "identity" && h.WireSize != h.Size {
			return BatchFrame{}, errors.New("identity frame size mismatch")
		}
	case "not_found", "version_changed", "blocked":
		if h.Size != 0 || h.WireSize != 0 {
			return BatchFrame{}, errors.New("error batch frame has a body")
		}
	default:
		return BatchFrame{}, errors.New("invalid batch status")
	}
	if d.plain+h.Size > BatchMaxTotal || d.wire+h.WireSize > BatchMaxTotal+BatchMaxItems*1024 {
		return BatchFrame{}, errors.New("batch totals exceed size limits")
	}
	body := make([]byte, int(h.WireSize))
	if _, err := io.ReadFull(d.r, body); err != nil {
		return BatchFrame{}, fmt.Errorf("read batch body: %w", err)
	}
	d.seen[h.ID] = struct{}{}
	d.count++
	d.plain += h.Size
	d.wire += h.WireSize
	return BatchFrame{Header: h, Body: body}, nil
}

func WriteBatchMagic(w io.Writer) error {
	_, err := io.WriteString(w, BatchMagic)
	return err
}

func WriteBatchFrame(w io.Writer, h BatchHeader, body []byte) error {
	if len(body) > BatchMaxWire || int64(len(body)) != h.WireSize {
		return errors.New("batch body size mismatch or limit exceeded")
	}
	encoded, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > BatchMaxHeader {
		return errors.New("batch header exceeds size limit")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(encoded)))
	if err := writeAll(w, prefix[:]); err != nil {
		return err
	}
	if err := writeAll(w, encoded); err != nil {
		return err
	}
	return writeAll(w, body)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
