package wire

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// maxArgumentsBytes bounds request arguments and reply results, as the
// control plane does.
const maxArgumentsBytes = 64 << 10

// schemas holds copies of the adapter frame schemas and the identity
// definitions they reference, kept byte-identical to contracts/jsonschema by a
// test in the control-plane repository.
//
//go:embed schemas
var schemas embed.FS

const schemaBase = "https://schemas.dac-plane.invalid/"

var frameSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	err := fs.WalkDir(schemas, "schemas", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := schemas.ReadFile(path)
		if err != nil {
			return err
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return err
		}
		return compiler.AddResource(schemaBase+path, document)
	})
	if err != nil {
		return nil, err
	}
	return compiler.Compile(schemaBase + "schemas/adapter/frame.schema.json")
})

// Decode parses one frame strictly, rejecting unknown fields and trailing
// data, and validates it against the frame schema and the rules the schema
// leaves to code. Every refusal wraps ErrMalformed.
func Decode(data []byte) (Frame, error) {
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return Frame{}, fmt.Errorf("%w: a frame is one JSON object", ErrMalformed)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var frame Frame
	if err := decoder.Decode(&frame); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return Frame{}, fmt.Errorf("%w: a message carries exactly one frame", ErrMalformed)
	}
	if err := validate(data, frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// Encode validates frame and returns its JSON form.
func Encode(frame Frame) ([]byte, error) {
	data, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := validate(data, frame); err != nil {
		return nil, err
	}
	return data, nil
}

// Validate reports whether frame is one valid ControlFrame.
func Validate(frame Frame) error {
	_, err := Encode(frame)
	return err
}

func validate(data []byte, frame Frame) error {
	schema, err := frameSchema()
	if err != nil {
		return fmt.Errorf("wire: embedded schemas do not compile: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return checkRules(frame)
}

// checkRules applies what the schemas document but leave to code.
func checkRules(frame Frame) error {
	switch {
	case frame.Lifecycle != nil && frame.Lifecycle.Phase == PhaseHello:
		for _, required := range frame.Lifecycle.Required {
			if !slices.Contains(frame.Lifecycle.Offered, required) {
				return fmt.Errorf("%w: a required capability must be offered", ErrMalformed)
			}
		}
	case frame.Request != nil && len(frame.Request.Arguments) > maxArgumentsBytes:
		return fmt.Errorf("%w: arguments exceed %d bytes", ErrMalformed, maxArgumentsBytes)
	case frame.Reply != nil && len(frame.Reply.Result) > maxArgumentsBytes:
		return fmt.Errorf("%w: result exceeds %d bytes", ErrMalformed, maxArgumentsBytes)
	case frame.Event != nil:
		return checkStreamFrame(frame.Event.Body)
	}
	return nil
}

func checkStreamFrame(body StreamFrame) error {
	if body.Rows == nil {
		return nil
	}
	var previous uint64
	for _, entry := range body.Rows.Entries {
		if entry.UpdatedSequence < entry.Sequence || entry.UpdatedSequence <= previous {
			return fmt.Errorf("%w: entries must be ordered by updated sequence", ErrMalformed)
		}
		if !entry.PayloadOmitted && PayloadDigest(entry.Payload) != entry.PayloadHash {
			return fmt.Errorf("%w: entry payload does not match its hash", ErrMalformed)
		}
		previous = entry.UpdatedSequence
	}
	previous = 0
	for _, tombstone := range body.Rows.Tombstones {
		if tombstone.UpdatedSequence < tombstone.Sequence || tombstone.UpdatedSequence <= previous {
			return fmt.Errorf("%w: tombstones must be ordered by updated sequence", ErrMalformed)
		}
		previous = tombstone.UpdatedSequence
	}
	return nil
}

// PayloadDigest is the payload_hash of an inline entry payload: the SHA-256
// of its bytes exactly as sent. Frames are encoded compactly, so a payload
// should be compact JSON before it is hashed.
func PayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
