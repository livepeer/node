package chain

import (
	"errors"
	"io"
	"net/url"
	"time"

	"github.com/BurntSushi/toml"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/destination"
)

// OperatorParams is the persistent operator context shared by chain commands.
type OperatorParams struct {
	Network              string             `optional:"true" persistent:"true" descr:"Network name (custom networks require explicit chain settings)"`
	DataDir              string             `basedir:"true" optional:"true" persistent:"true" descr:"Data directory; relative file paths start here"`
	RPCURL               *url.URL           `name:"rpc-url" secret:"true" optional:"true" persistent:"true"`
	RPCURLFile           string             `name:"rpc-url-file" secretfor:"RPCURL" persistent:"true"`
	ChainID              *uint64            `min:"1" persistent:"true" descr:"Optional expected RPC chain ID"`
	Account              *ethcommon.Address `persistent:"true" descr:"Account address to inspect or use for transactions; must match the keystore account when signing"`
	Controller           ethcommon.Address  `name:"controller-address" optional:"true" persistent:"true" descr:"Livepeer Controller address"`
	KeystoreFile         string             `optional:"true" persistent:"true" descr:"Encrypted geth account JSON file"`
	KeystorePassword     *string            `secret:"true" optional:"true" persistent:"true" descr:"Keystore decryption password"`
	KeystorePasswordFile string             `secretfor:"KeystorePassword" persistent:"true" descr:"Owner-only file containing the exact keystore password bytes"`
	MaxFeePerGas         *uint256.Int       `persistent:"true" descr:"Ceiling on the calculated transaction fee cap in wei per gas"`
}

func (p OperatorParams) Validate() error {
	if p.Network != "" && p.ChainID == nil {
		return errors.New("network requires an explicit chain-id and controller-address")
	}
	if err := destination.ValidateURL(p.RPCURL); err != nil {
		return errors.New("valid RPC URL is required")
	}
	if p.Controller == (ethcommon.Address{}) {
		return errors.New("controller-address must be nonzero")
	}
	if p.MaxFeePerGas != nil && p.MaxFeePerGas.IsZero() {
		return errors.New("max-fee-per-gas must be positive")
	}
	return nil
}

// printConfig deliberately includes only audited operator values.
func (p OperatorParams) printConfig(out io.Writer) error {
	data, err := toml.Marshal(struct {
		Network      string
		ChainID      *uint64
		Account      *ethcommon.Address
		Controller   ethcommon.Address
		MaxFeePerGas *uint256.Int
	}{p.Network, p.ChainID, p.Account, p.Controller, p.MaxFeePerGas})
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

// DisplayOptions are invocation controls, never persistent configuration.
type DisplayOptions struct {
	Output      string `default:"text" alts:"text,json" persistent:"true" boa:"noconfig,noenv"`
	PrintConfig bool   `optional:"true" persistent:"true" boa:"noconfig,noenv" descr:"Print audited operator configuration and exit"`
}

type RootParams struct {
	ConfigFile string `name:"config" configfile:"optional-default" default:"chain/config.toml" persistent:"true" boa:"noconfig" descr:"Configuration file; empty disables discovery"`
	OperatorParams
	DisplayOptions
}

// TransactionOptions are invocation controls, never TOML/environment values.
type TransactionOptions struct {
	Submit                     bool          `optional:"true" boa:"noconfig,noenv" descr:"Broadcast transactions (default: dry-run)"`
	Wait                       bool          `default:"true" boa:"noconfig,noenv" descr:"Wait for successful inclusion when submitting"`
	NoWait                     bool          `optional:"true" boa:"noconfig,noenv" descr:"Broadcast only; single transactions without replacements"`
	Quiet                      bool          `optional:"true" boa:"noconfig,noenv" descr:"Suppress normal transaction output"`
	GasLimit                   *uint64       `min:"1" boa:"noconfig,noenv" descr:"Gas units for every transaction; otherwise estimate each. Gas limit times fee cap bounds the gas fee"`
	MaxPriorityFeePerGas       *uint256.Int  `boa:"noconfig,noenv" descr:"Initial tip cap in wei per gas; otherwise use RPC suggestion"`
	TransactionTimeout         time.Duration `default:"3m" boa:"noconfig,noenv" descr:"Confirmation interval for each attempt"`
	MaxTransactionReplacements uint64        `default:"0" boa:"noconfig,noenv" descr:"Maximum replacements while waiting for inclusion"`
}

func (p TransactionOptions) transactionOptions() TransactionOptions { return p }

type AmountOptions struct {
	Amount    string `required:"true" boa:"noconfig,noenv" descr:"Amount in human LPT/ETH units, or all where supported"`
	BaseUnits bool   `optional:"true" boa:"noconfig,noenv" descr:"Interpret numeric amounts as integer token base units or wei"`
}
type LockOptions struct {
	LockID uint256.Int `required:"true" boa:"noconfig,noenv"`
}
type PercentageOptions struct {
	RewardCut *string `boa:"noconfig,noenv" descr:"Reward commission percentage, 0–100 with up to four decimals"`
	FeeCut    *string `boa:"noconfig,noenv" descr:"Fee commission percentage, 0–100 with up to four decimals"`
}
type SetConfigParams struct {
	TransactionOptions
	PercentageOptions
	ServiceURI boa.Text[*url.URL] `optional:"true" boa:"noconfig,noenv"`
}
type RegisterParams struct {
	TransactionOptions
	PercentageOptions
	BondOptions
	LockID     *uint256.Int       `boa:"noconfig,noenv" descr:"Cancel an unbonding lock into self-delegation"`
	ServiceURI boa.Text[*url.URL] `optional:"true" boa:"noconfig,noenv"`
}
type BondOptions struct {
	Amount     string `optional:"true" boa:"noconfig,noenv" descr:"Wallet LPT to bond: human units or all"`
	BaseUnits  bool   `optional:"true" boa:"noconfig,noenv"`
	Redelegate bool   `optional:"true" boa:"noconfig,noenv" descr:"Move existing stake without adding tokens"`
}
type BondParams struct {
	TransactionOptions
	BondOptions
	Orchestrator ethcommon.Address `positional:"true" required:"true" boa:"noconfig,noenv" descr:"Target orchestrator address"`
}
type UnbondParams struct {
	TransactionOptions
	AmountOptions
}
type CancelUnbondParams struct {
	TransactionOptions
	LockOptions
	Delegate *ethcommon.Address `positional:"true" optional:"true" boa:"noconfig,noenv" descr:"Required when unbonded; otherwise the existing delegate"`
}
type WithdrawStakeParams struct {
	TransactionOptions
	LockOptions
}
type ClaimParams struct {
	TransactionOptions
	EndRound *uint256.Int `boa:"noconfig,noenv" descr:"Last round to claim; default: current initialized round"`
}
type WithdrawFeesParams struct {
	TransactionOptions
	AmountOptions
	Recipient *ethcommon.Address `boa:"noconfig,noenv" descr:"Withdrawal recipient; default: configured account"`
}
type FundParams struct {
	TransactionOptions
	AmountOptions
	Reserve string `required:"true" boa:"noconfig,noenv" descr:"Reserve amount in human ETH units, or wei with --base-units"`
}
type TransferParams struct {
	TransactionOptions
	AmountOptions
	Recipient ethcommon.Address `positional:"true" required:"true" boa:"noconfig,noenv"`
}
type RewardParams struct {
	TransactionOptions
	Orchestrator *ethcommon.Address `boa:"noconfig,noenv" descr:"Target orchestrator; default: configured account"`
}
type RewardCallerParams struct {
	TransactionOptions
	Address ethcommon.Address `positional:"true" required:"true" boa:"noconfig,noenv"`
}
type PollVoteParams struct {
	TransactionOptions
	Address ethcommon.Address `positional:"true" required:"true" boa:"noconfig,noenv"`
	Choice  string            `positional:"true" required:"true" alts:"yes,no" boa:"noconfig,noenv"`
}
type ProposalVoteParams struct {
	TransactionOptions
	ID     uint256.Int `positional:"true" required:"true" boa:"noconfig,noenv"`
	Choice string      `positional:"true" required:"true" alts:"against,for,abstain" boa:"noconfig,noenv"`
	Reason *string     `boa:"noconfig,noenv"`
}
type LocksParams struct {
	FromID       uint256.Int `default:"0" boa:"noconfig,noenv"`
	Limit        uint64      `default:"100" min:"1" max:"1000" boa:"noconfig,noenv" descr:"Maximum lock IDs scanned per page"`
	Withdrawable bool        `optional:"true" boa:"noconfig,noenv"`
	Locked       bool        `optional:"true" boa:"noconfig,noenv"`
}
type ListParams struct {
	Active bool `optional:"true" boa:"noconfig,noenv"`
}
type MessageParams struct {
	MessageFile string `file:"true" required:"true" boa:"noconfig,noenv"`
}
type TypedDataParams struct {
	DataFile string `file:"true" required:"true" boa:"noconfig,noenv"`
}

type GasParams struct {
	MaxPriorityFeePerGas *uint256.Int `boa:"noconfig,noenv" descr:"Explicit initial tip cap in wei per gas"`
}
