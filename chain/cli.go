package chain

import (
	"context"
	"io"
	"strings"

	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/version"
	"github.com/spf13/cobra"
)

type transactionParams interface {
	transactionOptions() TransactionOptions
}

// transactionCommand keeps sourcing and validation local to the leaf's type.
func transactionCommand[T transactionParams](root *RootParams, command string, build func(T) ([]action, error), prepare prepareActions) boa.Cmd[T] {
	return boa.Cmd[T]{
		Use: command[strings.LastIndex(command, " ")+1:], Short: "Simulate or explicitly submit " + command,
		InitFuncCtx: func(ctx *boa.HookContext, _ *T, cmd *cobra.Command) error {
			for _, param := range ctx.AllMirrors() {
				if param.IsRequired() {
					param.SetRequiredFn(func() bool {
						printing, _ := cmd.Flags().GetBool("print-config")
						return !printing
					})
				}
			}
			return nil
		},
		PostCreateFunc: func(_ *T, cmd *cobra.Command) error {
			validateArgs := cmd.Args
			cmd.Args = func(cmd *cobra.Command, args []string) error {
				printing, _ := cmd.Flags().GetBool("print-config")
				if printing && len(args) == 0 {
					return nil
				}
				return validateArgs(cmd, args)
			}
			return nil
		},
		RunFuncE: func(p *T, cmd *cobra.Command, _ []string) error {
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
			return executeActions(cmd.Context(), root.OperatorParams, root.DisplayOptions, tx, out, command, actions, prepare)
		},
	}
}

func staticTransaction(root *RootParams, command, contract, method string) boa.Cmd[TransactionOptions] {
	return transactionCommand(root, command, func(TransactionOptions) ([]action, error) {
		return contractAction(contract, method), nil
	}, nil)
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

func Root(out, errOut io.Writer) *cobra.Command {
	params := new(RootParams)
	operatorEnv := boa.ParamEnricherCombine(boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_CHAIN"))
	root := (boa.Cmd[RootParams]{
		Use: "livepeer-chain", Short: "Direct Livepeer Ethereum management", Version: version.String(),
		Params: params, RejectUnknown: true, Args: cobra.NoArgs,
		ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, func(previous []boa.Parameter, param boa.Parameter, name string) error {
			if param.IsNoEnv() {
				return nil
			}
			return operatorEnv(previous, param, name)
		}),
		RunFuncE: func(p *RootParams, cmd *cobra.Command, _ []string) error {
			if p.PrintConfig {
				return p.OperatorParams.printConfig(cmd.OutOrStdout())
			}
			return cmd.Help()
		},
		SubCmds: boa.SubCmds(
			readCommand(params, "status", "Read the configured Ethereum chain status", Status),
			readCommand(params, "account", "Read the sender's ETH balance and pending nonce", Account),
			boa.Cmd[boa.NoParams]{Use: "orchestrator", Short: "Manage orchestrator configuration", SubCmds: boa.SubCmds(
				readCommand(params, "get", "Read orchestrator status and configuration", orchestratorGet),
				transactionCommand(params, "orchestrator activate", ActivateParams.actions, nil),
				transactionCommand(params, "orchestrator set-config", SetConfigParams.actions, nil),
				staticTransaction(params, "orchestrator reward", "bondingManager", "reward"),
			)},
			boa.Cmd[boa.NoParams]{Use: "stake", Short: "Manage delegated stake", SubCmds: boa.SubCmds(
				transactionCommand(params, "stake bond", BondParams.actions, prepareBond),
				transactionCommand(params, "stake unbond", UnbondParams.actions, nil),
				transactionCommand(params, "stake rebond", RebondParams.actions, nil),
				transactionCommand(params, "stake withdraw", WithdrawStakeParams.actions, nil),
			)},
			boa.Cmd[boa.NoParams]{Use: "earnings", Short: "Claim earnings and withdraw fees", SubCmds: boa.SubCmds(
				transactionCommand(params, "earnings claim", ClaimParams.actions, nil),
				transactionCommand(params, "earnings withdraw-fees", WithdrawFeesParams.actions, nil),
			)},
			boa.Cmd[boa.NoParams]{Use: "ticketbroker", Short: "Manage ticket broker funds", SubCmds: boa.SubCmds(
				transactionCommand(params, "ticketbroker fund", FundParams.actions, nil),
				staticTransaction(params, "ticketbroker unlock", "ticketBroker", "unlock"),
				staticTransaction(params, "ticketbroker cancel-unlock", "ticketBroker", "cancelUnlock"),
				staticTransaction(params, "ticketbroker withdraw", "ticketBroker", "withdraw"),
			)},
			boa.Cmd[boa.NoParams]{Use: "round", Short: "Manage protocol rounds", SubCmds: boa.SubCmds(
				staticTransaction(params, "round initialize", "roundsManager", "initializeRound"),
			)},
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
