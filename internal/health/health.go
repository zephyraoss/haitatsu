package health

import (
	"context"
	"fmt"
)

type Dependency interface {
	Health(context.Context) error
}

type MigrationStatus interface {
	MigrationsDone() bool
}

type Checker struct {
	database Dependency
	migrator MigrationStatus
	storage  Dependency
}

func NewChecker(database interface {
	Dependency
	MigrationStatus
}, storage Dependency) *Checker {
	return &Checker{database: database, migrator: database, storage: storage}
}

func (c *Checker) Health(context.Context) error {
	return nil
}

func (c *Checker) Ready(ctx context.Context) error {
	if !c.migrator.MigrationsDone() {
		return fmt.Errorf("migrations not complete")
	}
	if err := c.database.Health(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if err := c.storage.Health(ctx); err != nil {
		return fmt.Errorf("s3: %w", err)
	}
	return nil
}
