package pm

import (
	ethcommon "github.com/ethereum/go-ethereum/common"
)

type stubSigVerifier struct{ verifyResult bool }

func (s *stubSigVerifier) SetVerifyResult(valid bool) { s.verifyResult = valid }
func (s *stubSigVerifier) Verify(ethcommon.Address, []byte, []byte) bool {
	return s.verifyResult
}
