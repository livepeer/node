package eth

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/livepeer/node/eth/contracts"
)

// Inspection pins all state and Controller lookups to one canonical block hash.
// Pending nonces are deliberately read from the transaction pool.
type Inspection struct {
	contracts   *Contracts
	reference   any
	addresses   map[string]common.Address
	BlockNumber string      `json:"block_number"`
	BlockHash   common.Hash `json:"block_hash"`
}

type BlockSnapshot struct {
	BlockNumber string      `json:"block_number"`
	BlockHash   common.Hash `json:"block_hash"`
}

func (c *Contracts) Inspect(ctx context.Context) (*Inspection, error) {
	header, err := c.RPC.header(ctx, "latest")
	if err != nil {
		return nil, err
	}
	if header.Number == nil || (*big.Int)(header.Number).Sign() < 0 || header.Hash == (common.Hash{}) {
		return nil, errors.New("invalid inspection block")
	}
	return &Inspection{contracts: c, reference: map[string]any{"blockHash": header.Hash.Hex(), "requireCanonical": true}, addresses: map[string]common.Address{}, BlockNumber: (*big.Int)(header.Number).String(), BlockHash: header.Hash}, nil
}

func (s *Inspection) Snapshot() BlockSnapshot { return BlockSnapshot{s.BlockNumber, s.BlockHash} }
func (s *Inspection) Resolve(ctx context.Context, name string) (common.Address, error) {
	if address, ok := s.addresses[name]; ok {
		return address, nil
	}
	address, err := s.contracts.ResolveAt(ctx, s.reference, name)
	if err == nil {
		s.addresses[name] = address
	}
	return address, err
}
func (s *Inspection) Call(ctx context.Context, name, method string, args ...any) ([]any, error) {
	address, err := s.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.contracts.CallAt(ctx, s.reference, name, address, method, args...)
}
func (s *Inspection) number(ctx context.Context, name, method string, args ...any) (*big.Int, error) {
	values, err := s.Call(ctx, name, method, args...)
	if err != nil {
		return nil, err
	}
	if value, ok := values[0].(*big.Int); ok {
		return value, nil
	}
	if value, ok := values[0].(uint64); ok {
		return new(big.Int).SetUint64(value), nil
	}
	return nil, errors.New("invalid numeric inspection result")
}
func (s *Inspection) bonding(ctx context.Context) (*contracts.BondingManagerCaller, error) {
	address, err := s.Resolve(ctx, "bondingManager")
	if err != nil {
		return nil, err
	}
	return contracts.NewBondingManagerCaller(address, s.contracts.callerAt(s.reference))
}

type AccountInspection struct {
	BlockSnapshot
	Address      common.Address `json:"address"`
	BalanceWei   string         `json:"balance_wei"`
	LPTBalance   string         `json:"lpt_balance_base_units"`
	PendingNonce uint64         `json:"pending_nonce"`
}

func (s *Inspection) Account(ctx context.Context, address common.Address) (AccountInspection, error) {
	result := AccountInspection{BlockSnapshot: s.Snapshot(), Address: address}
	var balance hexutil.Big
	if err := s.contracts.RPC.ethereum.Client().CallContext(ctx, &balance, "eth_getBalance", address, s.reference); err != nil {
		return result, safeRPCError(err)
	}
	result.BalanceWei = (*big.Int)(&balance).String()
	lpt, err := s.number(ctx, "livepeerToken", "balanceOf", address)
	if err != nil {
		return result, err
	}
	result.LPTBalance = lpt.String()
	result.PendingNonce, err = s.contracts.RPC.PendingNonceAt(ctx, address)
	return result, err
}

type StakeInspection struct {
	BlockSnapshot
	Address         common.Address `json:"address"`
	Status          string         `json:"status"`
	BondedStake     string         `json:"bonded_stake_base_units"`
	PendingStake    string         `json:"pending_stake_base_units"`
	CollectedFees   string         `json:"collected_fees_wei"`
	PendingFees     string         `json:"pending_fees_wei"`
	Delegate        common.Address `json:"delegate"`
	DelegatedAmount string         `json:"delegated_amount_base_units"`
	StartRound      string         `json:"start_round"`
	LastClaimRound  string         `json:"last_claim_round"`
	NextLockID      string         `json:"next_lock_id"`
}

func (s *Inspection) Stake(ctx context.Context, address common.Address) (StakeInspection, error) {
	result := StakeInspection{BlockSnapshot: s.Snapshot(), Address: address}
	caller, err := s.bonding(ctx)
	if err != nil {
		return result, err
	}
	opts := &bind.CallOpts{Context: ctx}
	d, err := caller.GetDelegator(opts, address)
	if err != nil {
		return result, err
	}
	status, err := caller.DelegatorStatus(opts, address)
	if err != nil {
		return result, err
	}
	if status > 2 {
		return result, errors.New("invalid delegator status")
	}
	result.Status = []string{"Pending", "Bonded", "Unbonded"}[status]
	result.BondedStake, result.CollectedFees, result.Delegate = d.BondedAmount.String(), d.Fees.String(), d.DelegateAddress
	result.DelegatedAmount, result.StartRound, result.LastClaimRound, result.NextLockID = d.DelegatedAmount.String(), d.StartRound.String(), d.LastClaimRound.String(), d.NextUnbondingLockId.String()
	round, err := s.number(ctx, "roundsManager", "currentRound")
	if err != nil {
		return result, err
	}
	stake, err := caller.PendingStake(opts, address, round)
	if err != nil {
		return result, err
	}
	fees, err := caller.PendingFees(opts, address, round)
	if err != nil {
		return result, err
	}
	result.PendingStake, result.PendingFees = stake.String(), fees.String()
	return result, nil
}

type LockInspection struct {
	ID            string `json:"id"`
	Amount        string `json:"amount_base_units"`
	WithdrawRound string `json:"withdraw_round"`
	Withdrawable  bool   `json:"withdrawable"`
}
type LocksInspection struct {
	BlockSnapshot
	Address    common.Address   `json:"address"`
	Locks      []LockInspection `json:"locks"`
	NextFromID *string          `json:"next_from_id"`
}
type LockQuery struct {
	FromID               *big.Int
	Limit                uint64
	Withdrawable, Locked bool
}

func (s *Inspection) Locks(ctx context.Context, address common.Address, query LockQuery) (LocksInspection, error) {
	result := LocksInspection{BlockSnapshot: s.Snapshot(), Address: address, Locks: []LockInspection{}}
	if query.Withdrawable && query.Locked {
		return result, errors.New("withdrawable and locked filters are mutually exclusive")
	}
	if query.Limit == 0 || query.Limit > 1000 {
		return result, errors.New("lock limit must be between 1 and 1000")
	}
	from := new(big.Int)
	if query.FromID != nil {
		from.Set(query.FromID)
	}
	if from.Sign() < 0 || from.BitLen() > 256 {
		return result, errors.New("invalid starting lock ID")
	}
	caller, err := s.bonding(ctx)
	if err != nil {
		return result, err
	}
	opts := &bind.CallOpts{Context: ctx}
	d, err := caller.GetDelegator(opts, address)
	if err != nil {
		return result, err
	}
	round, err := s.number(ctx, "roundsManager", "currentRound")
	if err != nil {
		return result, err
	}
	for scanned := uint64(0); scanned < query.Limit && from.Cmp(d.NextUnbondingLockId) < 0; scanned++ {
		lock, err := caller.GetDelegatorUnbondingLock(opts, address, from)
		if err != nil {
			return result, err
		}
		if lock.WithdrawRound.Sign() > 0 {
			eligible := round.Cmp(lock.WithdrawRound) >= 0
			if (!query.Withdrawable || eligible) && (!query.Locked || !eligible) {
				result.Locks = append(result.Locks, LockInspection{from.String(), lock.Amount.String(), lock.WithdrawRound.String(), eligible})
			}
		}
		from.Add(from, big.NewInt(1))
	}
	if from.Cmp(d.NextUnbondingLockId) < 0 {
		next := from.String()
		result.NextFromID = &next
	}
	return result, nil
}

type OrchestratorInspection struct {
	BlockSnapshot
	Address         common.Address `json:"address"`
	Registered      bool           `json:"registered"`
	Active          bool           `json:"active"`
	DelegatedStake  string         `json:"delegated_stake_base_units"`
	RewardCut       string         `json:"reward_cut_percent"`
	FeeCut          string         `json:"fee_cut_percent"`
	ServiceURI      string         `json:"service_uri"`
	LastRewardRound string         `json:"last_reward_round"`
	RewardCaller    common.Address `json:"reward_caller"`
}

// DecimalUnits formats integer units exactly, without floating-point arithmetic.
func DecimalUnits(value *big.Int, places int) string {
	negative := value.Sign() < 0
	digits := new(big.Int).Abs(value).String()
	for len(digits) <= places {
		digits = "0" + digits
	}
	if places > 0 {
		split := len(digits) - places
		fraction := digits[split:]
		for len(fraction) > 0 && fraction[len(fraction)-1] == '0' {
			fraction = fraction[:len(fraction)-1]
		}
		digits = digits[:split]
		if fraction != "" {
			digits += "." + fraction
		}
	}
	if negative {
		digits = "-" + digits
	}
	return digits
}
func (s *Inspection) Orchestrator(ctx context.Context, address common.Address) (OrchestratorInspection, error) {
	return s.orchestrator(ctx, address, nil)
}
func (s *Inspection) orchestrator(ctx context.Context, address common.Address, stake *big.Int) (OrchestratorInspection, error) {
	result := OrchestratorInspection{BlockSnapshot: s.Snapshot(), Address: address}
	caller, err := s.bonding(ctx)
	if err != nil {
		return result, err
	}
	opts := &bind.CallOpts{Context: ctx}
	result.Registered, err = caller.IsRegisteredTranscoder(opts, address)
	if err != nil {
		return result, err
	}
	result.Active, err = caller.IsActiveTranscoder(opts, address)
	if err != nil {
		return result, err
	}
	if stake == nil {
		stake, err = caller.TranscoderTotalStake(opts, address)
		if err != nil {
			return result, err
		}
	}
	result.DelegatedStake = stake.String()
	tr, err := caller.GetTranscoder(opts, address)
	if err != nil {
		return result, err
	}
	if tr.RewardCut.Cmp(big.NewInt(1000000)) > 0 || tr.FeeShare.Cmp(big.NewInt(1000000)) > 0 {
		return result, errors.New("invalid orchestrator commission")
	}
	result.RewardCut = DecimalUnits(tr.RewardCut, 4)
	result.FeeCut = DecimalUnits(new(big.Int).Sub(big.NewInt(1000000), tr.FeeShare), 4)
	result.LastRewardRound = tr.LastRewardRound.String()
	result.RewardCaller, err = caller.TranscoderToRewardCaller(opts, address)
	if err != nil {
		return result, err
	}
	uri, err := s.Call(ctx, "serviceRegistry", "getServiceURI", address)
	if err != nil {
		return result, err
	}
	result.ServiceURI = uri[0].(string)
	return result, nil
}

type PoolInspection struct {
	BlockSnapshot
	Orchestrators []OrchestratorInspection `json:"orchestrators"`
}

func (s *Inspection) Pool(ctx context.Context, activeOnly bool) (PoolInspection, error) {
	result := PoolInspection{BlockSnapshot: s.Snapshot(), Orchestrators: []OrchestratorInspection{}}
	caller, err := s.bonding(ctx)
	if err != nil {
		return result, err
	}
	pool, _, err := transcoderPool(&bind.CallOpts{Context: ctx}, caller)
	if err != nil {
		return result, err
	}
	for _, entry := range pool {
		member, err := s.orchestrator(ctx, entry.Address, entry.DelegatedStake)
		if err != nil {
			return result, err
		}
		if !activeOnly || member.Active {
			result.Orchestrators = append(result.Orchestrators, member)
		}
	}
	return result, nil
}

type RoundInspection struct {
	BlockSnapshot
	CurrentRound         string `json:"current_round"`
	LastInitializedRound string `json:"last_initialized_round"`
	Initialized          bool   `json:"initialized"`
	Locked               bool   `json:"locked"`
}

func (s *Inspection) Round(ctx context.Context) (RoundInspection, error) {
	result := RoundInspection{BlockSnapshot: s.Snapshot()}
	round, err := s.number(ctx, "roundsManager", "currentRound")
	if err != nil {
		return result, err
	}
	result.CurrentRound = round.String()
	last, err := s.number(ctx, "roundsManager", "lastInitializedRound")
	if err != nil {
		return result, err
	}
	result.LastInitializedRound = last.String()
	initialized, err := s.Call(ctx, "roundsManager", "currentRoundInitialized")
	if err != nil {
		return result, err
	}
	result.Initialized = initialized[0].(bool)
	locked, err := s.Call(ctx, "roundsManager", "currentRoundLocked")
	if err != nil {
		return result, err
	}
	result.Locked = locked[0].(bool)
	return result, nil
}

type ProtocolInspection struct {
	BlockSnapshot
	Paused            bool   `json:"paused"`
	PoolLimit         string `json:"pool_limit"`
	RoundLength       string `json:"round_length"`
	RoundLockAmount   string `json:"round_lock_amount"`
	UnbondingPeriod   string `json:"unbonding_period"`
	Inflation         string `json:"inflation"`
	InflationChange   string `json:"inflation_change"`
	TargetBondingRate string `json:"target_bonding_rate"`
	Supply            string `json:"supply_base_units"`
	TotalBonded       string `json:"total_bonded_base_units"`
	ParticipationRate string `json:"participation_rate_percent"`
}

func (s *Inspection) Protocol(ctx context.Context) (ProtocolInspection, error) {
	result := ProtocolInspection{BlockSnapshot: s.Snapshot()}
	paused, err := s.Call(ctx, "controller", "paused")
	if err != nil {
		return result, err
	}
	result.Paused = paused[0].(bool)
	for _, field := range []struct {
		name, method string
		target       *string
	}{
		{"bondingManager", "getTranscoderPoolMaxSize", &result.PoolLimit},
		{"roundsManager", "roundLength", &result.RoundLength}, {"roundsManager", "roundLockAmount", &result.RoundLockAmount},
		{"bondingManager", "unbondingPeriod", &result.UnbondingPeriod}, {"minter", "inflation", &result.Inflation}, {"minter", "inflationChange", &result.InflationChange}, {"minter", "targetBondingRate", &result.TargetBondingRate},
		{"minter", "getGlobalTotalSupply", &result.Supply}, {"bondingManager", "getTotalBonded", &result.TotalBonded},
	} {
		value, err := s.number(ctx, field.name, field.method)
		if err != nil {
			return result, err
		}
		*field.target = value.String()
	}
	supply, _ := new(big.Int).SetString(result.Supply, 10)
	bonded, _ := new(big.Int).SetString(result.TotalBonded, 10)
	rate := new(big.Int)
	if supply.Sign() > 0 {
		rate.Div(new(big.Int).Mul(bonded, big.NewInt(1000000)), supply)
	}
	result.ParticipationRate = DecimalUnits(rate, 4)
	return result, nil
}

type TicketBrokerInspection struct {
	BlockSnapshot
	Address                common.Address `json:"address"`
	Deposit                string         `json:"deposit_wei"`
	Reserve                string         `json:"reserve_wei"`
	WithdrawRound          string         `json:"withdraw_round"`
	UnlockPeriod           string         `json:"unlock_period"`
	FundingStatus          string         `json:"funding_status"`
	ProjectedWithdrawRound *string        `json:"projected_withdraw_round"`
}

func (s *Inspection) TicketBroker(ctx context.Context, address common.Address) (TicketBrokerInspection, error) {
	result := TicketBrokerInspection{BlockSnapshot: s.Snapshot(), Address: address}
	broker, err := s.Resolve(ctx, "ticketBroker")
	if err != nil {
		return result, err
	}
	caller, err := contracts.NewTicketBrokerCaller(broker, s.contracts.callerAt(s.reference))
	if err != nil {
		return result, err
	}
	opts := &bind.CallOpts{Context: ctx}
	info, err := caller.GetSenderInfo(opts, address)
	if err != nil {
		return result, err
	}
	period, err := caller.UnlockPeriod(opts)
	if err != nil {
		return result, err
	}
	round, err := s.number(ctx, "roundsManager", "currentRound")
	if err != nil {
		return result, err
	}
	result.Deposit, result.Reserve, result.WithdrawRound, result.UnlockPeriod = info.Sender.Deposit.String(), info.Reserve.FundsRemaining.String(), info.Sender.WithdrawRound.String(), period.String()
	switch {
	case info.Sender.Deposit.Sign() == 0 && info.Reserve.FundsRemaining.Sign() == 0:
		result.FundingStatus = "Empty"
	case info.Sender.WithdrawRound.Sign() == 0:
		result.FundingStatus = "Locked"
		projected := new(big.Int).Add(round, period).String()
		result.ProjectedWithdrawRound = &projected
	case round.Cmp(info.Sender.WithdrawRound) < 0:
		result.FundingStatus = "Unlocking"
	default:
		result.FundingStatus = "Unlocked"
	}
	return result, nil
}

type ContractsInspection struct {
	BlockSnapshot
	Addresses map[string]common.Address `json:"addresses"`
}

func (s *Inspection) Addresses(ctx context.Context) (ContractsInspection, error) {
	result := ContractsInspection{BlockSnapshot: s.Snapshot(), Addresses: map[string]common.Address{"controller": s.contracts.Controller}}
	for _, name := range []string{"bondingManager", "ticketBroker", "roundsManager", "serviceRegistry", "livepeerToken", "minter", "governor"} {
		address, err := s.Resolve(ctx, name)
		if err != nil {
			return result, err
		}
		result.Addresses[name] = address
	}
	return result, nil
}
