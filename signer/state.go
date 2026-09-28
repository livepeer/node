package signer

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type stateStore struct {
	mu sync.Mutex
	db *sql.DB
}

func openStateStore(path string) (*stateStore, error) {
	if path == "" {
		return nil, errors.New("signer state-db is required")
	}
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("signer state requires a filesystem path")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("signer state file is unavailable")
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("signer state file must be owner-only")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", `CREATE TABLE IF NOT EXISTS payment_states(state_id TEXT PRIMARY KEY, sequence INTEGER NOT NULL, request_hash BLOB NOT NULL, response BLOB NOT NULL)`} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &stateStore{db: db}, nil
}

func (s *stateStore) close() error { return s.db.Close() }

var errStateConflict = errors.New("payment state is stale or already advanced")

// apply returns the exact prior response for an identical replay. A concurrent
// request advancing the same state with a different body is rejected.
func (s *stateStore) apply(id string, oldSequence int64, request []byte, makeResponse func() ([]byte, error)) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := sha256.Sum256(request)
	var storedSequence int64
	var storedDigest, storedResponse []byte
	err := s.db.QueryRow("SELECT sequence,request_hash,response FROM payment_states WHERE state_id=?", id).Scan(&storedSequence, &storedDigest, &storedResponse)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if storedSequence == oldSequence+1 && string(storedDigest) == string(digest[:]) {
			return storedResponse, nil
		}
		if storedSequence != oldSequence {
			return nil, errStateConflict
		}
	} else if oldSequence != -1 {
		return nil, errStateConflict
	}
	response, err := makeResponse()
	if err != nil {
		return nil, err
	}
	if oldSequence == -1 {
		_, err = s.db.Exec("INSERT INTO payment_states(state_id,sequence,request_hash,response) VALUES(?,?,?,?)", id, 0, digest[:], response)
	} else {
		result, updateErr := s.db.Exec("UPDATE payment_states SET sequence=?,request_hash=?,response=? WHERE state_id=? AND sequence=?", oldSequence+1, digest[:], response, id, oldSequence)
		if updateErr != nil {
			return nil, updateErr
		}
		var n int64
		n, err = result.RowsAffected()
		if err == nil && n != 1 {
			err = errStateConflict
		}
	}
	if err != nil {
		return nil, err
	}
	return response, nil
}
