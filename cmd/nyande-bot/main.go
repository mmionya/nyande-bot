package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mmionya/nyande-bot/internal/bot"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/resources"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	application, err := bot.New(cfg)
	if err != nil {
		return fmt.Errorf(resources.Get("main.error.initialize_telegram_bot"), err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf(resources.Get("main.error.close_bot"), "telegram", err))
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = application.Run(ctx)
	if ctx.Err() == nil {
		if err == nil || errors.Is(err, context.Canceled) {
			return fmt.Errorf(resources.Get("main.error.bot_stopped_unexpectedly"), "telegram")
		}
		return fmt.Errorf(resources.Get("main.error.run_bot"), "telegram", err)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf(resources.Get("main.error.run_bot"), "telegram", err)
	}
	return nil
}
