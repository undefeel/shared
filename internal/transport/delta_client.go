package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"sharedir/internal/folder"
)

func (c *Client) DeltaFetcher(addr string) folder.DeltaFetcher {
	return func(ctx context.Context, req folder.DeltaRequest, emit func(folder.DeltaBlock) error) error {
		return c.FetchDelta(ctx, addr, req, emit)
	}
}

func readDeltaHeader(r io.Reader) (deltaHeader, error) {
	var h deltaHeader
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return h, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > deltaMaxHeader {
		return h, errors.New("invalid delta header length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return h, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&h); err != nil {
		return h, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return h, errors.New("trailing delta header JSON")
	}
	return h, nil
}

func (c *Client) FetchDelta(ctx context.Context, addr string, req folder.DeltaRequest, emit func(folder.DeltaBlock) error) error {
	if req.TargetSize < 0 || req.TargetSize > int64(folder.DeltaMaxBlocks)*folder.DeltaBlockSize || !validDeltaHash(req.TargetHash) {
		return errors.New("invalid target")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	u := url.URL{Scheme: "https", Host: addr, Path: "/delta"}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set(portHeader, strconv.Itoa(c.port))
	request.Header.Set(ProtocolHeader, "2")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/x-sharedir-delta;version=2")
	request.Header.Set("Accept-Encoding", "gzip")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	request = request.WithContext(ctx)
	resp, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer closeResponseOnCancel(ctx, resp.Body)()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	typ, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || typ != "application/x-sharedir-delta" || params["version"] != "2" {
		return errors.New("invalid delta content type")
	}
	r := c.progressBody(resp.Body, cancel)
	var magic [8]byte
	if _, err = io.ReadFull(r, magic[:]); err != nil {
		return err
	}
	if string(magic[:]) != deltaMagic {
		return errors.New("invalid delta magic")
	}
	h, err := readDeltaHeader(r)
	if err != nil {
		return err
	}
	if h.TargetHash != req.TargetHash || h.TargetSize != req.TargetSize || h.Status != "ok" || h.Offset != 0 || h.Length != 0 || h.Hash != "" || h.Encoding != "" || h.WireSize != 0 || h.End || h.Frames != 0 {
		return errors.New("delta unavailable or invalid header")
	}
	count := 0
	var next int64
	for {
		h, err = readDeltaHeader(r)
		if err != nil {
			return err
		}
		if h.End {
			if h.TargetHash != req.TargetHash || h.TargetSize != req.TargetSize || h.Frames != count || h.Status != "" || h.Offset != 0 || h.Length != 0 || h.Hash != "" || h.Encoding != "" || h.WireSize != 0 {
				return errors.New("invalid delta end")
			}
			var tail [1]byte
			if _, err = io.ReadFull(r, tail[:]); err != io.EOF {
				if err == nil {
					return errors.New("trailing delta bytes")
				}
				return err
			}
			return nil
		}
		if h.Status != "" || h.TargetHash != "" || h.TargetSize != 0 || h.Frames != 0 || h.Offset < next || h.Offset < 0 || h.Offset%folder.DeltaBlockSize != 0 || h.Offset >= req.TargetSize || count >= deltaCount(req.TargetSize) || h.Length != deltaLength(req.TargetSize, int(h.Offset/folder.DeltaBlockSize)) || !validDeltaHash(h.Hash) || h.WireSize <= 0 || h.WireSize > folder.DeltaBlockSize+1024 || (h.Encoding != "identity" && h.Encoding != "gzip") || h.Encoding == "identity" && h.WireSize != h.Length {
			return errors.New("invalid delta block")
		}
		wire := make([]byte, int(h.WireSize))
		if _, err = io.ReadFull(r, wire); err != nil {
			return err
		}
		var plain bytes.Buffer
		n, hash, err := DecodeBody(&plain, bytes.NewReader(wire), h.Encoding, h.Length)
		if err != nil || n != h.Length || hex.EncodeToString(hash[:]) != h.Hash {
			return errors.New("invalid delta block body")
		}
		if err = emit(folder.DeltaBlock{Offset: h.Offset, Data: plain.Bytes()}); err != nil {
			return err
		}
		next = h.Offset + h.Length
		count++
	}
}
