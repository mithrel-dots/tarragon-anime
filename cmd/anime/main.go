package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"tarragon-anime/internal/anilist"
	"tarragon-anime/internal/anime"
	"tarragon-anime/internal/aniskip"
	"tarragon-anime/internal/auth"
	"tarragon-anime/internal/mpv"
	"tarragon-anime/internal/preview"
	"tarragon-anime/internal/provider/allanime"
	"tarragon-anime/internal/store"
	"tarragon-anime/internal/tarragon"
)

func main() {
	if len(os.Args) > 1 {
		if err := command(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "tarragon-anime: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tarragon-anime: %v\n", err)
		os.Exit(1)
	}
}

// command handles the non-daemon entry points, including the OAuth callback
// invoked through the registered URI scheme handler.
func command(args []string) error {
	switch args[0] {
	case "manifest":
		fmt.Print(tarragon.Manifest)
		return nil
	case "login":
		config, err := loadConfig()
		if err != nil {
			return err
		}
		store, err := tokenStore()
		if err != nil {
			return err
		}
		url, err := store.AuthorizationURL(config.ClientID)
		if err != nil {
			return err
		}
		fmt.Printf("Opening AniList sign-in:\n%s\n", url)
		return openBrowser{}.Open(url)
	case "auth":
		if len(args) != 2 {
			return fmt.Errorf("usage: tarragon-anime auth <callback-uri>")
		}
		store, err := tokenStore()
		if err != nil {
			return err
		}
		token, err := store.ParseCallback(args[1])
		if err != nil {
			return err
		}
		if err := store.Save(token); err != nil {
			return err
		}
		fmt.Printf("AniList sign-in complete; token stored at %s\n", store.Path())
		return nil
	case "logout":
		store, err := tokenStore()
		if err != nil {
			return err
		}
		if err := store.Delete(); err != nil {
			return err
		}
		fmt.Println("Signed out of AniList")
		return nil
	default:
		return fmt.Errorf("usage: %s [manifest|login|auth <uri>|logout]", os.Args[0])
	}
}

func loadConfig() (anime.Config, error) {
	path, err := anime.ConfigPath()
	if err != nil {
		return anime.Config{}, err
	}
	config, err := anime.LoadConfig(path)
	if err != nil {
		return anime.Config{}, fmt.Errorf("load config %s: %w", path, err)
	}
	return config, nil
}

func tokenStore() (*auth.Store, error) {
	path, err := anime.TokenPath()
	if err != nil {
		return nil, err
	}
	return auth.NewStore(path), nil
}

// openBrowser launches the system browser for the OAuth consent page.
type openBrowser struct{}

func (openBrowser) Open(url string) error {
	cmd := exec.Command("xdg-open", url)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run xdg-open: %w", err)
	}
	go cmd.Wait()
	return nil
}

func run() error {
	endpoint := os.Getenv("TARRAGON_PLUGINS_ENDPOINT")
	if endpoint == "" {
		return fmt.Errorf("TARRAGON_PLUGINS_ENDPOINT is not set")
	}
	name := os.Getenv("TARRAGON_PLUGIN_NAME")
	if name == "" {
		name = "anime"
	}
	prefix := os.Getenv("TARRAGON_PLUGIN_PREFIX")
	prefixSymbol := os.Getenv("TARRAGON_PREFIX_SYMBOL")
	logger := log.New(os.Stderr, "[PLUGIN: "+name+"] INFO ", 0)
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
	previewDir, err := preview.DefaultDir()
	if err != nil {
		return err
	}

	tokens, err := tokenStore()
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 20 * time.Second}
	aniListClient := anilist.NewClient(httpClient)
	provider := allanime.NewClient(httpClient)
	player := mpv.New(config.MPVArgs, logger)
	previews := preview.NewCache(httpClient, previewDir)

	syncClient := anilist.NewAuthenticatedClient(httpClient, tokens.Token)
	service := anime.NewService(aniListClient, provider, player, state, previews,
		syncClient, config, logger).WithAuth(tokens, openBrowser{}).WithSkip(aniskip.NewClient(httpClient))
	if config.Sync.Enabled {
		if service.SignedIn() {
			logger.Printf("AniList sync enabled conflict=%s trigger=%s", config.Sync.Conflict, config.Sync.Trigger)
		} else {
			logger.Printf("AniList sync idle: sign in with the login command")
		}
	}
	defer service.Close()
	plugin := tarragon.NewPlugin(service, name, prefix, prefixSymbol, logger)
	daemon := tarragon.NewDaemon(endpoint, name, plugin, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serviceDone := make(chan struct{})
	go func() {
		defer close(serviceDone)
		service.Run(ctx)
	}()
	err = daemon.Run(ctx)
	service.Close()
	<-serviceDone
	logger.Printf("shutdown")
	return err
}
