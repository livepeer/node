package chain

import (
	"bytes"
	"strings"
	"testing"

	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

func TestApprovedChainCommandsAndContractEncoding(t *testing.T) {
	const address = "0x0123456789abcdef0123456789abcdef01234567"
	params := Params{Sender: address, Controller: address, Amount: "100", Reserve: "20", Delegate: address, Recipient: address, LockID: "3", EndRound: "10", RewardCut: "100000", FeeShare: "900000", ServiceURI: "https://orchestrator.example"}
	contracts, err := eth.OpenContracts(nil, address)
	require.NoError(t, err)
	commands := []string{
		"status", "account", "orchestrator get", "orchestrator activate", "orchestrator set-config", "orchestrator reward",
		"stake bond", "stake unbond", "stake rebond", "stake withdraw", "earnings claim", "earnings withdraw-fees",
		"ticketbroker fund", "ticketbroker unlock", "ticketbroker cancel-unlock", "ticketbroker withdraw", "round initialize",
	}
	for _, name := range commands {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			command, _, err := root.Find(splitCommand(name))
			require.NoError(t, err)
			require.Equal(t, nameLast(name), command.Name())
			if name == "status" || name == "account" || name == "orchestrator get" {
				return
			}
			actions, err := buildActions(name, params)
			require.NoError(t, err)
			require.NotEmpty(t, actions)
			for _, action := range actions {
				_, err := contracts.Pack(action.Contract, action.Method, action.Args...)
				require.NoError(t, err)
			}
		})
	}
}

func splitCommand(value string) []string { return strings.Fields(value) }
func nameLast(value string) string       { parts := strings.Fields(value); return parts[len(parts)-1] }
