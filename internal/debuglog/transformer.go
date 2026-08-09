package debuglog

import "fmt"

// TransformerKindNoop is the only content-transformer descriptor V1 accepts.
const TransformerKindNoop = "noop"

// ContentTransformer rewrites log message data while a snapshot is encoded.
// Applying it at encode time means the first object uploaded to cloud storage
// already carries the transformed bytes, so no later raw-to-transformed copy
// is required. V1 ships only a byte-preserving implementation; real redaction
// rules arrive as a new descriptor kind or version.
type ContentTransformer interface {
	// Kind is the descriptor kind this transformer implements.
	Kind() string
	// Version is the descriptor version reported in the acknowledgement.
	Version() int
	// Transform rewrites one decoded data chunk. It must not be given
	// timestamps, sequence numbers, identity fields, or warning codes.
	Transform(data []byte) ([]byte, error)
}

// ErrUnsupportedTransformer reports a descriptor this worker cannot honor.
// The coordinator maps it to failed/unsupported_content_transformer and
// uploads nothing, so untransformed data can never reach the target.
type ErrUnsupportedTransformer struct {
	Kind    string
	Version int
}

func (e *ErrUnsupportedTransformer) Error() string {
	return fmt.Sprintf("debuglog: unsupported content transformer %q version %d", e.Kind, e.Version)
}

type noopTransformer struct{}

func (noopTransformer) Kind() string { return TransformerKindNoop }

func (noopTransformer) Version() int { return 1 }

func (noopTransformer) Transform(data []byte) ([]byte, error) { return data, nil }

// NewTransformer resolves a descriptor to its implementation.
func NewTransformer(kind string, version int) (ContentTransformer, error) {
	if kind == TransformerKindNoop && version == 1 {
		return noopTransformer{}, nil
	}
	return nil, &ErrUnsupportedTransformer{Kind: kind, Version: version}
}
