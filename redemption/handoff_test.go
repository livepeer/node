package redemption

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandoffDrainRequiresDurableAcknowledgementNotSettlement(t *testing.T) {
	s, binding := boundStore(t, "orchestrator", "https://redeemer.example", DefaultLimits)
	ticket, _ := winningTicket(t, 1)
	require.NoError(t, s.Admit(t.Context(), ticket))
	binding.Destination = "https://replacement.example"
	check := func(want bool) {
		t.Helper()
		drained, err := s.HandoffDrained(t.Context())
		require.NoError(t, err)
		require.Equal(t, want, drained)
	}
	for _, phase := range []string{"queued", "sending", "retryable", "rejected"} {
		require.NoError(t, s.delivery(t.Context(), ticket, phase, 0))
		check(false)
		require.ErrorContains(t, s.Bind(t.Context(), binding, DefaultLimits, nil), "drain")
	}
	require.NoError(t, s.delivery(t.Context(), ticket, "acknowledged", 0))
	check(true)
	var settled int
	require.NoError(t, s.db.QueryRow(`SELECT settled FROM ticket_state`).Scan(&settled))
	require.Zero(t, settled)
	require.NoError(t, s.Bind(t.Context(), binding, DefaultLimits, nil), "destination changes need handoff, not onchain confirmation")
	path := s.path
	require.NoError(t, s.Close())
	reopened, err := OpenRedeemerDB(path)
	require.NoError(t, err)
	defer reopened.Close()
	drained, err := reopened.HandoffDrained(t.Context())
	require.NoError(t, err)
	require.True(t, drained)
	require.NoError(t, reopened.Close())
	_, err = reopened.HandoffDrained(t.Context())
	require.Error(t, err, "unavailable storage cannot prove a completed handoff")
}
