package folder

import "context"

const (
	DeltaMinSize   = 8 << 20
	DeltaBlockSize = 1 << 20
	DeltaMaxBlocks = 65536
)

// DeltaRequest identifies the target and the receiver's base block signatures.
type DeltaRequest struct {
	Protocol   int      `json:"protocol"`
	Name       string   `json:"name"`
	TargetHash string   `json:"targetHash"`
	TargetSize int64    `json:"targetSize"`
	BaseHash   string   `json:"baseHash"`
	BaseSize   int64    `json:"baseSize"`
	BlockSize  int64    `json:"blockSize"`
	Blocks     []string `json:"blocks"`
}

// DeltaBlock is a verified target block at an absolute file offset.
type DeltaBlock struct {
	Offset int64
	Data   []byte
}

type DeltaFetcher func(context.Context, DeltaRequest, func(DeltaBlock) error) error

type deltaFetcherKey struct{}

// WithDeltaFetcher makes an optional delta transport available during apply.
func WithDeltaFetcher(ctx context.Context, fetch DeltaFetcher) context.Context {
	return context.WithValue(ctx, deltaFetcherKey{}, fetch)
}
