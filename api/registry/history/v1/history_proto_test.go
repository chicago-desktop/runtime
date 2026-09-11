package historyv1

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestRevisionCheckProtocol(t *testing.T) {
	tests := []struct {
		message     protoreflect.Name
		field       protoreflect.Name
		messageType protoreflect.FullName
		reserved    []protoreflect.FieldNumber
		number      protoreflect.FieldNumber
		kind        protoreflect.Kind
		cardinal    protoreflect.Cardinality
		presence    bool
	}{
		{message: "SubmitRequest", field: "expected_revision", number: 7, kind: protoreflect.Uint64Kind, cardinal: protoreflect.Optional, presence: true, reserved: []protoreflect.FieldNumber{3, 4}},
		{message: "RestoreRequest", field: "expected_revision", number: 6, kind: protoreflect.Uint64Kind, cardinal: protoreflect.Optional, presence: true, reserved: []protoreflect.FieldNumber{3, 4}},
		{message: "Candidate", field: "mutations", number: 6, kind: protoreflect.MessageKind, cardinal: protoreflect.Repeated, messageType: "wippy.registry.history.v1.Mutation", reserved: []protoreflect.FieldNumber{2, 3}},
	}
	for _, test := range tests {
		t.Run(string(test.message), func(t *testing.T) {
			message := File_api_registry_history_v1_history_proto.Messages().ByName(test.message)
			require.NotNil(t, message)
			field := message.Fields().ByName(test.field)
			require.NotNil(t, field)
			require.Equal(t, test.number, field.Number())
			require.Equal(t, test.kind, field.Kind())
			require.Equal(t, test.cardinal, field.Cardinality())
			require.Equal(t, test.presence, field.HasPresence())
			if test.messageType != "" {
				require.Equal(t, test.messageType, field.Message().FullName())
			}
			for _, number := range test.reserved {
				require.True(t, message.ReservedRanges().Has(number))
			}
		})
	}
	version := File_api_registry_history_v1_history_proto.Messages().ByName("Version")
	require.True(t, version.ReservedRanges().Has(5))
}
