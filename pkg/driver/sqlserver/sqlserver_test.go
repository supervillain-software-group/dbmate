package sqlserver

import (
	"database/sql"
	"io"
	"net/url"
	"testing"

	"github.com/amacneil/dbmate/v2/pkg/dbmate"
	"github.com/amacneil/dbmate/v2/pkg/dbutil"
	"github.com/stretchr/testify/require"
)

func testURL(t *testing.T) *url.URL {
	u, err := url.Parse("sqlserver://sa:Password123@localhost:1433/dbmate_test")
	require.NoError(t, err)
	return u
}

func prepTestDB(t *testing.T) *sql.DB {
	drv := NewDriver(dbmate.DriverConfig{
		DatabaseURL:         testURL(t),
		MigrationsTableName: "schema_migrations",
		Log:                 io.Discard,
	})

	// drop any existing database
	err := drv.DropDatabase()
	require.NoError(t, err)

	// create database
	err = drv.CreateDatabase()
	require.NoError(t, err)

	// connect to database
	db, err := drv.Open()
	require.NoError(t, err)

	return db
}

func TestConnectionString(t *testing.T) {
	u := testURL(t)
	connStr := connectionString(u)
	require.Equal(t, "sqlserver://sa:Password123@localhost:1433?database=dbmate_test", connStr)
}

func TestCreateDropDatabase(t *testing.T) {
	drv := NewDriver(dbmate.DriverConfig{
		DatabaseURL:         testURL(t),
		MigrationsTableName: "schema_migrations",
		Log:                 io.Discard,
	})

	// drop any existing database
	err := drv.DropDatabase()
	require.NoError(t, err)

	// create database
	err = drv.CreateDatabase()
	require.NoError(t, err)

	// check that database exists
	exists, err := drv.DatabaseExists()
	require.NoError(t, err)
	require.True(t, exists)

	// drop database
	err = drv.DropDatabase()
	require.NoError(t, err)

	// check that database no longer exists
	exists, err = drv.DatabaseExists()
	require.NoError(t, err)
	require.False(t, exists)
}

func TestDumpSchema(t *testing.T) {
	db := prepTestDB(t)
	defer dbutil.MustClose(db)

	// create schema_migrations table
	_, err := db.Exec(`
		CREATE TABLE schema_migrations (
			version VARCHAR(128) PRIMARY KEY
		);
		INSERT INTO schema_migrations (version)
		VALUES ('20200227231541');
	`)
	require.NoError(t, err)

	// create another table
	_, err = db.Exec(`
		CREATE TABLE users (
			id INT IDENTITY(1,1) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			email VARCHAR(255) UNIQUE
		);
	`)
	require.NoError(t, err)

	drv := NewDriver(dbmate.DriverConfig{
		DatabaseURL:         testURL(t),
		MigrationsTableName: "schema_migrations",
		Log:                 io.Discard,
	})

	// dump schema
	schema, err := drv.DumpSchema(db)
	require.NoError(t, err)

	schemaStr := string(schema)

	// Check header
	require.Contains(t, schemaStr, "-- SQL Server Schema Dump")

	// Check schema_migrations table
	require.Contains(t, schemaStr, "CREATE TABLE [dbo].[schema_migrations]")
	require.Contains(t, schemaStr, "[version] VARCHAR(128)")
	require.Contains(t, schemaStr, "PRIMARY KEY")

	// Check users table
	require.Contains(t, schemaStr, "CREATE TABLE [dbo].[users]")
	require.Contains(t, schemaStr, "[id] INT")
	require.Contains(t, schemaStr, "IDENTITY(1,1)")
	require.Contains(t, schemaStr, "PRIMARY KEY")
	require.Contains(t, schemaStr, "[name] VARCHAR(255) NOT NULL")
	require.Contains(t, schemaStr, "[email] VARCHAR(255)")

	// Check unique index
	require.Contains(t, schemaStr, "CREATE UNIQUE INDEX")
	require.Contains(t, schemaStr, "ON [dbo].[users] ([email])")

	// Check migrations data
	require.Contains(t, schemaStr, "-- Dbmate schema migrations")
	require.Contains(t, schemaStr, "INSERT INTO [schema_migrations] (version) VALUES")
	require.Contains(t, schemaStr, "('20200227231541')")
}

func TestMigrationsTableExists(t *testing.T) {
	db := prepTestDB(t)
	defer dbutil.MustClose(db)

	drv := NewDriver(dbmate.DriverConfig{
		DatabaseURL:         testURL(t),
		MigrationsTableName: "schema_migrations",
		Log:                 io.Discard,
	})

	// table should not exist
	exists, err := drv.MigrationsTableExists(db)
	require.NoError(t, err)
	require.False(t, exists)

	// create table
	err = drv.CreateMigrationsTable(db)
	require.NoError(t, err)

	// table should exist
	exists, err = drv.MigrationsTableExists(db)
	require.NoError(t, err)
	require.True(t, exists)
}

func TestSelectMigrations(t *testing.T) {
	db := prepTestDB(t)
	defer dbutil.MustClose(db)

	drv := NewDriver(dbmate.DriverConfig{
		DatabaseURL:         testURL(t),
		MigrationsTableName: "schema_migrations",
		Log:                 io.Discard,
	})

	err := drv.CreateMigrationsTable(db)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO schema_migrations (version)
		VALUES ('1'), ('2'), ('3');
	`)
	require.NoError(t, err)

	migrations, err := drv.SelectMigrations(db, -1)
	require.NoError(t, err)
	require.Equal(t, true, migrations["1"])
	require.Equal(t, true, migrations["2"])
	require.Equal(t, true, migrations["3"])

	// test limit param
	migrations, err = drv.SelectMigrations(db, 1)
	require.NoError(t, err)
	require.Equal(t, true, migrations["3"])
	require.Equal(t, false, migrations["2"])
	require.Equal(t, false, migrations["1"])
}

func TestInsertMigration(t *testing.T) {
	db := prepTestDB(t)
	defer dbutil.MustClose(db)

	drv := NewDriver(dbmate.DriverConfig{
		DatabaseURL:         testURL(t),
		MigrationsTableName: "schema_migrations",
		Log:                 io.Discard,
	})

	err := drv.CreateMigrationsTable(db)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	err = drv.InsertMigration(tx, "test")
	require.NoError(t, err)
	err = tx.Commit()
	require.NoError(t, err)

	migrations, err := drv.SelectMigrations(db, -1)
	require.NoError(t, err)
	require.Equal(t, true, migrations["test"])
}

func TestDeleteMigration(t *testing.T) {
	db := prepTestDB(t)
	defer dbutil.MustClose(db)

	drv := NewDriver(dbmate.DriverConfig{
		DatabaseURL:         testURL(t),
		MigrationsTableName: "schema_migrations",
		Log:                 io.Discard,
	})

	err := drv.CreateMigrationsTable(db)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	err = drv.InsertMigration(tx, "test")
	require.NoError(t, err)
	err = tx.Commit()
	require.NoError(t, err)

	tx, err = db.Begin()
	require.NoError(t, err)
	err = drv.DeleteMigration(tx, "test")
	require.NoError(t, err)
	err = tx.Commit()
	require.NoError(t, err)

	migrations, err := drv.SelectMigrations(db, -1)
	require.NoError(t, err)
	require.Equal(t, false, migrations["test"])
}
