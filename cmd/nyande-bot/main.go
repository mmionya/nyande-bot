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
	"github.com/mmionya/nyande-bot/internal/discordbot"
	"github.com/mmionya/nyande-bot/resources"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

type service interface {
	Run(context.Context) error
	Close() error
}

type namedService struct {
	name string
	service
}

func run() (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	services := make([]namedService, 0, 2)
	if cfg.BotToken != "" {
		application, err := bot.New(cfg)
		if err != nil {
			return fmt.Errorf(resources.Get("main.error.initialize_telegram_bot"), err)
		}
		services = append(services, namedService{name: "telegram", service: application})
	}
	defer func() {
		for index := len(services) - 1; index >= 0; index-- {
			if err := services[index].Close(); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf(resources.Get("main.error.close_bot"), services[index].name, err))
			}
		}
	}()
	if cfg.DiscordToken != "" {
		application, err := discordbot.New(cfg)
		if err != nil {
			return fmt.Errorf(resources.Get("main.error.initialize_discord_bot"), err)
		}
		services = append(services, namedService{name: "discord", service: application})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	type serviceResult struct {
		name string
		err  error
	}
	results := make(chan serviceResult, len(services))
	for _, current := range services {
		go func(current namedService) {
			results <- serviceResult{name: current.name, err: current.Run(ctx)}
		}(current)
	}

	for range services {
		result := <-results
		if ctx.Err() == nil {
			if result.err == nil || errors.Is(result.err, context.Canceled) {
				runErr = fmt.Errorf(resources.Get("main.error.bot_stopped_unexpectedly"), result.name)
			} else {
				runErr = fmt.Errorf(resources.Get("main.error.run_bot"), result.name, result.err)
			}
			cancel()
		} else if result.err != nil && !errors.Is(result.err, context.Canceled) && runErr == nil {
			runErr = fmt.Errorf(resources.Get("main.error.run_bot"), result.name, result.err)
		}
	}
	return runErr
}
