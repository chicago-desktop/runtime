package encoding

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/wippyai/runtime/api/attrs"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/registry"
)

const MaxEntryBytes = 4 * 1024 * 1024

var entryHandle = func() *codec.MsgpackHandle {
	h := &codec.MsgpackHandle{}
	h.MapType = reflect.TypeOf(map[string]any(nil))
	h.Canonical = true
	h.WriteExt = true
	h.ErrorIfNoField = true
	return h
}()

type entryValue struct {
	Meta     attrs.Bag
	Data     *payloadValue
	ID       string
	Kind     string
	Registry registry.EntryMetadata
}

type payloadValue struct {
	Data   any
	Format string
}

func EncodeEntry(entry registry.Entry) ([]byte, error) {
	entry.ID = entry.ID.Canonical()
	if entry.ID.Name == "" || entry.Kind == "" {
		return nil, errors.New("entry ID and kind are required")
	}
	value := entryValue{ID: entry.ID.String(), Kind: entry.Kind, Meta: entry.Meta, Registry: entry.Registry}
	if entry.Data != nil {
		value.Data = &payloadValue{Format: entry.Data.Format(), Data: entry.Data.Data()}
	}
	var data []byte
	if err := codec.NewEncoderBytes(&data, entryHandle).Encode(value); err != nil {
		return nil, fmt.Errorf("encode entry: %w", err)
	}
	if len(data)+1 > MaxEntryBytes {
		return nil, errors.New("entry exceeds size limit")
	}
	return append([]byte{1}, data...), nil
}

func DecodeEntry(data []byte) (registry.Entry, error) {
	if len(data) < 2 || len(data) > MaxEntryBytes || data[0] != 1 {
		return registry.Entry{}, errors.New("invalid entry encoding")
	}
	var value entryValue
	decoder := codec.NewDecoderBytes(data[1:], entryHandle)
	if err := decoder.Decode(&value); err != nil {
		return registry.Entry{}, fmt.Errorf("decode entry: %w", err)
	}
	if decoder.NumBytesRead() != len(data)-1 {
		return registry.Entry{}, errors.New("entry has trailing data")
	}
	id := registry.ParseID(value.ID).Canonical()
	if id.Name == "" || value.Kind == "" || id.String() != value.ID {
		return registry.Entry{}, errors.New("invalid entry ID or kind")
	}
	entry := registry.Entry{ID: id, Kind: value.Kind, Meta: value.Meta, Registry: value.Registry}
	if value.Data != nil {
		entry.Data = payload.NewPayload(value.Data.Data, value.Data.Format)
	}
	return entry, nil
}
