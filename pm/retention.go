package pm

import (
	"context"
	"math/big"
	"time"
)

// Ported from go-livepeer/pm/recipient.go's senderNonces. A restart rotates the
// recipient key, so parameters from a prior lifetime cannot authenticate.
type recipientNonces struct {
	nonceSeen       map[uint32]bool
	expirationBlock *big.Int
}

// observeBlock performs the upstream nonce cleanup at parameter expiry. Keep
// expiry monotonic within a recipient lifetime so a backward chain observation
// cannot revive parameters whose replay guards have already been removed.
// Caller holds e.mu.
func (e *Engine) observeBlock(block *big.Int) {
	if block.Cmp(e.lastSeenBlock) <= 0 {
		return
	}
	e.lastSeenBlock.Set(block)
	for key, nonces := range e.senderNonces {
		if nonces.expirationBlock.Cmp(block) <= 0 {
			e.nonceCount -= len(nonces.nonceSeen)
			delete(e.senderNonces, key)
		}
	}
}

// PruneControlState bounds process-local session and nonce state. Winning
// tickets and uncertain broadcasts remain durable for redemption/recovery.
func (e *Engine) PruneControlState(ctx context.Context) error {
	snapshot, err := e.chain.Snapshot(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	for manifest, session := range e.sessions {
		if session.updated.Before(cutoff) {
			delete(e.sessions, manifest)
		}
	}
	if err != nil {
		return err
	}
	if snapshot.Block != nil {
		e.observeBlock(snapshot.Block)
	}
	return nil
}
