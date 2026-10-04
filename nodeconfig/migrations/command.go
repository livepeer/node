package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"

	"github.com/j0sh/boa/pkg/boa"
	"github.com/spf13/cobra"
)

type params struct {
	DB string `persistent:"true"`
}

// Command creates migration subcommands for files containing migrations/*.sql.
// open must not run migrations. Only up requests creation; Command closes the database.
func Command(flag, env, defaultDB string, files fs.FS, open func(context.Context, string, bool) (*sql.DB, error)) *cobra.Command {
	p := new(params)
	run := func(_ *boa.NoParams, cmd *cobra.Command, _ []string) error {
		catalog, err := fs.Sub(files, "migrations")
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		db, err := open(ctx, p.DB, cmd.Name() == "up")
		if err != nil {
			return err
		}
		defer db.Close()
		switch cmd.Name() {
		case "up":
			err = Up(ctx, db, catalog)
		case "down":
			err = Down(ctx, db, catalog)
		}
		if err != nil {
			return err
		}
		status, err := List(ctx, db, catalog)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	return (boa.Cmd[params]{
		Use: "migrate", Short: "Manage embedded SQL migrations", Params: p, Args: cobra.NoArgs,
		InitFuncCtx: func(ctx *boa.HookContext, p *params, _ *cobra.Command) error {
			db := boa.Param(ctx, &p.DB)
			db.SetName(flag)
			db.SetEnv(env)
			db.SetDescription("SQLite database path")
			if defaultDB != "" {
				db.SetDefault(defaultDB)
			}
			return nil
		},
		SubCmds: boa.SubCmds(
			boa.Cmd[boa.NoParams]{Use: "up", Short: "Apply pending migrations", Args: cobra.NoArgs, RunFuncE: run},
			boa.Cmd[boa.NoParams]{Use: "down", Short: "Roll back the latest migration; destroys data", Args: cobra.NoArgs, RunFuncE: run},
			boa.Cmd[boa.NoParams]{Use: "status", Short: "Inspect migration status", Args: cobra.NoArgs, RunFuncE: run},
		),
	}).ToCobra()
}
