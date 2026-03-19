package commands

import (
	"encoding/json"
	"fmt"
	"io"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/ptr"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// writeResult writes a CommandResult envelope to w. If msg is non-nil it is
// serialized and placed in the response field. If errMsg is non-empty it is
// set as the error field. Both may be set (e.g. partial success in remove).
func writeResult(w io.Writer, msg proto.Message, errMsg string) error {
	result := &bridgev1.CommandResult{
		Error: ptr.OrNil(errMsg),
	}
	result.Outcome = bridgev1.CommandResultOutcome_success
	if result.Error != nil {
		result.Outcome = bridgev1.CommandResultOutcome_error
	}
	if msg != nil {
		raw, err := protojson.Marshal(msg)
		if err != nil {
			return fmt.Errorf("failed to marshal response: %w", err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("failed to decode response JSON: %w", err)
		}
		val, err := structpb.NewValue(v)
		if err != nil {
			return fmt.Errorf("failed to build response value: %w", err)
		}
		result.Response = val
	}
	data, err := protojson.Marshal(result)
	if err != nil {
		return fmt.Errorf("failed to marshal command result: %w", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// WriteErrorResult writes a CommandResult envelope with only the error field.
func WriteErrorResult(w io.Writer, err error) error {
	return writeResult(w, nil, err.Error())
}
