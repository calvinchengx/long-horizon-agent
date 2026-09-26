package durable

import (
	"context"
	"errors"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"
)

// ClaimCheck payload codec (python/src/lha/durable/codec.py + data_converter.py).
//
// Over a weeks-long run the workflow journals many activity inputs/results; large payloads would
// saturate Temporal's history. This codec offloads any payload whose data is larger than the
// threshold (32 KiB) to the object store and journals only a pointer: metadata
// {"encoding": "lha/claimcheck/v2"}, data = the ASCII sha256 key. The WHOLE original Payload
// (deterministic protobuf serialization: data + every metadata entry) is what is stored, so
// decode restores it byte-for-byte, and a blob written by the Python codec decodes here (and the
// reverse): same encoding values, same key (sha256 of the stored bytes), same store layout
// (<LHA_OBJECT_STORE_ROOT>/<key>). The legacy v1 pointer ("lha/claimcheck": the blob is the raw
// data; the original encoding is in metadata "lha-orig-encoding") is still decoded.

const (
	claimCheckEncoding       = "lha/claimcheck/v2"
	legacyClaimCheckEncoding = "lha/claimcheck"
	legacyOrigEncodingKey    = "lha-orig-encoding"
	// DefaultClaimCheckThreshold is the size above which a payload is offloaded (32 KiB).
	DefaultClaimCheckThreshold = 32 * 1024
)

// ClaimCheckCodec offloads large payloads to an object store, journaling only a pointer.
type ClaimCheckCodec struct {
	Store     ObjectStore
	Threshold int
}

var _ converter.PayloadCodec = (*ClaimCheckCodec)(nil)

// NewClaimCheckCodec is a codec over a local store at root (threshold 32 KiB).
func NewClaimCheckCodec(root string) (*ClaimCheckCodec, error) {
	store, err := NewLocalFileObjectStore(root)
	if err != nil {
		return nil, errors.New("ClaimCheckCodec needs an object store or an explicit root")
	}
	return &ClaimCheckCodec{Store: store, Threshold: DefaultClaimCheckThreshold}, nil
}

// Encode offloads every payload whose data exceeds the threshold.
func (c *ClaimCheckCodec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		if len(p.GetData()) <= c.Threshold {
			out[i] = p
			continue
		}
		blob, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
		if err != nil {
			return nil, err
		}
		key, err := c.Store.Put(context.Background(), blob)
		if err != nil {
			return nil, err
		}
		out[i] = &commonpb.Payload{
			Metadata: map[string][]byte{converter.MetadataEncoding: []byte(claimCheckEncoding)},
			Data:     []byte(key),
		}
	}
	return out, nil
}

// Decode restores offloaded payloads (v2 and legacy v1 pointers); others pass through.
func (c *ClaimCheckCodec) Decode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		switch string(p.GetMetadata()[converter.MetadataEncoding]) {
		case claimCheckEncoding:
			blob, err := c.Store.Get(context.Background(), string(p.GetData()))
			if err != nil {
				return nil, err
			}
			original := &commonpb.Payload{}
			if err := proto.Unmarshal(blob, original); err != nil {
				return nil, err
			}
			out[i] = original
		case legacyClaimCheckEncoding:
			data, err := c.Store.Get(context.Background(), string(p.GetData()))
			if err != nil {
				return nil, err
			}
			orig := p.GetMetadata()[legacyOrigEncodingKey]
			if orig == nil {
				orig = []byte{}
			}
			out[i] = &commonpb.Payload{Metadata: map[string][]byte{converter.MetadataEncoding: orig}, Data: data}
		default:
			out[i] = p
		}
	}
	return out, nil
}

// NewDataConverter is the default data converter (JSON payloads, "json/plain") with the
// ClaimCheck codec over the object store at objectStoreRoot. The client, every worker AND the
// replayer must use the same converter (same object store), or large payloads cannot be decoded.
func NewDataConverter(objectStoreRoot string) (converter.DataConverter, error) {
	codec, err := NewClaimCheckCodec(objectStoreRoot)
	if err != nil {
		return nil, err
	}
	return converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), codec), nil
}
