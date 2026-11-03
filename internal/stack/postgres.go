package stack

import (
	"fmt"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
)

// Credentials of every postgres module instance; the database is throwaway
// (tmpfs) and reachable only inside its stack.
const (
	pgUser     = "eval"
	pgPassword = "eval"
	pgDatabase = "eval"
)

func postgresImage(s profile.Service) string { return "postgres:" + s.Version }

func postgresOutputs(name string) map[string]string {
	return map[string]string{
		"url":  fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", pgUser, pgPassword, name, pgDatabase),
		"host": name,
	}
}

func postgresService(s profile.Service) map[string]any {
	return map[string]any{
		"image": postgresImage(s),
		"environment": map[string]string{
			"POSTGRES_DB":       pgDatabase,
			"POSTGRES_USER":     pgUser,
			"POSTGRES_PASSWORD": pgPassword,
		},
		"tmpfs":       []string{"/var/lib/postgresql/data"},
		"healthcheck": healthcheck(fmt.Sprintf("pg_isready -U %s -d %s", pgUser, pgDatabase), 30),
	}
}
