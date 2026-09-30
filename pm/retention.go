package pm

import (
	"context"
	"time"
)

// PruneControlState removes replay guards only after their authentication
// window has ended. Winning tickets and uncertain broadcasts are retained.
func (s *SQLiteStore) PruneControlState(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"DELETE FROM used_payment_tickets WHERE recipient_rand_hash IN (SELECT hash FROM payment_ticket_epochs WHERE expires_at<?)",
		"DELETE FROM payment_ticket_epochs WHERE expires_at<?",
	} {
		if _, err := tx.ExecContext(ctx, statement, time.Now().Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM payment_challenges WHERE created_at<?", time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}
