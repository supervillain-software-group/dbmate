package informix

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"

	"github.com/amacneil/dbmate/v2/pkg/dbmate"
	"github.com/amacneil/dbmate/v2/pkg/dbutil"

	_ "github.com/ibmdb/go_ibm_db" // database/sql driver
)

func init() {
	dbmate.RegisterDriver(NewDriver, "informix")
}

// Driver provides top level database functions
type Driver struct {
	migrationsTableName string
	databaseURL         *url.URL
	log                 io.Writer
}

// NewDriver initializes the driver
func NewDriver(config dbmate.DriverConfig) dbmate.Driver {
	return &Driver{
		migrationsTableName: config.MigrationsTableName,
		databaseURL:         config.DatabaseURL,
		log:                 config.Log,
	}
}

func connectionString(u *url.URL) string {
	// Format: informix://username:password@host:port/database
	// Convert to: HOSTNAME=host;PORT=port;DATABASE=database;UID=username;PWD=password
	query := u.Query()
	database := strings.TrimPrefix(u.Path, "/")

	params := []string{
		fmt.Sprintf("HOSTNAME=%s", u.Hostname()),
		fmt.Sprintf("PORT=%s", u.Port()),
	}

	if database != "" {
		params = append(params, fmt.Sprintf("DATABASE=%s", database))
	}

	if u.User != nil {
		if username := u.User.Username(); username != "" {
			params = append(params, fmt.Sprintf("UID=%s", username))
		}
		if password, ok := u.User.Password(); ok {
			params = append(params, fmt.Sprintf("PWD=%s", password))
		}
	}

	// Add any additional query parameters
	for key, values := range query {
		if len(values) > 0 {
			params = append(params, fmt.Sprintf("%s=%s", key, values[0]))
		}
	}

	return strings.Join(params, ";")
}

// Open creates a new database connection
func (drv *Driver) Open() (*sql.DB, error) {
	return sql.Open("go_ibm_db", connectionString(drv.databaseURL))
}

func (drv *Driver) openRootDB() (*sql.DB, error) {
	// clone databaseURL
	rootURL, err := url.Parse(drv.databaseURL.String())
	if err != nil {
		return nil, err
	}

	// connect without database name
	rootURL.Path = ""
	return sql.Open("go_ibm_db", connectionString(rootURL))
}

func (drv *Driver) quoteIdentifier(str string) string {
	return fmt.Sprintf("\"%s\"", strings.Replace(str, "\"", "\"\"", -1))
}

// CreateDatabase creates the specified database
func (drv *Driver) CreateDatabase() error {
	name := strings.TrimPrefix(drv.databaseURL.Path, "/")
	if name == "" {
		return fmt.Errorf("database name cannot be empty")
	}
	fmt.Fprintf(drv.log, "Creating: %s\n", name)

	db, err := drv.openRootDB()
	if err != nil {
		return err
	}
	defer dbutil.MustClose(db)

	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE %s", drv.quoteIdentifier(name)))
	return err
}

// DropDatabase drops the specified database (if it exists)
func (drv *Driver) DropDatabase() error {
	name := strings.TrimPrefix(drv.databaseURL.Path, "/")
	if name == "" {
		return fmt.Errorf("database name cannot be empty")
	}
	fmt.Fprintf(drv.log, "Dropping: %s\n", name)

	db, err := drv.openRootDB()
	if err != nil {
		return err
	}
	defer dbutil.MustClose(db)

	_, err = db.Exec(fmt.Sprintf("DROP DATABASE %s IF EXISTS", drv.quoteIdentifier(name)))
	return err
}

// DatabaseExists determines whether the database exists
func (drv *Driver) DatabaseExists() (bool, error) {
	name := strings.TrimPrefix(drv.databaseURL.Path, "/")
	if name == "" {
		return false, fmt.Errorf("database name cannot be empty")
	}

	db, err := drv.openRootDB()
	if err != nil {
		return false, err
	}
	defer dbutil.MustClose(db)

	exists := false
	err = db.QueryRow(
		"SELECT 1 FROM sysdatabases WHERE name = ?", name).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}

	return exists, err
}

// MigrationsTableExists checks if the schema_migrations table exists
func (drv *Driver) MigrationsTableExists(db *sql.DB) (bool, error) {
	exists := false
	err := db.QueryRow(`
		SELECT 1 FROM systables
		WHERE tabname = ? AND owner = USER`,
		drv.migrationsTableName).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}

	return exists, err
}

// CreateMigrationsTable creates the schema_migrations table
func (drv *Driver) CreateMigrationsTable(db *sql.DB) error {
	// First check if table exists
	exists, err := drv.MigrationsTableExists(db)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	_, err = db.Exec(fmt.Sprintf(`
		CREATE TABLE %s (
			version VARCHAR(128) PRIMARY KEY
		)`, drv.quoteIdentifier(drv.migrationsTableName)))

	return err
}

// SelectMigrations returns a list of applied migrations
// with an optional limit (in descending order)
func (drv *Driver) SelectMigrations(db *sql.DB, limit int) (map[string]bool, error) {
	query := fmt.Sprintf("SELECT version FROM %s ORDER BY version DESC",
		drv.quoteIdentifier(drv.migrationsTableName))
	if limit >= 0 {
		query = fmt.Sprintf("%s FIRST %d", query, limit)
	}

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer dbutil.MustClose(rows)

	migrations := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		migrations[version] = true
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return migrations, nil
}

// InsertMigration adds a new migration record
func (drv *Driver) InsertMigration(db dbutil.Transaction, version string) error {
	_, err := db.Exec(
		fmt.Sprintf("INSERT INTO %s (version) VALUES (?)",
			drv.quoteIdentifier(drv.migrationsTableName)),
		version)

	return err
}

// DeleteMigration removes a migration record
func (drv *Driver) DeleteMigration(db dbutil.Transaction, version string) error {
	_, err := db.Exec(
		fmt.Sprintf("DELETE FROM %s WHERE version = ?",
			drv.quoteIdentifier(drv.migrationsTableName)),
		version)

	return err
}

// DumpSchema returns the current database schema using dbschema
func (drv *Driver) DumpSchema(db *sql.DB) ([]byte, error) {
	// Get database name from URL
	dbName := strings.TrimPrefix(drv.databaseURL.Path, "/")
	if dbName == "" {
		return nil, fmt.Errorf("database name cannot be empty")
	}

	// Run dbschema and capture output
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("dbschema", dbName)

	// Set environment variables from connection string
	env := []string{
		fmt.Sprintf("INFORMIXSERVER=%s", drv.databaseURL.Host),
		"INFORMIXDIR=/opt/informix", // Default Informix installation directory
		"DB_LOCALE=en_US.utf8",
		"PATH=/opt/informix/bin:/usr/local/bin:/usr/bin:/bin",
	}

	// Add credentials if provided
	if u := drv.databaseURL.User; u != nil {
		if username := u.Username(); username != "" {
			env = append(env, fmt.Sprintf("INFORMIXUSER=%s", username))
		}
		if password, ok := u.Password(); ok {
			env = append(env, fmt.Sprintf("INFORMIXPASSWORD=%s", password))
		}
	}

	cmd.Env = env

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("dbschema failed: %v: %s", err, stderr.String())
	}

	// Add migrations
	var buf bytes.Buffer
	buf.WriteString("-- Informix Schema Dump (via dbschema)\n\n")
	buf.Write(stdout.Bytes())

	migrations, err := drv.schemaMigrationsDump(db)
	if err != nil {
		return nil, err
	}
	buf.Write(migrations)

	return buf.Bytes(), nil
}

func (drv *Driver) schemaMigrationsDump(db *sql.DB) ([]byte, error) {
	migrations, err := dbutil.QueryColumn(db,
		fmt.Sprintf("SELECT version FROM %s ORDER BY version ASC",
			drv.quoteIdentifier(drv.migrationsTableName)))
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteString("\n--\n-- Dbmate schema migrations\n--\n\n")

	if len(migrations) > 0 {
		buf.WriteString(
			fmt.Sprintf("INSERT INTO %s (version) VALUES\n  ('",
				drv.quoteIdentifier(drv.migrationsTableName)) +
				strings.Join(migrations, "'),\n  ('") +
				"');\n")
	}

	return buf.Bytes(), nil
}

// Ping verifies a connection to the database server. It does not verify whether the
// specified database exists.
func (drv *Driver) Ping() error {
	db, err := drv.openRootDB()
	if err != nil {
		return err
	}
	defer dbutil.MustClose(db)

	return db.Ping()
}

// Return a normalized version of the driver-specific error type.
func (drv *Driver) QueryError(query string, err error) error {
	return &dbmate.QueryError{Err: err, Query: query}
}

func (drv *Driver) quotedMigrationsTableName() string {
	return drv.quoteIdentifier(drv.migrationsTableName)
}
