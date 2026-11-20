package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// Schema changes live in migrations/ as numbered SQL files and are compiled
// into the binary. Opening the store applies any that have not run yet, so
// installing a new Superdeck and starting it is the whole upgrade.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is Superdeck's own data: accounts and the instances to bring back.
// Claude's data stays in each account's config directory.
type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	_, statErr := os.Stat(path)
	existed := statErr == nil
	dsn := (&url.URL{Scheme: "file", Path: path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; SQLite is plenty fast for this
	if err := migrate(db, path, existed); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func migrate(db *sql.DB, path string, existed bool) error {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	var latest int64
	for _, s := range p.ListSources() {
		if s.Version > latest {
			latest = s.Version
		}
	}
	if current > latest {
		return fmt.Errorf("%s was written by a newer Superdeck (schema %d); this one understands up to %d. Install the newer version", path, current, latest)
	}
	if current == latest {
		return nil
	}
	// Keep a copy of the data as it was before changing its shape.
	if existed && current > 0 {
		backup := fmt.Sprintf("%s.before-schema-%d", path, latest)
		os.Remove(backup)
		if _, err := db.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			return fmt.Errorf("could not back up the data before upgrading it: %w", err)
		}
		log.Printf("backed up data to %s before upgrading", backup)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("upgrading the data failed; nothing was started: %w", err)
	}
	for _, r := range results {
		log.Printf("applied migration %s", r.Source.Path)
	}
	return nil
}

func (s *Store) Accounts() ([]account, error) {
	rows, err := s.db.Query(`SELECT id, name FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []account{}
	for rows.Next() {
		var a account
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddAccount(id, name string) error {
	_, err := s.db.Exec(`INSERT INTO accounts (id, name) VALUES (?, ?)`, id, name)
	return nameError(err)
}

func (s *Store) RenameAccount(id, name string) error {
	res, err := s.db.Exec(`UPDATE accounts SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return nameError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("there is no account with that name")
	}
	return nil
}

func nameError(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("an account already has that name")
	}
	return err
}

// Wanted lists the instances to bring back when Superdeck starts.
func (s *Store) Wanted() ([]wanted, error) {
	rows, err := s.db.Query(`SELECT account_id, folder, spawn, capacity FROM instances ORDER BY folder, account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wanted
	for rows.Next() {
		var w wanted
		if err := rows.Scan(&w.Account, &w.Project, &w.Spawn, &w.Capacity); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWanted adds or removes one instance from that list.
func (s *Store) SetWanted(w wanted, keep bool) error {
	if !keep {
		_, err := s.db.Exec(`DELETE FROM instances WHERE account_id = ? AND folder = ?`, w.Account, w.Project)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO instances (account_id, folder, spawn, capacity) VALUES (?, ?, ?, ?)
		ON CONFLICT (account_id, folder) DO UPDATE SET spawn = excluded.spawn, capacity = excluded.capacity`,
		w.Account, w.Project, w.Spawn, w.Capacity)
	return err
}
