package migratecli_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/migrations"
	"github.com/livepeer/node/migrations/migratecli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

var files = fstest.MapFS{
	"migrations/001_initial.sql": {Data: []byte("-- UP\nCREATE TABLE items(id INTEGER PRIMARY KEY);\n-- DOWN\nDROP TABLE items;")},
}

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		name, env, defaultDB, path, action, wantError string
		args                                          []string
	}{
		{name: "up flag before action", args: []string{"--store-db", "flag.sqlite", "up"}, path: "flag.sqlite", action: "up"},
		{name: "down flag after action", args: []string{"down", "--store-db", "flag.sqlite"}, path: "flag.sqlite", action: "down"},
		{name: "status from environment", args: []string{"status"}, env: "env.sqlite", path: "env.sqlite", action: "status"},
		{name: "default", args: []string{"up"}, defaultDB: "default.sqlite", path: "default.sqlite", action: "up"},
		{name: "environment overrides default", args: []string{"status"}, env: "env.sqlite", defaultDB: "default.sqlite", path: "env.sqlite", action: "status"},
		{name: "flag overrides environment", args: []string{"up", "--store-db", "flag.sqlite"}, env: "env.sqlite", defaultDB: "default.sqlite", path: "flag.sqlite", action: "up"},
		{name: "missing database", args: []string{"up"}, wantError: "missing required param 'store-db'"},
		{name: "unexpected argument", args: []string{"up", "extra"}, defaultDB: "default.sqlite", wantError: "unknown command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_MIGRATIONS_DB", tc.env)
			var path string
			var create bool
			var calls int
			db, err := sql.Open("sqlite", ":memory:")
			require.NoError(t, err)
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			if tc.action == "down" {
				require.NoError(t, migrations.Up(t.Context(), db, fstest.MapFS{"001_initial.sql": files["migrations/001_initial.sql"]}))
			}
			command := migratecli.Command("store-db", "TEST_MIGRATIONS_DB", tc.defaultDB, files, func(ctx context.Context, dbPath string, mayCreate bool) (*sql.DB, error) {
				require.Equal(t, t.Context(), ctx)
				path, create = dbPath, mayCreate
				calls++
				return db, nil
			})
			// Service credentials stay local to the root; migration children run
			// only their own group's persistent sourcing and validation pipeline.
			type serviceParams struct{ Credential string }
			root := (boa.Cmd[serviceParams]{Use: "service", SubCmds: []*cobra.Command{command}}).ToCobra()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SilenceUsage, root.SilenceErrors = true, true
			root.SetContext(t.Context())
			root.SetArgs(append([]string{"migrate"}, tc.args...))
			err = root.Execute()
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Zero(t, calls)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, tc.path, path)
			require.Equal(t, tc.action == "up", create)
			var status []migrations.Status
			require.NoError(t, json.Unmarshal(output.Bytes(), &status))
			require.Len(t, status, 1)
			require.Equal(t, tc.action == "up", status[0].Applied)
			require.ErrorContains(t, db.PingContext(t.Context()), "database is closed")
		})
	}
}

func TestCommandOpenErrorAndHelp(t *testing.T) {
	wantError := errors.New("database open failed")
	command := migratecli.Command("store-db", "TEST_MIGRATIONS_DB", "default.sqlite", files, func(context.Context, string, bool) (*sql.DB, error) {
		return nil, wantError
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	command.SetArgs([]string{"up"})
	require.ErrorIs(t, command.Execute(), wantError)
	require.Empty(t, output.String())
	command.SetArgs([]string{"down", "--help"})
	require.NoError(t, command.Execute())
	for _, want := range []string{"destroys data", "Global Flags:", "--store-db", "TEST_MIGRATIONS_DB", "default.sqlite"} {
		require.Contains(t, output.String(), want)
	}
}
