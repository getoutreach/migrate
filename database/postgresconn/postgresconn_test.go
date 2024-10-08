package postgresconn

// error codes https://github.com/lib/pq/blob/master/error.go

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/getoutreach/migrate/v4"

	"github.com/dhui/dktest"

	"github.com/getoutreach/migrate/v4/database"
	dt "github.com/getoutreach/migrate/v4/database/testing"
	"github.com/getoutreach/migrate/v4/dktesting"
	_ "github.com/getoutreach/migrate/v4/source/file"
)

const (
	pgPassword = "postgres"
)

var (
	opts = dktest.Options{
		Env:          map[string]string{"POSTGRES_PASSWORD": pgPassword},
		PortRequired: true, ReadyFunc: isReady}
	// Supported versions: https://www.postgresql.org/support/versioning/
	specs = []dktesting.ContainerSpec{
		{ImageName: "postgres:13", Options: opts},
	}
)

func pgConnectionString(host, port string, options ...string) string {
	options = append(options, "sslmode=disable")
	return fmt.Sprintf("postgres://postgres:%s@%s:%s/postgres?%s", pgPassword, host, port, strings.Join(options, "&"))
}

func isReady(ctx context.Context, c dktest.ContainerInfo) bool {
	ip, port, err := c.FirstPort()
	if err != nil {
		return false
	}

	db, err := sql.Open("postgres", pgConnectionString(ip, port))
	if err != nil {
		return false
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Println("close error:", err)
		}
	}()
	if err = db.PingContext(ctx); err != nil {
		switch err {
		case sqldriver.ErrBadConn, io.EOF:
			return false
		default:
			log.Println(err)
		}
		return false
	}

	return true
}

func mustRun(t *testing.T, d database.Driver, statements []string) {
	for _, statement := range statements {
		if err := d.Run(strings.NewReader(statement)); err != nil {
			t.Fatal(err)
		}
	}
}

func Test(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
		dt.Test(t, d, []byte("SELECT 1"))
	})
}

func TestMigrate(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
		m, err := migrate.NewWithDatabaseInstance("file://./examples/migrations", "postgres", d)
		if err != nil {
			t.Fatal(err)
		}
		dt.TestMigrate(t, m)
	})
}

func TestMultipleStatements(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := d.Run(strings.NewReader("CREATE TABLE foo (foo text); CREATE TABLE bar (bar text);")); err != nil {
			t.Fatalf("expected err to be nil, got %v", err)
		}

		// make sure second table exists
		var exists bool
		if err := d.(*Postgres).conn.QueryRowContext(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'bar' AND table_schema = (SELECT current_schema()))").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("expected table bar to exist")
		}
	})
}

func TestBeginAndRollback(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()

		if err := d.Begin(); err != nil {
			t.Fatal(err)
		}

		// set version to 1, we'll check version record exists during and after tx.
		if err := d.SetVersion(1, false); err != nil {
			t.Fatal(err)
		}

		var exists bool
		if err := d.(*Postgres).conn.QueryRowContext(context.Background(),
			fmt.Sprintf("SELECT 1 FROM %s WHERE version = 1", DefaultMigrationsTable)).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatal("expected to find version 1")
		}

		if err := d.Run(strings.NewReader("CREATE TABLE a (c1 text)")); err != nil {
			t.Fatalf("expected err to be nil, got %v", err)
		}

		if err := d.Rollback(); err != nil {
			t.Fatal(err)
		}

		// make sure table does not exist after rollback
		if err := d.(*Postgres).conn.QueryRowContext(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'a' AND table_schema = (SELECT current_schema()))").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("unexpected, table a should not exist(rollback)")
		}

		if err := d.(*Postgres).conn.QueryRowContext(context.Background(),
			fmt.Sprintf("SELECT 1 FROM %s WHERE version = 1", DefaultMigrationsTable)).Scan(&exists); err != nil {
			// ErrNoRows is good, we expect not to find version one since it should have
			// rolled back.
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
		}
		if exists {
			t.Fatal("expected no version 1 record after rollback")
		}
	})
}

func TestMultipleStatementsInMultiStatementMode(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port, "x-multi-statement=true")
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := d.Run(strings.NewReader("CREATE TABLE foo (foo text); CREATE INDEX idx_foo ON foo (foo);")); err != nil {
			t.Fatalf("expected err to be nil, got %v", err)
		}

		// make sure created index exists
		var exists bool
		if err := d.(*Postgres).conn.QueryRowContext(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = (SELECT current_schema()) AND indexname = 'idx_foo')").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("expected table bar to exist")
		}
	})
}

func TestErrorParsing(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()

		// Parser no longer does line or statement parsing.
		//
		wantErr := `migration failed: syntax error at or near "TABLEE" (column 37) in line 1: CREATE TABLE foo (foo text); CREATE TABLEE bar (bar text); (details: pq: syntax error at or near "TABLEE")`
		if err := d.Run(strings.NewReader(`CREATE TABLE foo (foo text); CREATE TABLEE bar (bar text);`)); err == nil {
			t.Fatal("expected err but got nil")
		} else if err.Error() != wantErr {
			t.Fatalf("expected '%s' but got '%s'", wantErr, err.Error())
		}
	})
}

func TestEmbeddedComment(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()

		if err := d.Run(strings.NewReader(`create table consumers(skip boolean, skip_reason text, name text); UPDATE consumers SET skip = true, skip_reason = 'https://outreach-hq.slack.com/archives/C03TX2QNHTQ/p1686116838617179 -- settings org life events are unneeded' WHERE name = 'settings'`)); err != nil {
			t.Fatalf("expected no error but got %v", err)
		}
	})
}

func TestFilterCustomQuery(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := fmt.Sprintf("postgres://postgres:%s@%v:%v/postgres?sslmode=disable&x-custom=foobar",
			pgPassword, ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
	})
}

func TestWithSchema(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
		}()

		// create foobar schema
		if err := d.Run(strings.NewReader(
			"CREATE SCHEMA foobar AUTHORIZATION postgres;")); err != nil {
			t.Fatal(err)
		}
		if err := d.SetVersion(1, false); err != nil {
			t.Fatal(err)
		}

		// re-connect using that schema
		d2, err := p.Open(fmt.Sprintf("postgres://postgres:%s@%v:%v/postgres?sslmode=disable&search_path=foobar",
			pgPassword, ip, port))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d2.Close(); err != nil {
				t.Fatal(err)
			}
		}()

		version, err := d2.Version()
		if err != nil {
			t.Fatal(err)
		}
		if version.Version != database.NilVersion {
			t.Fatal("expected NilVersion")
		}

		// now update version and compare
		if err := d2.SetVersion(2, false); err != nil {
			t.Fatal(err)
		}
		version, err = d2.Version()
		if err != nil {
			t.Fatal(err)
		}
		if version.Version != 2 {
			t.Fatal("expected version 2")
		}

		// meanwhile, the public schema still has the other version
		version, err = d.Version()
		if err != nil {
			t.Fatal(err)
		}
		if version.Version != 1 {
			t.Fatal("expected version 2")
		}
	})
}

func TestFailToCreateTableWithoutPermissions(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)

		// Check that opening the postgres connection returns NilVersion
		p := &Postgres{}

		d, err := p.Open(addr)

		if err != nil {
			t.Fatal(err)
		}

		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()

		// create user who is not the owner.
		//Although we're concatenating strings in an sql statement it should be fine
		// since this is a test environment and we're not expecting the pgPassword to
		//be malicious
		mustRun(t, d, []string{
			"CREATE USER not_owner WITH ENCRYPTED PASSWORD '" + pgPassword + "';",
			"CREATE SCHEMA barfoo AUTHORIZATION postgres;",
			"GRANT USAGE ON SCHEMA barfoo TO not_owner;",
			"REVOKE CREATE ON SCHEMA barfoo FROM PUBLIC;",
			"REVOKE CREATE ON SCHEMA barfoo FROM not_owner;",
		})

		// re-connect using that schema
		d2, err := p.Open(fmt.Sprintf("postgres://not_owner:%s@%v:%v/postgres?sslmode=disable&search_path=barfoo",
			pgPassword, ip, port))

		defer func() {
			if d2 == nil {
				return
			}
			if err := d2.Close(); err != nil {
				t.Fatal(err)
			}
		}()

		var e *database.Error
		if !errors.As(err, &e) || err == nil {
			t.Fatal("Unexpected error, want permission denied error. Got: ", err)
		}

		if !strings.Contains(e.OrigErr.Error(), "permission denied for schema barfoo") {
			t.Fatal(e)
		}
	})
}

func TestParallelSchema(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()

		// create foo and bar schemas
		if err := d.Run(strings.NewReader(
			"CREATE SCHEMA foo AUTHORIZATION postgres;")); err != nil {
			t.Fatal(err)
		}
		if err := d.Run(strings.NewReader(
			"CREATE SCHEMA bar AUTHORIZATION postgres;")); err != nil {
			t.Fatal(err)
		}

		// re-connect using that schemas
		dfoo, err := p.Open(fmt.Sprintf("postgres://postgres:%s@%v:%v/postgres?sslmode=disable&search_path=foo",
			pgPassword, ip, port))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := dfoo.Close(); err != nil {
				t.Error(err)
			}
		}()

		dbar, err := p.Open(fmt.Sprintf("postgres://postgres:%s@%v:%v/postgres?sslmode=disable&search_path=bar",
			pgPassword, ip, port))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := dbar.Close(); err != nil {
				t.Error(err)
			}
		}()

		if err := dfoo.Lock(); err != nil {
			t.Fatal(err)
		}

		if err := dbar.Lock(); err != nil {
			t.Fatal(err)
		}

		if err := dbar.Unlock(); err != nil {
			t.Fatal(err)
		}

		if err := dfoo.Unlock(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPostgres_Lock(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		addr := pgConnectionString(ip, port)
		p := &Postgres{}
		d, err := p.Open(addr)
		if err != nil {
			t.Fatal(err)
		}

		dt.Test(t, d, []byte("SELECT 1"))

		ps := d.(*Postgres)

		err = ps.Lock()
		if err != nil {
			t.Fatal(err)
		}

		err = ps.Unlock()
		if err != nil {
			t.Fatal(err)
		}

		err = ps.Lock()
		if err != nil {
			t.Fatal(err)
		}

		err = ps.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestWithInstance_Concurrent(t *testing.T) {
	dktesting.ParallelTest(t, specs, func(t *testing.T, c dktest.ContainerInfo) {
		ip, port, err := c.FirstPort()
		if err != nil {
			t.Fatal(err)
		}

		// The number of concurrent processes running WithInstance
		const concurrency = 30

		// We can instantiate a single database handle because it is
		// actually a connection pool, and so, each of the below go
		// routines will have a high probability of using a separate
		// connection, which is something we want to exercise.
		db, err := sql.Open("postgres", pgConnectionString(ip, port))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}()

		db.SetMaxIdleConns(concurrency)
		db.SetMaxOpenConns(concurrency)

		var wg sync.WaitGroup
		defer wg.Wait()

		wg.Add(concurrency)
		ctx := context.Background()
		for i := 0; i < concurrency; i++ {
			go func(i int) {
				defer wg.Done()
				conn, err := db.Conn(ctx)
				if err != nil {
					t.Errorf("TestWithInstance_Concurrent(conn) %d error: %s", i, err)
				}
				defer conn.Close()
				_, err = WithConn(ctx, conn, &Config{})
				if err != nil {
					t.Errorf("process %d error: %s", i, err)
				}
			}(i)
		}
	})
}
func Test_computeLineFromPos(t *testing.T) {
	testcases := []struct {
		pos      int
		wantLine uint
		wantCol  uint
		input    string
		wantOk   bool
	}{
		{
			15, 2, 6, "SELECT *\nFROM foo", true, // foo table does not exists
		},
		{
			16, 3, 6, "SELECT *\n\nFROM foo", true, // foo table does not exists, empty line
		},
		{
			25, 3, 7, "SELECT *\nFROM foo\nWHERE x", true, // x column error
		},
		{
			27, 5, 7, "SELECT *\n\nFROM foo\n\nWHERE x", true, // x column error, empty lines
		},
		{
			10, 2, 1, "SELECT *\nFROMM foo", true, // FROMM typo
		},
		{
			11, 3, 1, "SELECT *\n\nFROMM foo", true, // FROMM typo, empty line
		},
		{
			17, 2, 8, "SELECT *\nFROM foo", true, // last character
		},
		{
			18, 0, 0, "SELECT *\nFROM foo", false, // invalid position
		},
	}
	for i, tc := range testcases {
		t.Run("tc"+strconv.Itoa(i), func(t *testing.T) {
			run := func(crlf bool, nonASCII bool) {
				var name string
				if crlf {
					name = "crlf"
				} else {
					name = "lf"
				}
				if nonASCII {
					name += "-nonascii"
				} else {
					name += "-ascii"
				}
				t.Run(name, func(t *testing.T) {
					input := tc.input
					if crlf {
						input = strings.Replace(input, "\n", "\r\n", -1)
					}
					if nonASCII {
						input = strings.Replace(input, "FROM", "FRÖM", -1)
					}
					gotLine, gotCol, gotOK := computeLineFromPos(input, tc.pos)

					if tc.wantOk {
						t.Logf("pos %d, want %d:%d, %#v", tc.pos, tc.wantLine, tc.wantCol, input)
					}

					if gotOK != tc.wantOk {
						t.Fatalf("expected ok %v but got %v", tc.wantOk, gotOK)
					}
					if gotLine != tc.wantLine {
						t.Fatalf("expected line %d but got %d", tc.wantLine, gotLine)
					}
					if gotCol != tc.wantCol {
						t.Fatalf("expected col %d but got %d", tc.wantCol, gotCol)
					}
				})
			}
			run(false, false)
			run(true, false)
			run(false, true)
			run(true, true)
		})
	}
}
