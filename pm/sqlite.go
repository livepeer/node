// Winning-ticket store operations follow livepeer/go-livepeer/common/db.go,
// by Elad Mallel and Nico Vergauwen (404d24a9455cd0997af1be6d18faa998702cce69).
// The redemption recovery schema and transaction state are implemented here.
package pm

import (
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	_ "modernc.org/sqlite"
)

// SQLiteStore owns the recipient's durable ticket queue and chain-watcher view.
// It is intentionally in pm: ticket state and redemption belong to the payment
// component, even when the orchestrator exposes the HTTP endpoint.
type SQLiteStore struct{ db *sql.DB }

func OpenSQLite(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("payment SQLite path is required")
	}
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("payment SQLite state requires a filesystem path")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("payment SQLite file is unavailable")
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("payment SQLite file must be owner-only")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		`CREATE TABLE IF NOT EXISTS winning_tickets (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			sender TEXT NOT NULL, recipient TEXT NOT NULL,
			face_value BLOB NOT NULL, win_prob BLOB NOT NULL,
			sender_nonce INTEGER NOT NULL, recipient_rand BLOB NOT NULL,
			recipient_rand_hash TEXT NOT NULL, sig BLOB NOT NULL UNIQUE,
			creation_round INTEGER NOT NULL, creation_round_block_hash TEXT NOT NULL,
			params_expiration_block TEXT NOT NULL,
			redeemed_at TEXT, tx_hash TEXT)`,
		"CREATE INDEX IF NOT EXISTS winning_tickets_pending ON winning_tickets(sender, creation_round, seq) WHERE tx_hash IS NULL",
		"CREATE INDEX IF NOT EXISTS winning_tickets_epoch ON winning_tickets(recipient_rand_hash)",
		"CREATE INDEX IF NOT EXISTS winning_tickets_liability ON winning_tickets(sender,creation_round) WHERE redeemed_at IS NULL",
		`CREATE TABLE IF NOT EXISTS orchestrator_rounds (
			address TEXT NOT NULL, round TEXT NOT NULL, active INTEGER NOT NULL,
			PRIMARY KEY(address, round))`,
		`CREATE TABLE IF NOT EXISTS redemption_attempts (
			sig BLOB PRIMARY KEY, attempted_at TEXT NOT NULL, error TEXT,
			phase TEXT NOT NULL, raw_transaction BLOB, key_address TEXT, nonce TEXT)`,
		"CREATE UNIQUE INDEX IF NOT EXISTS redemption_nonce ON redemption_attempts(key_address,nonce) WHERE nonce IS NOT NULL",
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize payment SQLite: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) StoreWinningTicket(t *SignedTicket) error {
	if err := validStoredTicket(t); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO winning_tickets
		(sender,recipient,face_value,win_prob,sender_nonce,recipient_rand,recipient_rand_hash,sig,creation_round,creation_round_block_hash,params_expiration_block)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, t.Sender.Hex(), t.Recipient.Hex(), t.FaceValue.Bytes(), t.WinProb.Bytes(),
		t.SenderNonce, t.RecipientRand.Bytes(), t.RecipientRandHash.Hex(), t.Sig, t.CreationRound,
		t.CreationRoundBlockHash.Hex(), t.ParamsExpirationBlock.String())
	return err
}

func validStoredTicket(t *SignedTicket) error {
	if t == nil || t.Ticket == nil || t.FaceValue == nil || t.WinProb == nil || t.ParamsExpirationBlock == nil || t.RecipientRand == nil || len(t.Sig) == 0 {
		return errors.New("incomplete signed ticket")
	}
	if t.FaceValue.Sign() <= 0 || t.WinProb.Sign() <= 0 || t.ParamsExpirationBlock.Sign() < 0 {
		return errors.New("invalid signed ticket values")
	}
	return nil
}

func (s *SQLiteStore) SelectEarliestWinningTicket(sender ethcommon.Address, minCreationRound int64) (*SignedTicket, error) {
	row := s.db.QueryRow(`SELECT recipient,face_value,win_prob,sender_nonce,recipient_rand,recipient_rand_hash,sig,creation_round,creation_round_block_hash,params_expiration_block
		FROM winning_tickets WHERE sender=? AND creation_round>=? AND tx_hash IS NULL AND sig NOT IN (SELECT sig FROM redemption_attempts) ORDER BY seq LIMIT 1`, sender.Hex(), minCreationRound)
	var recipient, randHash, blockHash, expiration string
	var face, prob, rand, sig []byte
	var nonce int64
	var round int64
	if err := row.Scan(&recipient, &face, &prob, &nonce, &rand, &randHash, &sig, &round, &blockHash, &expiration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	exp, ok := new(big.Int).SetString(expiration, 10)
	if !ok || nonce < 0 || nonce > 1<<32-1 {
		return nil, errors.New("corrupt winning ticket")
	}
	return &SignedTicket{Ticket: &Ticket{Sender: sender, Recipient: ethcommon.HexToAddress(recipient),
		FaceValue: new(big.Int).SetBytes(face), WinProb: new(big.Int).SetBytes(prob), SenderNonce: uint32(nonce),
		RecipientRandHash: ethcommon.HexToHash(randHash), CreationRound: round,
		CreationRoundBlockHash: ethcommon.HexToHash(blockHash), ParamsExpirationBlock: exp},
		Sig: sig, RecipientRand: new(big.Int).SetBytes(rand)}, nil
}

func (s *SQLiteStore) WinningTicketCount(sender ethcommon.Address, minCreationRound int64) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM winning_tickets WHERE sender=? AND creation_round>=? AND tx_hash IS NULL`, sender.Hex(), minCreationRound).Scan(&count)
	return count, err
}

type SubmittedRedemption struct {
	Signature []byte
	Hash      ethcommon.Hash
}

func (s *SQLiteStore) SubmittedRedemptions() ([]SubmittedRedemption, error) {
	rows, err := s.db.Query(`SELECT w.sig,w.tx_hash FROM winning_tickets w JOIN redemption_attempts a ON a.sig=w.sig WHERE w.tx_hash IS NOT NULL AND w.redeemed_at IS NULL AND a.phase NOT IN ('confirmed','reverted','expired')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SubmittedRedemption
	for rows.Next() {
		var sig []byte
		var raw string
		if err := rows.Scan(&sig, &raw); err != nil {
			return nil, err
		}
		if !ethcommon.IsHexHash(raw) {
			return nil, errors.New("corrupt redemption transaction hash")
		}
		result = append(result, SubmittedRedemption{Signature: sig, Hash: ethcommon.HexToHash(raw)})
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ConfirmRedemption(sig []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE winning_tickets SET redeemed_at=? WHERE sig=? AND tx_hash IS NOT NULL AND redeemed_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), sig)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("redemption is absent or already confirmed")
	}
	_, err = tx.Exec("UPDATE redemption_attempts SET phase='confirmed',error=NULL WHERE sig=?", sig)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) FailRedemption(sig []byte) error {
	result, err := s.db.Exec(`UPDATE redemption_attempts SET phase='reverted',error='transaction reverted' WHERE sig=? AND phase!='reverted'`, sig)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("redemption attempt is absent or already failed")
	}
	return nil
}

func (s *SQLiteStore) RemoveWinningTicket(t *SignedTicket) error {
	if t == nil || t.Ticket == nil || len(t.Sig) == 0 {
		return errors.New("incomplete signed ticket")
	}
	_, err := s.db.Exec("DELETE FROM winning_tickets WHERE sig=?", t.Sig)
	return err
}

// SetOrchestratorActive records an observed active status for a round; it is
// not a finality assertion or a substitute for canonical chain reads.
func (s *SQLiteStore) SetOrchestratorActive(addr ethcommon.Address, round *big.Int, active bool) error {
	if round == nil || round.Sign() < 0 {
		return errors.New("invalid round")
	}
	_, err := s.db.Exec(`INSERT INTO orchestrator_rounds(address,round,active) VALUES(?,?,?)
		ON CONFLICT(address,round) DO UPDATE SET active=excluded.active`, addr.Hex(), round.String(), active)
	return err
}

func (s *SQLiteStore) IsOrchActive(addr ethcommon.Address, round *big.Int) (bool, error) {
	if round == nil || round.Sign() < 0 {
		return false, errors.New("invalid round")
	}
	var active bool
	err := s.db.QueryRow("SELECT active FROM orchestrator_rounds WHERE address=? AND round=?", addr.Hex(), round.String()).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return active, err
}

func (s *SQLiteStore) PendingSenders() ([]ethcommon.Address, error) {
	rows, err := s.db.Query(`SELECT DISTINCT sender FROM winning_tickets WHERE tx_hash IS NULL AND sig NOT IN (SELECT sig FROM redemption_attempts)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ethcommon.Address
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if !ethcommon.IsHexAddress(raw) {
			return nil, errors.New("corrupt ticket sender")
		}
		result = append(result, ethcommon.HexToAddress(raw))
	}
	return result, rows.Err()
}

var _ TicketStore = (*SQLiteStore)(nil)
