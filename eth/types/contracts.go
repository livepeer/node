package types

import "github.com/ethereum/go-ethereum/common"

type VoteChoice int

const (
	Yes VoteChoice = iota
	No
)

var VoteChoices = []VoteChoice{Yes, No}

func (v VoteChoice) String() string {
	switch v {
	case Yes:
		return "Yes"
	case No:
		return "No"
	default:
		return ""
	}
}

func (v VoteChoice) IsValid() bool {
	return v == Yes || v == No
}

type TranscoderPoolHints struct {
	PosNext common.Address
	PosPrev common.Address
}
