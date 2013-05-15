package db

import (
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/bmizerany/pq"
	"goposm/mapping"
	"log"
	"strings"
)

type Config struct {
	Type             string
	ConnectionParams string
	Srid             int
	Schema           string
}

type DB interface {
	Init(*mapping.Mapping) error
	InsertBatch(string, [][]interface{}) error
}

type ColumnSpec struct {
	Name string
	Type ColumnType
}
type TableSpec struct {
	Name         string
	Schema       string
	Columns      []ColumnSpec
	GeometryType string
	Srid         int
}

func (col *ColumnSpec) AsSQL() string {
	return fmt.Sprintf("\"%s\" %s", col.Name, col.Type.Name)
}

func (spec *TableSpec) CreateTableSQL() string {
	cols := []string{
		"id SERIAL PRIMARY KEY",
		// "osm_id BIGINT",
	}
	for _, col := range spec.Columns {
		if col.Type.Name == "GEOMETRY" {
			continue
		}
		cols = append(cols, col.AsSQL())
	}
	columnSQL := strings.Join(cols, ",\n")
	return fmt.Sprintf(`
        CREATE TABLE IF NOT EXISTS "%s"."%s" (
            %s
        );`,
		spec.Schema,
		spec.Name,
		columnSQL,
	)
}

func (spec *TableSpec) InsertSQL() string {
	cols := []string{
	// "osm_id",
	// "geometry",
	}
	vars := []string{
	// "$1",
	// fmt.Sprintf("ST_GeomFromWKB($2, %d)", spec.Srid),
	}
	for _, col := range spec.Columns {
		cols = append(cols, col.Name)
		if col.Type.ValueTemplate != "" {
			vars = append(vars, fmt.Sprintf(
				col.Type.ValueTemplate,
				len(vars)+1))
		} else {
			vars = append(vars, fmt.Sprintf("$%d", len(vars)+1))
		}
	}
	columns := strings.Join(cols, ", ")
	placeholders := strings.Join(vars, ", ")

	return fmt.Sprintf(`INSERT INTO "%s"."%s" (%s) VALUES (%s)`,
		spec.Schema,
		spec.Name,
		columns,
		placeholders,
	)
}

func NewTableSpec(conf *Config, t *mapping.Table) *TableSpec {
	spec := TableSpec{
		Name:         t.Name,
		Schema:       conf.Schema,
		GeometryType: t.Type,
		Srid:         conf.Srid,
	}
	for _, field := range t.Fields {
		col := ColumnSpec{field.Name, pgTypes[field.Type]}
		if col.Type.Name == "" {
			log.Println("unhandled", field)
			col.Type.Name = "VARCHAR"
		}
		spec.Columns = append(spec.Columns, col)
	}
	return &spec
}

type SQLError struct {
	query         string
	originalError error
}

func (e *SQLError) Error() string {
	return fmt.Sprintf("SQL Error: %s in query %s", e.originalError.Error(), e.query)
}

type SQLInsertError struct {
	SQLError
	data interface{}
}

func (e *SQLInsertError) Error() string {
	return fmt.Sprintf("SQL Error: %s in query %s (%+v)", e.originalError.Error(), e.query, e.data)
}

func (pg *PostGIS) createTable(spec TableSpec) error {
	var sql string
	var err error

	sql = fmt.Sprintf(`DROP TABLE IF EXISTS "%s"."%s"`, spec.Schema, spec.Name)
	_, err = pg.Db.Exec(sql)
	if err != nil {
		return &SQLError{sql, err}
	}

	sql = spec.CreateTableSQL()
	_, err = pg.Db.Exec(sql)
	if err != nil {
		return &SQLError{sql, err}
	}
	sql = fmt.Sprintf("SELECT AddGeometryColumn('%s', '%s', 'geometry', %d, '%s', 2);",
		spec.Schema, spec.Name, spec.Srid, strings.ToUpper(spec.GeometryType))
	row := pg.Db.QueryRow(sql)
	var void interface{}
	err = row.Scan(&void)
	if err != nil {
		return &SQLError{sql, err}
	}
	return nil
}

func (pg *PostGIS) createSchema() error {
	var sql string
	var err error

	if pg.Config.Schema == "public" {
		return nil
	}

	sql = fmt.Sprintf("SELECT EXISTS(SELECT schema_name FROM information_schema.schemata WHERE schema_name = '%s');",
		pg.Config.Schema)
	row := pg.Db.QueryRow(sql)
	var exists bool
	err = row.Scan(&exists)
	if err != nil {
		return &SQLError{sql, err}
	}
	if exists {
		return nil
	}

	sql = fmt.Sprintf("CREATE SCHEMA \"%s\"", pg.Config.Schema)
	_, err = pg.Db.Exec(sql)
	if err != nil {
		return &SQLError{sql, err}
	}
	return nil
}

type PostGIS struct {
	Db     *sql.DB
	Config Config
	Tables map[string]*TableSpec
}

func (pg *PostGIS) Open() error {
	var err error
	pg.Db, err = sql.Open("postgres", pg.Config.ConnectionParams)
	if err != nil {
		return err
	}
	// sql.Open is lazy, make a query to check that the
	// connection actually works
	row := pg.Db.QueryRow("SELECT 1;")
	var v string
	err = row.Scan(&v)
	if err != nil {
		return err
	}
	return nil
}

func (pg *PostGIS) InsertBatch(table string, rows [][]interface{}) error {
	spec, ok := pg.Tables[table]
	if !ok {
		return errors.New("unkown table: " + table)
	}

	tx, err := pg.Db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			if err := tx.Rollback(); err != nil {
				log.Println("rollback failed", err)
			}
		}
	}()

	sql := spec.InsertSQL()
	stmt, err := tx.Prepare(sql)
	if err != nil {
		return &SQLError{sql, err}
	}
	defer stmt.Close()

	for _, row := range rows {
		_, err := stmt.Exec(row...)
		if err != nil {
			return &SQLInsertError{SQLError{sql, err}, row}
		}
	}

	err = tx.Commit()
	if err != nil {
		return err
	}
	tx = nil
	return nil

}

func (pg *PostGIS) Init(m *mapping.Mapping) error {
	if err := pg.createSchema(); err != nil {
		return err
	}

	for name, table := range m.Tables {
		pg.Tables[name] = NewTableSpec(&pg.Config, table)
	}
	for _, spec := range pg.Tables {
		if err := pg.createTable(*spec); err != nil {
			return err
		}
	}
	return nil
}

func Open(conf Config) (DB, error) {
	if conf.Type != "postgres" {
		panic("unsupported database type: " + conf.Type)
	}
	db := &PostGIS{}
	db.Tables = make(map[string]*TableSpec)
	db.Config = conf
	err := db.Open()
	if err != nil {
		return nil, err
	}
	return db, nil
}
