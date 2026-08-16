package main

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Donk3ys/kit/db"
)

// Storage failures the service classifies. These are plain sentinels, not
// apperr values, on purpose: a repository reports what happened, and the layer
// above decides what it means to a client. That is the same reason kit does
// not map pgx.ErrNoRows to a 404 for you — only this layer knows whether a
// missing row is an expected outcome or a bug.
var (
	errWidgetNotFound = errors.New("widget not found")
	errNameTaken      = errors.New("widget name taken")
)

// widgetStore is the seam that lets the service be exercised end to end
// without a database — see main_test.go.
type widgetStore interface {
	ByID(ctx context.Context, id uuid.UUID) (Widget, error)
	// Create inserts w, reporting errNameTaken if the name is in use. The
	// check and the insert must be atomic.
	Create(ctx context.Context, w Widget) error
	Delete(ctx context.Context, id uuid.UUID) error
}

// pgStore is the production implementation.
type pgStore struct{ pool *pgxpool.Pool }

func (s *pgStore) ByID(ctx context.Context, id uuid.UUID) (Widget, error) {
	var w Widget
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, quantity FROM widgets WHERE id = $1`, id,
	).Scan(&w.ID, &w.Name, &w.Quantity)

	if errors.Is(err, pgx.ErrNoRows) {
		return Widget{}, errWidgetNotFound
	}
	return w, err
}

// Create shows db.InTx: the uniqueness check and the insert have to see the
// same snapshot, so they belong in one transaction. Returning a non-nil error
// from the function rolls it back.
func (s *pgStore) Create(ctx context.Context, w Widget) error {
	return db.InTx(ctx, s.pool, "create_widget", func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM widgets WHERE name = $1)`, w.Name,
		).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errNameTaken
		}

		_, err := tx.Exec(ctx,
			`INSERT INTO widgets (id, name, quantity) VALUES ($1, $2, $3)`,
			w.ID, w.Name, w.Quantity)
		return err
	})
}

func (s *pgStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM widgets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errWidgetNotFound
	}
	return nil
}
