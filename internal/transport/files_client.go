package transport

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// BatchResult contains either a verified file body or the server's per-item status.
type BatchResult struct {
	Header BatchHeader
	Body   []byte
}

// FetchFiles retrieves one /files batch. Fully verified frames survive a broken
// connection; subsequent requests contain only IDs which have not completed.
// Results are returned on cancellation so callers can keep already-fetched data.
func (c *Client) FetchFiles(ctx context.Context, addr string, items []BatchItem) (map[string]BatchResult, error) {
	results := make(map[string]BatchResult, len(items))
	if len(items) == 0 || len(items) > BatchMaxItems {
		return results, errors.New("invalid batch item count")
	}
	ids, names := make(map[string]bool, len(items)), make(map[string]bool, len(items))
	var total int64
	for _, item := range items {
		if item.ID == "" || item.Name == "" || item.Hash == "" || item.Size < 0 || item.Size > BatchMaxPlain || ids[item.ID] || names[item.Name] {
			return results, errors.New("invalid batch item")
		}
		ids[item.ID], names[item.Name] = true, true
		total += item.Size
	}
	if total > BatchMaxTotal {
		return results, errors.New("batch exceeds size limit")
	}
	delay := 250 * time.Millisecond
	for len(results) != len(items) {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		remaining := make([]BatchItem, 0, len(items)-len(results))
		expected := make(map[string]struct{}, cap(remaining))
		byID := make(map[string]BatchItem, cap(remaining))
		for _, item := range items {
			if _, ok := results[item.ID]; !ok {
				remaining = append(remaining, item)
				expected[item.ID] = struct{}{}
				byID[item.ID] = item
			}
		}
		if err := c.fetchFilesAttempt(ctx, addr, remaining, expected, byID, results); err != nil {
			if ctx.Err() != nil {
				return results, ctx.Err()
			}
			if len(results) == len(items) {
				return results, err
			}
			if !retryableFilesError(err) {
				return results, err
			}
			// A broken stream is retried by the outer loop with only unseen IDs.
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return results, ctx.Err()
			case <-timer.C:
			}
			if delay < 30*time.Second {
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
			continue
		}
	}
	return results, nil
}

func (c *Client) fetchFilesAttempt(ctx context.Context, addr string, items []BatchItem, expected map[string]struct{}, byID map[string]BatchItem, results map[string]BatchResult) error {
	payload, err := json.Marshal(struct {
		Protocol int         `json:"protocol"`
		Items    []BatchItem `json:"items"`
	}{ProtocolVersion, items})
	if err != nil {
		return err
	}
	u := url.URL{Scheme: "https", Host: addr, Path: "/files"}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set(portHeader, strconv.Itoa(c.port))
	req.Header.Set(ProtocolHeader, "2")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-sharedir-files;version=2")
	// Setting this explicitly disables net/http's transparent decompression;
	// /files frames carry their own encoding and wire-size accounting.
	req.Header.Set("Accept-Encoding", "gzip")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer closeResponseOnCancel(ctx, resp.Body)()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-sharedir-files" || params["version"] != "2" {
		return errors.New("invalid /files content type")
	}
	d, err := NewBatchDecoder(c.progressBody(resp.Body, cancel), expected)
	if err != nil {
		return err
	}
	for {
		frame, err := d.Next()
		if err != nil {
			return err
		}
		if frame.Header.End {
			return nil
		}
		want := byID[frame.Header.ID]
		if frame.Header.Status == "ok" {
			if frame.Header.Name != want.Name || frame.Header.Hash != want.Hash || frame.Header.Size != want.Size {
				return errors.New("/files response does not match requested version")
			}
			var plain bytes.Buffer
			n, hash, err := DecodeBody(&plain, bytes.NewReader(frame.Body), frame.Header.Encoding, want.Size)
			if err != nil {
				return fmt.Errorf("invalid /files body encoding: %v", err)
			}
			if n != want.Size || hex.EncodeToString(hash[:]) != want.Hash {
				return errors.New("/files body hash or size mismatch")
			}
			frame.Body = plain.Bytes()
		}
		results[frame.Header.ID] = BatchResult{Header: frame.Header, Body: frame.Body}
	}
}

func retryableFilesError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
