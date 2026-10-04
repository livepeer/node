package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm"
)

// SavedNonceFloor reserves every signed identity across process restarts,
// including confirmed/reverted attempts that an RPC may omit from pending.
func (s *RedeemerDB) SavedNonceFloor(ctx context.Context, address ethcommon.Address) (uint64, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT nonce FROM redemption_attempts WHERE redeemer_address=? AND nonce IS NOT NULL ORDER BY length(nonce) DESC, nonce DESC LIMIT 1", address.Hex()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	nonce, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || nonce == math.MaxUint64 {
		return 0, errors.New("invalid or exhausted saved redemption nonce")
	}
	return nonce + 1, nil
}

func (s *RedeemerDB) recordPrepared(ctx context.Context, ticket *pm.SignedTicket, prepared eth.SignedTransaction) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var blocked int
	if err := tx.QueryRow("SELECT COUNT(*) FROM redemption_attempts WHERE phase IN ('prepared','broadcast')").Scan(&blocked); err != nil {
		return err
	}
	if blocked != 0 {
		return errors.New("unresolved redemption must be reconciled before preparing another")
	}
	_, err = tx.Exec("INSERT INTO redemption_attempts(sig,attempted_at,phase,raw_transaction,redeemer_address,nonce) VALUES(?,?,'prepared',?,?,?)", ticket.Sig, time.Now().UTC().Format(time.RFC3339Nano), prepared.Raw, prepared.From.Hex(), strconv.FormatUint(prepared.Nonce, 10))
	if err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE winning_tickets SET tx_hash=? WHERE sig=?", prepared.Hash.Hex(), ticket.Sig)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RedemptionStatus is safe to display: it excludes recipient randomness,
// signatures, and signed transaction bytes.
type RedemptionStatus struct {
	Hash  string `json:"hash,omitempty"`
	Phase string `json:"phase"`
	Error string `json:"error,omitempty"`
}

func (s *RedeemerDB) Redemptions() ([]RedemptionStatus, error) {
	rows, err := s.db.Query("SELECT COALESCE(w.tx_hash,''),a.phase,COALESCE(a.error,'') FROM redemption_attempts a JOIN winning_tickets w ON w.sig=a.sig ORDER BY a.attempted_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RedemptionStatus{}
	for rows.Next() {
		var item RedemptionStatus
		if err := rows.Scan(&item.Hash, &item.Phase, &item.Error); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// RetryRedemption is an explicit operator action after an uncertain broadcast.
// It can only send the identical stored transaction, preserving its nonce/hash.
func RetryRedemption(ctx context.Context, store *RedeemerDB, contracts *eth.Contracts, hash ethcommon.Hash) error {
	var sig, raw []byte
	var phase string
	err := store.db.QueryRow("SELECT a.sig,a.raw_transaction,a.phase FROM redemption_attempts a JOIN winning_tickets w ON w.sig=a.sig WHERE w.tx_hash=?", hash.Hex()).Scan(&sig, &raw, &phase)
	if err != nil {
		return err
	}
	if len(raw) == 0 || (phase != "prepared" && phase != "broadcast" && phase != "submitted") {
		return errors.New("redemption has no retryable signed transaction")
	}
	return broadcastRedemption(ctx, store, contracts, sig, eth.SignedTransaction{Hash: hash, Raw: raw})
}

func broadcastRedemption(ctx context.Context, store *RedeemerDB, contracts *eth.Contracts, sig []byte, tx eth.SignedTransaction) error {
	result, err := store.db.Exec("UPDATE redemption_attempts SET phase='broadcast',error=NULL WHERE sig=? AND phase IN ('prepared','broadcast','submitted')", sig)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("redemption is already resolved")
	}
	if err := contracts.Broadcast(ctx, tx); err != nil {
		_, dbErr := store.db.Exec("UPDATE redemption_attempts SET error=? WHERE sig=? AND phase='broadcast'", err.Error(), sig)
		return errors.Join(fmt.Errorf("transaction %s broadcast outcome uncertain: %w", tx.Hash.Hex(), err), dbErr)
	}
	_, err = store.db.Exec("UPDATE redemption_attempts SET phase='submitted',error=NULL WHERE sig=? AND phase='broadcast'", sig)
	return err
}
