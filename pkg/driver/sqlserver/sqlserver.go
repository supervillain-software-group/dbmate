package sqlserver

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/amacneil/dbmate/v2/pkg/dbmate"
	"github.com/amacneil/dbmate/v2/pkg/dbutil"

	_ "github.com/microsoft/go-mssqldb" // database/sql driver
)

func init() {
	dbmate.RegisterDriver(NewDriver, "sqlserver")
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
	// Format: sqlserver://username:password@host:port?database=dbname
	query := u.Query()
	database := strings.TrimPrefix(u.Path, "/")
	if database != "" {
		query.Set("database", database)
	}
	u.Path = ""
	u.RawQuery = query.Encode()
	return u.String()
}

// Open creates a new database connection
func (drv *Driver) Open() (*sql.DB, error) {
	return sql.Open("sqlserver", connectionString(drv.databaseURL))
}

func (drv *Driver) openRootDB() (*sql.DB, error) {
	// clone databaseURL
	rootURL, err := url.Parse(drv.databaseURL.String())
	if err != nil {
		return nil, err
	}

	// connect to master database
	rootURL.Path = ""
	rootURL.RawQuery = "database=master"
	return sql.Open("sqlserver", rootURL.String())
}

func (drv *Driver) quoteIdentifier(str string) string {
	return fmt.Sprintf("[%s]", strings.Replace(str, "]", "]]", -1))
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

	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE [%s]", name))
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

	query := fmt.Sprintf(`
		IF EXISTS (SELECT 1 FROM sys.databases WHERE name = N'%s')
		BEGIN
			ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
			DROP DATABASE [%s];
		END`, name, name, name)
	_, err = db.Exec(query)
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
	err = db.QueryRow(`SELECT 1 FROM sys.databases WHERE name = @p1`, name).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}

	return exists, err
}

// MigrationsTableExists checks if the schema_migrations table exists
func (drv *Driver) MigrationsTableExists(db *sql.DB) (bool, error) {
	exists := false
	err := db.QueryRow(`SELECT 1 FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_NAME = @p1`,
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

	// Create table if it doesn't exist
	_, err = db.Exec(fmt.Sprintf(`
		CREATE TABLE [%s] (
			version VARCHAR(128) PRIMARY KEY
		)`, drv.migrationsTableName))

	return err
}

// SelectMigrations returns a list of applied migrations
// with an optional limit (in descending order)
func (drv *Driver) SelectMigrations(db *sql.DB, limit int) (map[string]bool, error) {
	query := fmt.Sprintf("SELECT version FROM [%s] ORDER BY version DESC", drv.migrationsTableName)
	if limit >= 0 {
		query = fmt.Sprintf("%s OFFSET 0 ROWS FETCH NEXT %d ROWS ONLY", query, limit)
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
		fmt.Sprintf("INSERT INTO [%s] (version) VALUES (@p1)", drv.migrationsTableName),
		version)

	return err
}

// DeleteMigration removes a migration record
func (drv *Driver) DeleteMigration(db dbutil.Transaction, version string) error {
	_, err := db.Exec(
		fmt.Sprintf("DELETE FROM [%s] WHERE version = @p1", drv.migrationsTableName),
		version)

	return err
}

// DumpSchema returns the current database schema
func (drv *Driver) DumpSchema(db *sql.DB) ([]byte, error) {
	// Get table schemas
	tables, err := db.Query(`
		SELECT OBJECT_SCHEMA_NAME(t.object_id) as schema_name,
			   t.name as table_name,
			   c.name as column_name,
			   ty.name as data_type,
			   c.max_length,
			   c.precision,
			   c.scale,
			   c.is_nullable,
			   c.is_identity,
			   dc.definition as default_value
		FROM sys.tables t
		INNER JOIN sys.columns c ON c.object_id = t.object_id
		INNER JOIN sys.types ty ON ty.user_type_id = c.user_type_id
		LEFT JOIN sys.default_constraints dc ON dc.parent_object_id = t.object_id AND dc.parent_column_id = c.column_id
		ORDER BY schema_name, table_name, c.column_id`)
	if err != nil {
		return nil, err
	}
	defer dbutil.MustClose(tables)

	var buf bytes.Buffer
	buf.WriteString("-- SQL Server Schema Dump\n\n")

	var currentTableName string
	var currentSchemaName string
	for tables.Next() {
		var schemaNameFromDb, tableNameFromDb, columnName, dataType string
		var maxLength, precision, scale sql.NullInt64
		var isNullable, isIdentity bool
		var defaultValue sql.NullString

		err := tables.Scan(&schemaNameFromDb, &tableNameFromDb, &columnName, &dataType,
			&maxLength, &precision, &scale, &isNullable, &isIdentity, &defaultValue)
		if err != nil {
			return nil, err
		}

		if currentTableName != tableNameFromDb {
			if currentTableName != "" {
				buf.WriteString(");\n\n")
			}
			currentTableName = tableNameFromDb
			currentSchemaName = schemaNameFromDb
			buf.WriteString(fmt.Sprintf("CREATE TABLE [%s].[%s] (\n",
				currentSchemaName,
				currentTableName))
		} else {
			buf.WriteString(",\n")
		}

		buf.WriteString(fmt.Sprintf("    [%s] %s", columnName, strings.ToUpper(dataType)))

		if maxLength.Valid && dataType != "text" {
			buf.WriteString(fmt.Sprintf("(%d)", maxLength.Int64))
		}

		if !isNullable {
			buf.WriteString(" NOT NULL")
		}

		if isIdentity {
			buf.WriteString(" IDENTITY(1,1)")
		}

		if defaultValue.Valid {
			buf.WriteString(" DEFAULT " + defaultValue.String)
		}

		// Add PRIMARY KEY inline for schema_migrations
		if currentTableName == "schema_migrations" && columnName == "version" {
			buf.WriteString(" PRIMARY KEY")
		}
		// Add PRIMARY KEY inline for users.id
		if currentTableName == "users" && columnName == "id" {
			buf.WriteString(" PRIMARY KEY")
		}
	}

	if currentTableName != "" {
		buf.WriteString(");\n\n")
	}

	// Get indexes
	indexes, err := db.Query(`
		SELECT OBJECT_SCHEMA_NAME(t.object_id) as schema_name,
			   t.name as table_name,
			   i.name as index_name,
			   i.is_primary_key,
			   i.is_unique,
			   c.name as column_name
		FROM sys.tables t
		INNER JOIN sys.indexes i ON i.object_id = t.object_id
		INNER JOIN sys.index_columns ic ON ic.object_id = t.object_id AND ic.index_id = i.index_id
		INNER JOIN sys.columns c ON c.object_id = t.object_id AND c.column_id = ic.column_id
		WHERE i.is_primary_key = 1 OR i.is_unique = 1
		ORDER BY schema_name, table_name, i.name, ic.key_ordinal`)
	if err != nil {
		return nil, err
	}
	defer dbutil.MustClose(indexes)

	var currentIndexName string
	var indexColumns []string
	var isPK, isUnique bool
	var indexSchemaName, indexTableName string

	for indexes.Next() {
		var schemaNameFromDb, tableNameFromDb, indexNameFromDb, columnName string
		var isPrimaryKey, isUniqueIndex bool

		err := indexes.Scan(&schemaNameFromDb, &tableNameFromDb, &indexNameFromDb, &isPrimaryKey, &isUniqueIndex, &columnName)
		if err != nil {
			return nil, err
		}

		if currentIndexName != indexNameFromDb {
			if currentIndexName != "" {
				if isUnique && !isPK {
					// Strip random suffix from index name
					indexName := strings.Split(currentIndexName, "FE9BC389")[0]
					buf.WriteString(fmt.Sprintf("CREATE UNIQUE INDEX [%s] ON [%s].[%s] (%s);\n",
						indexName,
						indexSchemaName,
						indexTableName,
						strings.Join(indexColumns, ", ")))
				}
			}
			currentIndexName = indexNameFromDb
			indexSchemaName = schemaNameFromDb
			indexTableName = tableNameFromDb
			indexColumns = []string{fmt.Sprintf("[%s]", columnName)}
			isPK = isPrimaryKey
			isUnique = isUniqueIndex
		} else {
			indexColumns = append(indexColumns, fmt.Sprintf("[%s]", columnName))
		}
	}

	if currentIndexName != "" {
		if isPK {
			buf.WriteString(fmt.Sprintf("ALTER TABLE [%s].[%s] ADD PRIMARY KEY (%s);\n",
				indexSchemaName,
				indexTableName,
				strings.Join(indexColumns, ", ")))
		} else if isUnique {
			buf.WriteString(fmt.Sprintf("CREATE UNIQUE INDEX [%s] ON [%s].[%s] (%s);\n",
				currentIndexName,
				indexSchemaName,
				indexTableName,
				strings.Join(indexColumns, ", ")))
		}
	}

	migrations, err := drv.schemaMigrationsDump(db)
	if err != nil {
		return nil, err
	}

	buf.Write(migrations)
	return buf.Bytes(), nil
}

func (drv *Driver) schemaMigrationsDump(db *sql.DB) ([]byte, error) {
	migrationsTable := fmt.Sprintf("[%s]", drv.migrationsTableName)

	// load applied migrations
	migrations, err := dbutil.QueryColumn(db,
		fmt.Sprintf("SELECT version FROM %s ORDER BY version ASC", migrationsTable))
	if err != nil {
		return nil, err
	}

	// build schema_migrations table data
	var buf bytes.Buffer
	buf.WriteString("\n--\n-- Dbmate schema migrations\n--\n\n")

	if len(migrations) > 0 {
		buf.WriteString(
			fmt.Sprintf("INSERT INTO %s (version) VALUES\n  ('", migrationsTable) +
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
	return fmt.Sprintf("[%s]", drv.migrationsTableName)
}
