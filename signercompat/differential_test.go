package signercompat

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// An independent runtime descriptor locks the selected legacy schema while
// keeping generated messages/runtime dependencies confined to wire.
func orchestratorDescriptor(t testing.TB) protoreflect.MessageDescriptor {
	t.Helper()
	field := func(name string, n int32, kind descriptorpb.FieldDescriptorProto_Type, message string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: new(name), Number: new(n), Type: &kind}
		if message != "" {
			f.TypeName = new(message)
		}
		return f
	}
	b := func(name string, n int32) *descriptorpb.FieldDescriptorProto {
		return field(name, n, descriptorpb.FieldDescriptorProto_TYPE_BYTES, "")
	}
	i := func(name string, n int32) *descriptorpb.FieldDescriptorProto {
		return field(name, n, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
	}
	s := func(name string, n int32) *descriptorpb.FieldDescriptorProto {
		return field(name, n, descriptorpb.FieldDescriptorProto_TYPE_STRING, "")
	}
	m := func(name string, n int32, typ string) *descriptorpb.FieldDescriptorProto {
		return field(name, n, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, typ)
	}
	message := func(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: new(name), Field: fields}
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: new("retained.proto"), Syntax: new("proto3"), MessageType: []*descriptorpb.DescriptorProto{
			message("PriceInfo", i("pricePerUnit", 1), i("pixelsPerUnit", 2)),
			message("ExpirationParams", i("creationRound", 1), b("creationRoundBlockHash", 2)),
			message("TicketParams", b("recipient", 1), b("faceValue", 2), b("winProb", 3), b("recipientRandHash", 4), b("seed", 5), b("expirationBlock", 6), m("expirationParams", 7, "ExpirationParams")),
			message("AuthToken", b("token", 1), s("sessionId", 2), i("expiration", 3)),
			message("OrchestratorInfo", s("transcoder", 1), m("ticketParams", 2, "TicketParams"), m("priceInfo", 3, "PriceInfo"), b("address", 4), m("authToken", 6, "AuthToken")),
		},
	}, nil)
	require.NoError(t, err)
	return file.Messages().ByName("OrchestratorInfo")
}

// Compare selected values, ignoring unknown fields and empty-message presence
// because the small public adapter intentionally does not expose either.
func selectedValues(m protoreflect.Message) map[string]any {
	result := map[string]any{}
	fields := m.Descriptor().Fields()
	for n := 0; n < fields.Len(); n++ {
		f := fields.Get(n)
		v := m.Get(f)
		switch f.Kind() {
		case protoreflect.MessageKind:
			result[string(f.Name())] = selectedValues(v.Message())
		case protoreflect.BytesKind:
			result[string(f.Name())] = string(v.Bytes())
		default:
			result[string(f.Name())] = v.Interface()
		}
	}
	return result
}

func FuzzOrchestratorProtobufSemantics(f *testing.F) {
	descriptor := orchestratorDescriptor(f)
	f.Add(EncodeOrchestratorInfo(OrchestratorInfo{Auth: AuthToken{SessionID: "manifest"}}))
	f.Add(append(bytesField(3, intField(1, 9)), bytesField(3, intField(2, 1))...))
	f.Add(append(bytesField(6, bytesField(2, []byte("manifest"))), bytesField(6, intField(3, 100))...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxEnvelope {
			return
		}
		native := dynamicpb.NewMessage(descriptor)
		nativeErr := proto.Unmarshal(data, native)
		decoded, err := DecodeOrchestratorInfo(data)
		if nativeErr != nil {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		roundTrip := dynamicpb.NewMessage(descriptor)
		require.NoError(t, proto.Unmarshal(EncodeOrchestratorInfo(decoded), roundTrip))
		require.Equal(t, selectedValues(native), selectedValues(roundTrip))
	})
}

func TestMergedMessagesAndNumericLimits(t *testing.T) {
	price := append(bytesField(5, intField(1, 9)), bytesField(5, intField(2, 1))...)
	price = append(price, bytesField(5, intField(1, 0))...)
	p, err := DecodePayment(price)
	require.NoError(t, err)
	require.Equal(t, PriceInfo{PricePerUnit: 0, UnitsPerPrice: 1}, p.ExpectedPrice, "an explicit zero overwrites the previous scalar, but preserves omitted fields")
	seg := append(bytesField(8, bytesField(2, []byte("session"))), bytesField(8, intField(3, 100))...)
	s, err := DecodeSegData(seg)
	require.NoError(t, err)
	require.Equal(t, AuthToken{SessionID: "session", Expiration: 100}, s.Auth)
	_, err = DecodePayment(bytesField(4, intField(1, 1<<32)))
	require.ErrorContains(t, err, "uint32")
	_, err = DecodeOrchestratorInfo(bytesField(1, []byte{0xff}))
	require.Error(t, err)
	_, err = DecodeOrchestratorInfo(protowire.AppendTag(nil, protowire.MaxValidNumber+1, protowire.VarintType))
	require.Error(t, err)
}
