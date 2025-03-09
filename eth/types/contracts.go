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

type ProposalVoteChoice int

const (
	Against ProposalVoteChoice = iota
	For
	Abstain
)

var ProposalVoteChoices = []ProposalVoteChoice{Against, For, Abstain}

func (v ProposalVoteChoice) String() string {
	switch v {
	case Against:
		return "Against"
	case For:
		return "For"
	case Abstain:
		return "Abstain"
	default:
		return ""
	}
}

func (v ProposalVoteChoice) IsValid() bool {
	return v == Against || v == For || v == Abstain
}

type TranscoderPoolHints struct {
	PosNext common.Address
	PosPrev common.Address
}
