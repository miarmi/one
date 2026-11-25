package main

import (
	"context"
	"database/sql"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Note is one saved note.
type Note struct {
	ID      int64
	Text    string
	Created time.Time
}

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		text TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

func (s *store) Close() error { return s.db.Close() }

func (s *store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (s *store) Add(ctx context.Context, text string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO notes (text, created_at) VALUES (?, ?)", text, time.Now().Unix())
	return err
}

func (s *store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM notes WHERE id = ?", id)
	return err
}

// List returns the newest notes first.
func (s *store) List(ctx context.Context, limit int) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, text, created_at FROM notes ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		var created int64
		if err := rows.Scan(&n.ID, &n.Text, &created); err != nil {
			return nil, err
		}
		n.Created = time.Unix(created, 0)
		out = append(out, n)
	}
	return out, rows.Err()
}
