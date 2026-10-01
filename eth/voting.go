// Voting APIs follow livepeer/go-livepeer/eth/client.go: poll voting by
// Nico Vergauwen (994f4c0e5a755717f47feab1a397c74307cef8ef) and proposal voting
// by Rick Staa (89d9ebc7fff0004aab11bbb733035327da7b2e1b).
package eth

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	lpTypes "github.com/livepeer/node/eth/types"
)

// Vote plans a vote on the explicitly selected Poll contract.
func (c *Contracts) Vote(ctx context.Context, from, poll common.Address, choice lpTypes.VoteChoice) (TransactionPlan, error) {
	if !choice.IsValid() || poll == (common.Address{}) {
		return TransactionPlan{}, errors.New("invalid poll or vote choice")
	}
	data, err := c.Pack("poll", "vote", big.NewInt(int64(choice)))
	if err != nil {
		return TransactionPlan{}, err
	}
	return c.PlanTransaction(ctx, from, poll, data, new(big.Int))
}

func (c *Contracts) ProposalVote(ctx context.Context, from common.Address, proposal *big.Int, choice lpTypes.ProposalVoteChoice) (TransactionPlan, error) {
	return c.proposalVote(ctx, from, proposal, choice, nil)
}

func (c *Contracts) ProposalVoteWithReason(ctx context.Context, from common.Address, proposal *big.Int, choice lpTypes.ProposalVoteChoice, reason string) (TransactionPlan, error) {
	return c.proposalVote(ctx, from, proposal, choice, &reason)
}

func (c *Contracts) proposalVote(ctx context.Context, from common.Address, proposal *big.Int, choice lpTypes.ProposalVoteChoice, reason *string) (TransactionPlan, error) {
	if proposal == nil || proposal.Sign() < 0 || proposal.BitLen() > 256 || !choice.IsValid() {
		return TransactionPlan{}, errors.New("invalid proposal or vote choice")
	}
	method, args := "castVote", []any{proposal, uint8(choice)}
	if reason != nil {
		method, args = "castVoteWithReason", append(args, *reason)
	}
	return c.PlanContract(ctx, from, "governor", method, new(big.Int), args...)
}
