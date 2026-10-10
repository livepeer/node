package nodeconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/j0sh/boa/pkg/boa"
	"github.com/stretchr/testify/require"
)

func TestDirectorySelection(t *testing.T) {
	type params struct {
		ConfigFile string `name:"config" configfile:"optional-default" basepath:"source" default:"config.toml" boa:"noconfig"`
		Settings
		InputFile string `file:"true" optional:"true" basepath:"source"`
		Storage   string `file:"optional" default:"state.sqlite"`
	}
	const defaultDir = ".lpData/" + Mainnet + "/example"
	absolute := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(absolute, "input"), nil, 0600))
	for _, tc := range []struct {
		name, dir, network, input, storage, discovered, wantError string
		args                                                      []string
		env                                                       map[string]string
		supplied, noHome                                          bool
	}{
		{name: "defaults"},
		{name: "network environment", env: map[string]string{"NETWORK": "custom"}, network: "custom", dir: ".lpData/custom/example"},
		{name: "network flag overrides environment", args: []string{"--network", "offchain"}, env: map[string]string{"NETWORK": "custom"}, network: "offchain", dir: ".lpData/offchain/example"},
		{name: "explicit config network", args: []string{"--config", "configs/network.toml"}, network: "custom", dir: ".lpData/custom/example"},
		{name: "environment config", env: map[string]string{"CONFIG": "configs/network.toml"}, network: "custom", dir: ".lpData/custom/example"},
		{name: "config paths", args: []string{"--config", "configs/data.toml"}, network: "custom", dir: "data", input: "configs/input", supplied: true},
		{name: "JSON paths without HOME", args: []string{"--config", "configs/data.json"}, network: "custom", dir: "data", input: "configs/input", supplied: true, noHome: true},
		{name: "environment overrides config", args: []string{"--config", "configs/data.toml"}, env: map[string]string{"DATA_DIR": "env", "NETWORK": "offchain", "INPUT_FILE": "input"}, network: "offchain", dir: "env", input: "input", supplied: true},
		{name: "flags override environment and config", args: []string{"--config", "configs/data.toml", "--data-dir", "cli", "--network", Mainnet, "--input-file", "input"}, env: map[string]string{"DATA_DIR": "env", "NETWORK": "offchain", "CONFIG": "missing.toml", "INPUT_FILE": "missing"}, dir: "cli", input: "input", supplied: true},
		{name: "absolute paths", args: []string{"--config", "configs/data.toml", "--data-dir", absolute, "--input-file", filepath.Join(absolute, "input"), "--storage", filepath.Join(absolute, "db")}, network: "custom", dir: absolute, input: filepath.Join(absolute, "input"), storage: filepath.Join(absolute, "db"), supplied: true},
		{name: "supplied default flag", args: []string{"--data-dir", defaultDir}, supplied: true},
		{name: "supplied default environment", env: map[string]string{"DATA_DIR": defaultDir}, supplied: true},
		{name: "supplied default config", args: []string{"--config", "configs/equal.toml"}, supplied: true},
		{name: "empty datadir uses cwd", args: []string{"--data-dir=", "--config="}, dir: ".", supplied: true},
		{name: "disable environment config", args: []string{"--config="}, env: map[string]string{"CONFIG": "missing.toml"}},
		{name: "missing explicit default config", args: []string{"--config", defaultDir + "/config.toml"}, wantError: "no such file"},
		{name: "missing environment config", env: map[string]string{"CONFIG": defaultDir + "/config.toml"}, wantError: "no such file"},
		{name: "discovered input", discovered: "InputFile = 'input'", input: defaultDir + "/input"},
		{name: "discovery mismatch", discovered: "Network = 'custom'", wantError: "pass --network custom or select the file with --config"},
		{name: "discovery mismatch with explicit datadir", discovered: "Network = 'custom'", args: []string{"--data-dir", "data"}, dir: "data", wantError: "pass --network custom or select the file with --config"},
		{name: "CLI overrides discovered network", discovered: "Network = 'custom'", args: []string{"--network", Mainnet}},
		{name: "environment overrides discovered network", discovered: "Network = 'custom'", env: map[string]string{"NETWORK": Mainnet}},
		{name: "discovered datadir forbidden", discovered: "DataDir = 'elsewhere'", wantError: `config key "DataDir" is forbidden`},
		{name: "malformed config", discovered: "[", wantError: "configfile"},
		{name: "unknown config key", discovered: "Unknown = true", wantError: "unknown"},
		{name: "disable malformed discovery", discovered: "[", args: []string{"--config="}},
		{name: "missing HOME", noHome: true, wantError: "set HOME or pass --data-dir"},
		{name: "explicit datadir without HOME", noHome: true, args: []string{"--data-dir", "data"}, dir: "data", supplied: true},
		{name: "empty network", args: []string{"--network="}, wantError: "nonempty directory name"},
		{name: "dot network", args: []string{"--network", "."}, wantError: "nonempty directory name"},
		{name: "parent network before discovery", args: []string{"--network", "..", "--data-dir", "data"}, dir: "data", discovered: "[", wantError: "nonempty directory name"},
		{name: "network path from environment", env: map[string]string{"NETWORK": "../../outside"}, wantError: "nonempty directory name"},
		{name: "network subdirectory", args: []string{"--network", "a/b"}, wantError: "nonempty directory name"},
		{name: "backslash network", args: []string{"--network", `a\b`}, wantError: "nonempty directory name"},
		{name: "explicit config network path", args: []string{"--config", "configs/bad.toml"}, wantError: "nonempty directory name"},
		{name: "effective config network", args: []string{"--config", "configs/bad.toml", "--network", Mainnet}, dir: "data", supplied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Chdir(home)
			t.Setenv("HOME", home)
			for _, name := range []string{"CONFIG", "NETWORK", "DATA_DIR", "INPUT_FILE", "STORAGE"} {
				t.Setenv("LIVEPEER_EXAMPLE_"+name, tc.env[name])
			}
			write := func(path, content string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
				require.NoError(t, os.WriteFile(path, []byte(content), 0600))
			}
			write("configs/network.toml", "Network = 'custom'")
			write("configs/data.toml", "Network = 'custom'\nDataDir = '../data'\nInputFile = 'input'\nStorage = 'state.sqlite'")
			write("configs/data.json", `{"Network":"custom","DataDir":"../data","InputFile":"input","Storage":"state.sqlite"}`)
			write("configs/equal.toml", "DataDir = '../"+defaultDir+"'")
			write("configs/bad.toml", "Network = '..'\nDataDir = '../data'")
			write("configs/input", "input")
			write("input", "input")
			if tc.dir == "" {
				tc.dir = defaultDir
			}
			if tc.discovered != "" {
				write(filepath.Join(tc.dir, "config.toml"), tc.discovered)
				write(filepath.Join(tc.dir, "input"), "input")
			}
			if tc.noHome {
				t.Setenv("HOME", "")
			}
			var p params
			err := Command("example", Mainnet, boa.Cmd[params]{Params: &p, RawArgs: tc.args, RejectUnknown: true}, &p.ConfigFile).Validate()
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			if tc.network == "" {
				tc.network = Mainnet
			}
			require.Equal(t, tc.network, p.Network)
			dir, err := filepath.Abs(tc.dir)
			require.NoError(t, err)
			require.Equal(t, dir, p.DataDir)
			keyRoot := dir
			if !tc.supplied {
				keyRoot = filepath.Dir(dir)
			}
			require.Equal(t, filepath.Join(keyRoot, "keystore"), p.KeystoreDir())
			if tc.storage == "" {
				tc.storage = filepath.Join(dir, "state.sqlite")
			}
			require.Equal(t, tc.storage, p.Storage)
			if tc.input != "" {
				tc.input, err = filepath.Abs(tc.input)
				require.NoError(t, err)
			}
			require.Equal(t, tc.input, p.InputFile)
			if tc.discovered == "" {
				require.NoDirExists(t, filepath.Join(home, ".lpData"), "configuration must not create storage")
			}
		})
	}
}
