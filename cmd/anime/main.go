package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/anime"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/provider/allanime"
	"tarragon-anime/internal/store"
	"tarragon-anime/internal/tarragon"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "manifest" {
		fmt.Print(tarragon.Manifest)
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [manifest]\n", os.Args[0])
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tarragon-anime: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := log.New(os.Stderr, "tarragon-anime: ", log.LstdFlags|log.Lmsgprefix)
	endpoint := os.Getenv("TARRAGON_PLUGINS_ENDPOINT")
	if endpoint == "" {
		return fmt.Errorf("TARRAGON_PLUGINS_ENDPOINT is not set")
	}
	name := os.Getenv("TARRAGON_PLUGIN_NAME")
	if name == "" {
		name = "anime"
	}
	configPath, err := anime.ConfigPath()
	if err != nil {
		return err
	}
	config, err := anime.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", configPath, err)
	}
	statePath, err := store.StatePath()
	if err != nil {
		return err
	}
	state, err := store.Open(statePath)
	if err != nil {
		return fmt.Errorf("open state %s: %w", statePath, err)
	}
	defer state.Close()

	httpClient := &http.Client{Timeout: 20 * time.Second}
	aniListClient := anilist.NewClient(httpClient)
	provider := allanime.NewClient(httpClient)
	player := mpv.New(config.MPVArgs, logger)
	service := anime.NewService(aniListClient, provider, player, state, config, logger)
	defer service.Close()
	plugin := tarragon.NewPlugin(service, logger)
	daemon := tarragon.NewDaemon(endpoint, name, plugin, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = daemon.Run(ctx)
	logger.Printf("shutdown")
	return err
}
