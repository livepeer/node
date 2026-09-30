package orchestrator

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRegressionPaymentConfigurationRequiresKey(t *testing.T) {
	p := Params{Listen: "127.0.0.1:8935", MetricsListen: "127.0.0.1:8936", ServiceURL: "http://127.0.0.1:8935", BootstrapSecret: "test", HeartbeatInterval: time.Second, HeartbeatTTL: time.Minute, PaymentDB: "payment.sqlite", PaymentRPCURL: "https://rpc.example", PaymentChainID: "1", PaymentController: "0x0000000000000000000000000000000000000001", WeiPerUSD: "1", TicketFaceValue: "1", TicketWinProb: "1"}
	require.Error(t, p.Validate(), "without payment-key-file the complete payment configuration silently starts off-chain")
}
