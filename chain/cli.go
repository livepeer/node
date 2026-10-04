package chain

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/version"
	"github.com/spf13/cobra"
)

type transactionParams interface {
	transactionOptions() TransactionOptions
}

// invocationCommand allows auditing configuration without action inputs.
func invocationCommand[T any](command boa.Cmd[T]) boa.Cmd[T] {
	init := command.InitFuncCtx
	command.InitFuncCtx = func(ctx *boa.HookContext, p *T, cmd *cobra.Command) error {
		if init != nil {
			if err := init(ctx, p, cmd); err != nil {
				return err
			}
		}
		for _, param := range ctx.AllMirrors() {
			if param.IsRequired() {
				param.SetRequiredFn(func() bool { printing, _ := cmd.Flags().GetBool("print-config"); return !printing })
			}
		}
		return nil
	}
	postCreate := command.PostCreateFunc
	command.PostCreateFunc = func(p *T, cmd *cobra.Command) error {
		if postCreate != nil {
			if err := postCreate(p, cmd); err != nil {
				return err
			}
		}
		validateArgs := cmd.Args
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			printing, _ := cmd.Flags().GetBool("print-config")
			if printing && len(args) == 0 {
				return nil
			}
			return validateArgs(cmd, args)
		}
		return nil
	}
	return command
}

func transactionCommand[T transactionParams](root *RootParams, command string, build func(T) ([]action, error)) boa.Cmd[T] {
	return invocationCommand(boa.Cmd[T]{Use: command[strings.LastIndex(command, " ")+1:], Short: "Simulate or explicitly submit " + command, RunFuncE: func(p *T, cmd *cobra.Command, _ []string) error {
		tx, out := (*p).transactionOptions(), cmd.OutOrStdout()
		if tx.Quiet {
			out = io.Discard
		}
		if root.PrintConfig {
			return root.OperatorParams.printConfig(out)
		}
		actions, err := build(*p)
		if err != nil {
			return boa.NewUserInputError(err)
		}
		return executeActions(cmd.Context(), root.OperatorParams, root.DisplayOptions, tx, out, command, actions)
	}})
}

func signingCommand[T any](root *RootParams, use, short string, path func(T) string, typed bool) boa.Cmd[T] {
	return invocationCommand(boa.Cmd[T]{Use: use, Short: short, Args: cobra.NoArgs, RunFuncE: func(p *T, cmd *cobra.Command, _ []string) error {
		if root.PrintConfig {
			return root.OperatorParams.printConfig(cmd.OutOrStdout())
		}
		return signFile(root.OperatorParams, root.DisplayOptions, cmd.OutOrStdout(), path(*p), typed)
	}})
}

func gasCommand(root *RootParams) boa.Cmd[GasParams] {
	return boa.Cmd[GasParams]{Use: "get", Short: "Read base fee, tip and calculated fee cap", Args: cobra.NoArgs, RunFuncE: func(p *GasParams, cmd *cobra.Command, _ []string) error {
		if root.PrintConfig {
			return root.OperatorParams.printConfig(cmd.OutOrStdout())
		}
		return gasGet(cmd.Context(), root.OperatorParams, root.DisplayOptions, cmd.OutOrStdout(), *p)
	}}
}

func staticTransaction(root *RootParams, command, contract, method string) boa.Cmd[TransactionOptions] {
	return transactionCommand(root, command, func(TransactionOptions) ([]action, error) {
		return contractAction(contract, method), nil
	})
}

func readCommand(root *RootParams, use, short string, run func(context.Context, OperatorParams, DisplayOptions, io.Writer) error) boa.Cmd[boa.NoParams] {
	return boa.Cmd[boa.NoParams]{
		Use: use, Short: short, Args: cobra.NoArgs,
		RunFuncE: func(_ *boa.NoParams, cmd *cobra.Command, _ []string) error {
			if root.PrintConfig {
				return root.OperatorParams.printConfig(cmd.OutOrStdout())
			}
			return run(cmd.Context(), root.OperatorParams, root.DisplayOptions, cmd.OutOrStdout())
		},
	}
}

func accountCommand(root *RootParams) boa.Cmd[boa.NoParams] {
	command := boa.Cmd[boa.NoParams]{Use: "account", Short: "Inspect accounts or create an encrypted keystore"}
	command.SubCmds = boa.SubCmds(
		readCommand(root, "get", "Read ETH/LPT balances and pending nonce", Account),
		boa.Cmd[boa.NoParams]{Use: "create", Short: "Create a new encrypted keystore locally without overwriting an existing file",
			Long: "Create a new encrypted keystore locally. --keystore-file is the new output file.\n" +
				"--keystore-password-file is an existing owner-only input file containing the\n" +
				"encryption password; this command does not create a password file or prompt\n" +
				"for a password. Alternatively, supply LIVEPEER_CHAIN_KEYSTORE_PASSWORD.\n" +
				"The output's parent directory must exist. No RPC or account address is needed.\n\n" +
				"Example:\n  umask 077\n" +
				"  openssl rand -hex 32 | tr -d '\\n' > account.password\n" +
				"  livepeer-chain account create --keystore-file account.json --keystore-password-file account.password",
			Args: cobra.NoArgs, RunFuncE: func(_ *boa.NoParams, cmd *cobra.Command, _ []string) error {
				if root.PrintConfig {
					return root.OperatorParams.printConfig(cmd.OutOrStdout())
				}
				address, err := eth.CreateKeystore(root.KeystoreFile, root.KeystorePassword, root.KeystorePasswordFile)
				if err != nil {
					return err
				}
				return formatResult(cmd.OutOrStdout(), root.Output, map[string]string{"address": address.Hex()})
			}},
	)
	return groupCommand(root, command)
}

func Root(out, errOut io.Writer) *cobra.Command {
	params := new(RootParams)
	root := (boa.Cmd[RootParams]{
		Use: "livepeer-chain", Short: "Direct Livepeer Ethereum management", Version: version.String(),
		Params: params, RejectUnknown: true, Args: cobra.NoArgs,
		ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_CHAIN")),
		RunFuncE: func(p *RootParams, cmd *cobra.Command, _ []string) error {
			if p.PrintConfig {
				return p.OperatorParams.printConfig(cmd.OutOrStdout())
			}
			return cmd.Help()
		},
		SubCmds: boa.SubCmds(
			readCommand(params, "status", "Read the configured Ethereum chain status", Status),
			accountCommand(params),
			readCommand(params, "contracts", "Read Controller-resolved contract addresses", contractsGet),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "protocol", Short: "Inspect protocol settings", SubCmds: boa.SubCmds(readCommand(params, "get", "Read protocol settings and participation", protocolGet))}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "gas", Short: "Inspect transaction gas prices", SubCmds: boa.SubCmds(gasCommand(params))}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "reward", Short: "Call orchestrator rewards", SubCmds: boa.SubCmds(transactionCommand(params, "reward call", RewardParams.actions))}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "token", Short: "Manage wallet LPT", SubCmds: boa.SubCmds(transactionCommand(params, "token transfer", TransferParams.actions))}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "governance", Short: "Vote on polls and proposals", SubCmds: boa.SubCmds(
				groupCommand(params, boa.Cmd[boa.NoParams]{Use: "poll", SubCmds: boa.SubCmds(transactionCommand(params, "governance poll vote", PollVoteParams.actions))}),
				groupCommand(params, boa.Cmd[boa.NoParams]{Use: "proposal", SubCmds: boa.SubCmds(transactionCommand(params, "governance proposal vote", ProposalVoteParams.actions))}),
			)}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "sign", Short: "Sign files locally with the configured keystore", SubCmds: boa.SubCmds(
				signingCommand(params, "message", "Sign exact message-file bytes with the Ethereum message prefix", func(p MessageParams) string { return p.MessageFile }, false),
				signingCommand(params, "typed-data", "Sign EIP-712 typed JSON", func(p TypedDataParams) string { return p.DataFile }, true),
			)}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "orchestrator", Short: "Manage orchestrator configuration", SubCmds: boa.SubCmds(
				readCommand(params, "get", "Read orchestrator status and configuration", orchestratorGet),
				transactionCommand(params, "orchestrator register", RegisterParams.actions),
				transactionCommand(params, "orchestrator set-config", SetConfigParams.actions),
				inspectionCommand(params, "list", "List the current orchestrator pool", func(ctx context.Context, p ListParams, s *eth.Inspection) (any, error) { return s.Pool(ctx, p.Active) }),
				groupCommand(params, boa.Cmd[boa.NoParams]{Use: "reward-caller", Short: "Manage delegated reward authorization", SubCmds: boa.SubCmds(
					readCommand(params, "get", "Read the configured orchestrator’s reward caller", rewardCallerGet),
					transactionCommand(params, "orchestrator reward-caller set", RewardCallerParams.actions),
					transactionCommand(params, "orchestrator reward-caller unset", func(TransactionOptions) ([]action, error) { return rewardCallerActions(common.Address{}), nil }),
				)}),
			)}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "stake", Short: "Manage delegated stake", SubCmds: boa.SubCmds(
				readCommand(params, "get", "Read delegator stake and earnings", stakeGet),
				locksCommand(params),
				transactionCommand(params, "stake bond", BondParams.actions),
				transactionCommand(params, "stake unbond", UnbondParams.actions),
				transactionCommand(params, "stake cancel-unbond", CancelUnbondParams.actions),
				transactionCommand(params, "stake withdraw", WithdrawStakeParams.actions),
			)}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "earnings", Short: "Claim earnings and withdraw fees", SubCmds: boa.SubCmds(
				transactionCommand(params, "earnings claim", ClaimParams.actions),
				transactionCommand(params, "earnings withdraw-fees", WithdrawFeesParams.actions),
			)}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "ticketbroker", Short: "Manage ticket broker funds", SubCmds: boa.SubCmds(
				readCommand(params, "get", "Read TicketBroker funding and withdrawal status", ticketBrokerGet),
				transactionCommand(params, "ticketbroker fund", FundParams.actions),
				staticTransaction(params, "ticketbroker unlock", "ticketBroker", "unlock"),
				staticTransaction(params, "ticketbroker cancel-unlock", "ticketBroker", "cancelUnlock"),
				staticTransaction(params, "ticketbroker withdraw", "ticketBroker", "withdraw"),
			)}),
			groupCommand(params, boa.Cmd[boa.NoParams]{Use: "round", Short: "Manage protocol rounds", SubCmds: boa.SubCmds(
				readCommand(params, "get", "Read protocol round status", roundGet),
				staticTransaction(params, "round initialize", "roundsManager", "initializeRound"),
			)}),
		),
	}).ToCobra()
	// Boa tracks CLI presence through the declaring command's Flags(). Keep
	// the same persistent flag objects there so CLI still wins over env/config
	// when Cobra parses them on a descendant.
	root.Flags().AddFlagSet(root.PersistentFlags())
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.SetOut(out)
	root.SetErr(errOut)
	root.InitDefaultCompletionCmd()
	return root
}

func inspectionCommand[T any](root *RootParams, use, short string, run func(context.Context, T, *eth.Inspection) (any, error)) boa.Cmd[T] {
	return boa.Cmd[T]{Use: use, Short: short, Args: cobra.NoArgs, RunFuncE: func(p *T, cmd *cobra.Command, _ []string) error {
		if root.PrintConfig {
			return root.OperatorParams.printConfig(cmd.OutOrStdout())
		}
		return inspectRead(cmd.Context(), root.OperatorParams, root.DisplayOptions, cmd.OutOrStdout(), func(s *eth.Inspection) (any, error) { return run(cmd.Context(), *p, s) })
	}}
}

func groupCommand(root *RootParams, command boa.Cmd[boa.NoParams]) boa.Cmd[boa.NoParams] {
	command.Args = cobra.NoArgs
	command.RunFuncE = func(_ *boa.NoParams, cmd *cobra.Command, _ []string) error {
		if root.PrintConfig {
			return root.OperatorParams.printConfig(cmd.OutOrStdout())
		}
		return cmd.Help()
	}
	return command
}

func locksCommand(root *RootParams) boa.Cmd[LocksParams] {
	command := inspectionCommand(root, "locks", "List outstanding unbonding locks", func(ctx context.Context, p LocksParams, s *eth.Inspection) (any, error) {
		a, err := root.account()
		if err != nil {
			return nil, err
		}
		return s.Locks(ctx, a, eth.LockQuery{FromID: p.FromID.ToBig(), Limit: p.Limit, Withdrawable: p.Withdrawable, Locked: p.Locked})
	})
	command.PreValidateFunc = func(p *LocksParams, _ *cobra.Command, _ []string) error {
		if !root.PrintConfig && p.Withdrawable && p.Locked {
			return boa.NewUserInputError(errors.New("withdrawable and locked filters are mutually exclusive"))
		}
		return nil
	}
	return command
}
