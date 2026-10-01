package pm

import (
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestVerifyTicketSignature(t *testing.T) {
	addr := ethcommon.HexToAddress("3BadDb1eeE2105893136A3F96c8a963E9C6309d6")
	msg := ethcommon.FromHex("b7da355477356fc4c47fcabcf232dc77a6db9b07b7e48b76261cc55cc8fbabb3")
	sig := ethcommon.FromHex("206443228e8f784bc3a122de0d85eb3ebff82d6a79cca26c7eeb907099a6404f6dff57bc6828f28bd6cd073c89d94cf3364204679ed8365fa45b5ee6af19a9841c")
	highS := ethcommon.FromHex("e742ff452d41413616a5bf43fe15dd88294e983d3d36206c2712f39083d638bde0a0fc89be718fbc1033e1d30d78be1c68081562ed2e97af876f286f3453231d1b")
	invalidV := append([]byte(nil), sig...)
	invalidV[64] -= 27
	for _, tt := range []struct {
		name               string
		address            ethcommon.Address
		message, signature []byte
		valid              bool
	}{
		{"valid", addr, msg, sig, true},
		{"wrong sender", ethcommon.HexToAddress("0x1234"), msg, sig, false},
		{"wrong message", addr, []byte("different message"), sig, false},
		{"truncated signature", addr, msg, sig[:64], false},
		{"malleable high S", addr, msg, highS, false},
		{"invalid recovery ID", addr, msg, invalidV, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.valid, (DefaultSigVerifier{}).Verify(tt.address, tt.message, tt.signature))
		})
	}
}
