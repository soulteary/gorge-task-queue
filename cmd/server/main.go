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

	store, err := queue.NewStore(cfg.DSN(), cfg.LeaseDuration, cfg.RetryWait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	httpapi.RegisterRoutes(e, &httpapi.Deps{
		Store: store,
		Token: cfg.ServiceToken,
	})

	e.Logger.Fatal(e.Start(cfg.ListenAddr))
}
