// adduser creates a login account (AMS step 1).
//
//	go run ./cmd/adduser -user admin -role admin
//
// Roles: viewer, operator, engineer, supervisor, admin.
// The password is read from the PLC_USER_PASSWORD environment variable, or
// typed on stdin when that is not set (it is never a command-line argument,
// so it does not end up in shell history or the process list).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/config"
	"PLC_Monitoring/internal/store"
)

func main() {
	user := flag.String("user", "", "username (required)")
	name := flag.String("name", "", "display name (optional)")
	role := flag.String("role", "operator", "viewer | operator | engineer | supervisor | admin")
	flag.Parse()
	if *user == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/adduser -user NAME [-name \"Full Name\"] [-role operator]")
		os.Exit(2)
	}

	password := os.Getenv("PLC_USER_PASSWORD")
	if password == "" {
		fmt.Fprintf(os.Stderr, "Password for %s (min %d characters): ", *user, auth.MinPasswordLen)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		password = strings.TrimRight(line, "\r\n")
	}

	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PostgreSQL:", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := auth.NewService(db).CreateUser(ctx, *user, *name, password, *role); err != nil {
		fmt.Fprintln(os.Stderr, "could not create user:", err)
		os.Exit(1)
	}
	fmt.Printf("user %q created with role %q\n", strings.ToLower(strings.TrimSpace(*user)), *role)
}
