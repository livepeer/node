package migrations_test

import (
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/livepeer/node/migrations"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func catalog() fs.FS {
	return fstest.MapFS{
		"001_initial.sql": {Data: []byte("-- UP\nCREATE TABLE first_items (id INTEGER PRIMARY KEY);\n-- DOWN\nDROP TABLE first_items;")},
		"002_second.sql":  {Data: []byte("-- UP\nCREATE TABLE second_items (id INTEGER PRIMARY KEY);\n-- DOWN\nDROP TABLE second_items;")},
	}
}

func TestMigrationRoundTrip(t *testing.T) {
	ctx, db, files := t.Context(), openDB(t), catalog()
	status, err := migrations.List(ctx, db, files)
	require.NoError(t, err)
	require.Len(t, status, 2)
	require.False(t, status[0].Applied)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='migrations'`).Scan(&count))
	require.Zero(t, count, "status must not create metadata")

	require.NoError(t, migrations.Up(ctx, db, files))
	status, err = migrations.List(ctx, db, files)
	require.NoError(t, err)
	for i, item := range status {
		require.Equal(t, i+1, item.Version)
		require.Len(t, item.SHA256, 64)
		require.True(t, item.Applied)
		require.NotNil(t, item.AppliedAtMS)
		require.Positive(t, *item.AppliedAtMS)
	}
	_, err = db.Exec(`INSERT INTO first_items VALUES (7)`)
	require.NoError(t, err)
	require.NoError(t, migrations.Up(ctx, db, files))
	repeated, err := migrations.List(ctx, db, files)
	require.NoError(t, err)
	require.Equal(t, status, repeated, "repeated up must preserve metadata")

	require.NoError(t, migrations.Down(ctx, db, files))
	status, err = migrations.List(ctx, db, files)
	require.NoError(t, err)
	require.True(t, status[0].Applied)
	require.False(t, status[1].Applied)
	require.NoError(t, db.QueryRow(`SELECT id FROM first_items`).Scan(&count))
	require.Equal(t, 7, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='second_items'`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, migrations.Up(ctx, db, files))
	require.NoError(t, migrations.Down(ctx, db, files))
	require.NoError(t, migrations.Down(ctx, db, files))
	require.NoError(t, migrations.Down(ctx, db, files), "down with no applied versions is a no-op")
	require.NoError(t, migrations.Up(ctx, db, files))
}

func TestFailedMetadataWriteRollsBackEntireMigration(t *testing.T) {
	ctx, db, files := t.Context(), openDB(t), catalog()
	require.NoError(t, migrations.Up(ctx, db, files))
	require.NoError(t, migrations.Down(ctx, db, files))
	_, err := db.Exec(`CREATE TRIGGER fail_metadata BEFORE INSERT ON migrations BEGIN SELECT RAISE(ABORT,'fixture failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, migrations.Up(ctx, db, files), "fixture failure")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='second_items'`).Scan(&count))
	require.Zero(t, count, "failed metadata write must roll back schema")
	_, err = db.Exec(`DROP TRIGGER fail_metadata`)
	require.NoError(t, err)
	require.NoError(t, migrations.Up(ctx, db, files))
	_, err = db.Exec(`CREATE TRIGGER fail_metadata BEFORE DELETE ON migrations BEGIN SELECT RAISE(ABORT,'fixture failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, migrations.Down(ctx, db, files), "fixture failure")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='second_items'`).Scan(&count))
	require.Equal(t, 1, count, "failed metadata delete must roll back dropped schema")
	status, err := migrations.List(ctx, db, files)
	require.NoError(t, err)
	require.True(t, status[1].Applied)
}

func TestMigrationHistoryMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, statement, want string
		args                  []any
	}{
		{"filename", `UPDATE migrations SET filename='001_other.sql' WHERE version=1`, "filename mismatch", nil},
		{"checksum", `UPDATE migrations SET sha256=? WHERE version=1`, "checksum mismatch", []any{strings.Repeat("0", 64)}},
		{"unknown version", `INSERT INTO migrations VALUES(999,'999_unknown.sql',?,0)`, "unknown migration", []any{strings.Repeat("0", 64)}},
		{"history gap", `DELETE FROM migrations WHERE version=1`, "history has a gap", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db, files := t.Context(), openDB(t), catalog()
			require.NoError(t, migrations.Up(ctx, db, files))
			_, err := db.Exec(tc.statement, tc.args...)
			require.NoError(t, err)
			_, err = migrations.List(ctx, db, files)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, migrations.Up(ctx, db, files), tc.want)
			require.ErrorContains(t, migrations.Down(ctx, db, files), tc.want)
		})
	}
}

func TestInvalidMigrationCatalog(t *testing.T) {
	valid := []byte("-- UP\nSELECT 1;\n-- DOWN\nSELECT 1;")
	for _, tc := range []struct {
		name, want string
		files      fstest.MapFS
	}{
		{"filename", "invalid migration filename", fstest.MapFS{"1_initial.sql": {Data: valid}}},
		{"zero", "invalid migration version", fstest.MapFS{"000_initial.sql": {Data: valid}}},
		{"gap", "contiguous from 1", fstest.MapFS{"002_initial.sql": {Data: valid}}},
		{"duplicate", "contiguous from 1", fstest.MapFS{"001_a.sql": {Data: valid}, "001_b.sql": {Data: valid}}},
		{"missing down", "invalid UP/DOWN", fstest.MapFS{"001_initial.sql": {Data: []byte("-- UP\nSELECT 1;")}}},
		{"duplicate down", "invalid UP/DOWN", fstest.MapFS{"001_initial.sql": {Data: append(append([]byte(nil), valid...), []byte("\n-- DOWN")...)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := migrations.List(t.Context(), openDB(t), tc.files)
			require.ErrorContains(t, err, tc.want)
		})
	}
}
