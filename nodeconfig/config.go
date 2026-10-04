// Package nodeconfig supplies shared CLI policy, data-directory defaults, and storage paths.
package nodeconfig

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common"
	"github.com/j0sh/boa/pkg/boa"
)

const Mainnet = "arbitrum-one-mainnet"

var (
	Controller = common.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4")
	ETHUSDFeed = common.HexToAddress("0x639Fe6ab55C921f74e7fac1ee960C0B6293ba612")
)

type Settings struct {
	Network string `optional:"true" descr:"Network name (custom networks require explicit chain settings)"`
	DataDir string `basedir:"true" optional:"true" descr:"Data directory; relative file paths start here"`
}

func init() {
	boa.RegisterConfigFormat(".toml", toml.Unmarshal)
	boa.RegisterConfigMarshaler(".toml", toml.Marshal)
}

// Command supplies the shared CLI policy and a fixed storage default.
func Command[P any](component, network string, cmd boa.Cmd[P]) boa.Cmd[P] {
	cmd.ParamEnrich = boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_"+strings.ToUpper(component)),
		func(_ []boa.Parameter, param boa.Parameter, name string) error {
			if param.IsBaseDir() {
				if home, err := os.UserHomeDir(); err == nil {
					param.SetDefault(filepath.Join(home, ".lpData", network))
				} else {
					param.SetRequired(true)
				}
			} else if name == "Network" {
				param.SetDefault(network)
			}
			return nil
		})
	return cmd
}

// Default fills an absent value after configuration has selected the network.
func Default[T any](ctx *boa.HookContext, field *T, value T) {
	if !ctx.HasValue(field) {
		*field = value
	}
}

func Path(root, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}
