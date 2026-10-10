// Package nodeconfig supplies shared CLI policy, data-directory defaults, and storage paths.
package nodeconfig

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/spf13/cobra"
)

const Mainnet = "arbitrum-one-mainnet"

var (
	Controller = common.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4")
	ETHUSDFeed = common.HexToAddress("0x639Fe6ab55C921f74e7fac1ee960C0B6293ba612")
)

type Settings struct {
	Network string `optional:"true" descr:"Network name (custom networks require explicit chain settings)"`
	DataDir string `basedir:"true" basepath:"source" optional:"true" descr:"Component data directory; defaults to ~/.lpData/<network>/<component>"`

	sharedKeystore bool
}

// PersistentSettings shares directory settings with nested commands.
type PersistentSettings struct{ Settings }

func (s *PersistentSettings) InitCtx(ctx *boa.HookContext) error {
	boa.Param(ctx, &s.Network).SetPersistent(true)
	boa.Param(ctx, &s.DataDir).SetPersistent(true)
	return nil
}

func (s *Settings) settings() *Settings { return s }

// KeystoreDir uses provenance retained by Command, including supplied paths
// equal to the calculated default. Direct callers supplying DataDir use it as-is.
func (s Settings) KeystoreDir() string {
	root := s.DataDir
	if s.sharedKeystore {
		root = filepath.Dir(root)
	}
	return filepath.Join(root, "keystore")
}

func init() {
	boa.RegisterConfigFormat(".toml", toml.Unmarshal)
	boa.RegisterConfigMarshaler(".toml", toml.Marshal)
}

func defaultDataDir(network, component string) (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".lpData", network, component), err
}

// Command applies shared source and discovery policy using Boa's merge pipeline.
// Parameters must embed Settings or PersistentSettings.
func Command[P any](component, network string, cmd boa.Cmd[P], config *string) boa.Cmd[P] {
	cmd.ParamEnrich = boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_"+strings.ToUpper(component)),
		func(_ []boa.Parameter, param boa.Parameter, name string) error {
			if param.IsBaseDir() {
				if dir, err := defaultDataDir(network, component); err == nil {
					param.SetDefault(dir)
				}
			} else if name == "Network" {
				param.SetDefault(network)
			}
			return nil
		})
	var automatic bool
	var discoveryNetwork string
	post := cmd.PostConfigFuncCtx
	cmd.PreConfigFuncCtx = func(ctx *boa.HookContext, p *P, _ *cobra.Command, _ []string) error {
		s := any(p).(interface{ settings() *Settings }).settings()
		automatic, discoveryNetwork = !ctx.HasInput(config), s.Network
		boa.Param(ctx, config).SetConfigFileOptionalDefault(false)
		boa.Param(ctx, config).SetConfigFileOptional(automatic)
		boa.Param(ctx, &s.DataDir).SetNoConfig(automatic)
		if automatic {
			return s.setDefaultDataDir(ctx, component)
		}
		return nil
	}
	cmd.PostConfigFuncCtx = func(ctx *boa.HookContext, p *P, c *cobra.Command, args []string) error {
		s := any(p).(interface{ settings() *Settings }).settings()
		if automatic && s.Network != discoveryNetwork {
			return boa.NewUserInputErrorf("config.toml sets network %q but the selected network is %q; pass --network %s or select the file with --config", s.Network, discoveryNetwork, s.Network)
		}
		if err := s.setDefaultDataDir(ctx, component); err != nil {
			return err
		}
		if post != nil {
			return post(ctx, p, c, args)
		}
		return nil
	}
	return cmd
}

func (s *Settings) setDefaultDataDir(ctx *boa.HookContext, component string) error {
	if !filepath.IsLocal(s.Network) || s.Network == "." || strings.ContainsAny(s.Network, `/\`) {
		return boa.NewUserInputErrorf("network %q must be a nonempty directory name, not a path; set --network to a name such as %s or offchain", s.Network, Mainnet)
	}
	s.sharedKeystore = !ctx.HasInput(&s.DataDir)
	if !s.sharedKeystore {
		return nil
	}
	var err error
	s.DataDir, err = defaultDataDir(s.Network, component)
	if err != nil {
		return boa.NewUserInputErrorf("cannot determine the default data directory: %v; set HOME or pass --data-dir", err)
	}
	return nil
}

// Default fills an absent value after configuration has selected the network.
func Default[T any](ctx *boa.HookContext, field *T, value T) {
	if !ctx.HasValue(field) {
		*field = value
	}
}
