package signercompat

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// These bytes were serialized by the pinned Python runner's lp_rpc_pb2.py.
// They lock field numbers and the retained binary wire shape independently of
// this package's encoder.
func TestPinnedPythonProtobufFixtures(t *testing.T) {
	const orchestrator = "0a1c68747470733a2f2f6f7263686573747261746f722e6578616d706c651289010a1472727272727272727272727272727272727272721201641a20ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff222071717171717171717171717171717171717171717171717171717171717171712a01013201803a240811122068686868686868686868686868686868686868686868686868686868686868681a04081910012214616161616161616161616161616161616161616132170a05746f6b656e12086d616e69666573741880a8d6b907"
	const payment = "0a89010a1472727272727272727272727272727272727272721201641a20ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff222071717171717171717171717171717171717171717171717171717171717171712a01013201803a24081112206868686868686868686868686868686868686868686868686868686868686868121473737373737373737373737373737373737373731a2408111220686868686868686868686868686868686868686868686868686868686868686822450804124153535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353532a0408191001"
	const segment = "0a086d616e69666573741a206b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b2a41535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535353535342170a05746f6b656e12086d616e69666573741880a8d6b907"
	decode := func(value string) []byte { data, err := hex.DecodeString(value); require.NoError(t, err); return data }
	o, err := DecodeOrchestratorInfo(decode(orchestrator))
	require.NoError(t, err)
	require.Equal(t, "manifest", o.Auth.SessionID)
	require.Equal(t, int64(25), o.Price.PricePerUnit)
	require.Equal(t, decode(orchestrator), EncodeOrchestratorInfo(o))
	p, err := DecodePayment(decode(payment))
	require.NoError(t, err)
	require.Equal(t, uint32(4), p.SenderParams[0].SenderNonce)
	require.Equal(t, decode(payment), EncodePayment(p))
	s, err := DecodeSegData(decode(segment))
	require.NoError(t, err)
	require.Equal(t, "manifest", s.Auth.SessionID)
	require.Equal(t, decode(segment), EncodeSegData(s))
}

func FuzzDecodeEnvelopes(f *testing.F) {
	f.Add([]byte{0x0a, 0x01, 0x78})
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeOrchestratorInfo(data)
		_, _ = DecodePayment(data)
		_, _ = DecodeSegData(data)
	})
}
