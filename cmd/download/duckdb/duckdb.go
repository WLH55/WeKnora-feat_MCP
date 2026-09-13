package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// duckdbExtensions is the list of DuckDB extensions required by WeKnora's
// data analysis tool. `spatial` is used for layer metadata (st_read_meta)
// so we can enumerate sheet names from Excel files, while `excel` provides
// the dedicated read_xlsx reader with proper type inference.
var duckdbExtensions = []string{"spatial", "excel"}

// downloadRetries bounds the attempts per extension file; the extensions CDN
// intermittently answers 502 or drops connections on some networks.
const downloadRetries = 3

// extensionRepoURL returns the HTTPS extension repository base URL for the
// embedded DuckDB engine. DuckDB's own remote installer uses a plain-HTTP
// endpoint that is unreachable on some build networks, and switching it to
// HTTPS dead-ends because HTTPS fetching requires the httpfs extension to be
// installed first. So the files are downloaded with Go's HTTP client and
// installed from a local path instead.
func extensionRepoURL(db *sql.DB) (string, error) {
	var version string
	if err := queryVersion(db, &version); err != nil {
		return "", fmt.Errorf("query duckdb version: %w", err)
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "osx"
	}
	return fmt.Sprintf("https://extensions.duckdb.org/%s/%s_%s", version, osName, runtime.GOARCH), nil
}

// queryVersion reads the engine version from pragma_version(), whose version
// column changed names across DuckDB releases, so it matches by name.
func queryVersion(db *sql.DB, out *string) error {
	rows, err := db.Query("SELECT * FROM pragma_version()")
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	idx := -1
	for i, c := range cols {
		if strings.Contains(c, "version") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no version column in pragma_version() (columns: %v)", cols)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if !rows.Next() {
		return rows.Err()
	}
	if err := rows.Scan(ptrs...); err != nil {
		return err
	}
	v, ok := vals[idx].(string)
	if !ok {
		return fmt.Errorf("pragma_version() column %q is not a string", cols[idx])
	}
	*out = v
	return nil
}

func downloadFile(url string, dest string) error {
	var lastErr error
	for attempt := 0; attempt < downloadRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 3 * time.Second)
		}
		if err := tryDownload(url, dest); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func tryDownload(url string, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Close()
}

func downloadExtensions() {
	ctx := context.Background()

	sqlDB, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		panic(err)
	}
	defer sqlDB.Close()

	repo, err := extensionRepoURL(sqlDB)
	if err != nil {
		panic(err)
	}
	tmpDir, err := os.MkdirTemp("", "duckdb-extensions")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpDir)

	for _, ext := range duckdbExtensions {
		local := filepath.Join(tmpDir, ext+".duckdb_extension.gz")
		if err := downloadFile(repo+"/"+ext+".duckdb_extension.gz", local); err != nil {
			panic(fmt.Errorf("failed to download %s extension: %w", ext, err))
		}
		if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("INSTALL '%s';", local)); err != nil {
			panic(fmt.Errorf("failed to install %s extension: %w", ext, err))
		}
		if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("LOAD %s;", ext)); err != nil {
			panic(fmt.Errorf("failed to load %s extension: %w", ext, err))
		}
	}
}

func main() {
	downloadExtensions()
}
