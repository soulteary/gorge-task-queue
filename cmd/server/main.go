package main

import (
	"fmt"
	"os"

	"github.com/soulteary/gorge-task-queue/internal/config"
	"github.com/soulteary/gorge-task-queue/internal/httpapi"
	"github.com/soulteary/gorge-task-queue/internal/queue"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	cfg := config.LoadFromEnv()

	var store queue.Store
	var err error

	switch cfg.QueueBackend {
	case "redis":
		store, err = queue.NewRedisStore(
			cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.RedisKeyPrefix,
			cfg.LeaseDuration, cfg.RetryWait,
		)
	default:
		store, err = queue.NewMySQLStore(cfg.DSN(), cfg.LeaseDuration, cfg.RetryWait)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to %s backend: %v\n", cfg.QueueBackend, err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	e := echo.New()
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogURI: true, LogMethod: true, LogLatency: true, LogError: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			if v.Error != nil {
				e.Logger.Errorf("%s %s %d %v err=%v", v.Method, v.URI, v.Status, v.Latency, v.Error)
			} else {
				e.Logger.Infof("%s %s %d %v", v.Method, v.URI, v.Status, v.Latency)
			}
			return nil
		},
	}))
	e.Use(middleware.Recover())

	httpapi.RegisterRoutes(e, &httpapi.Deps{
		Store: store,
		Token: cfg.ServiceToken,
	})

	e.Logger.Fatal(e.Start(cfg.ListenAddr))
}
