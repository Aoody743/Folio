package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func recoverAccess(args []string) error {
	flags := flag.NewFlagSet("recover-access", flag.ContinueOnError)
	dir := flags.String("data-dir", dataDefault(), "private data directory; stop the API and run as its service user")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected access recovery arguments")
	}
	lock, err := acquireLock(*dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	service, err := core.New(*dir)
	if err != nil {
		return err
	}
	defer service.Close()
	if !service.Initialized(context.Background()) {
		return errors.New("administrator is not initialized")
	}
	if err = service.RecoverPanelAccess(context.Background()); err != nil {
		return err
	}
	fmt.Println("Panel entry restrictions recovered; administrator credentials and MFA retained.")
	return nil
}
