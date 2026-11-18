// Command muster-plugin-template is a minimal muster plugin: an "echo"
// round-trip. A fake upstream produces a message every ECHO_INTERVAL and posts
// it to muster; the reply drafted for it (by your chief, or by a session the
// chief routes it to) and approved by you is claimed, "sent" (written to
// stdout) and its result reported.
//
// Configuration is read from the environment only (see .env.example):
//
//	MUSTER_URL           muster's address (https, or http to localhost)
//	MUSTER_PLUGIN_TOKEN  the mpl_… token minted on muster's /plugins page
//	ECHO_INTERVAL        how often the fake upstream produces a message (default 30s)
//	STATE_DIR            where the sent-drafts journal and the upstream cursor live (default ./state)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ZacxDev/muster-plugin-template/internal/echo"
	"github.com/ZacxDev/muster-plugin-template/internal/journal"
	"github.com/ZacxDev/muster-plugin-template/internal/muster"
	"github.com/ZacxDev/muster-plugin-template/internal/plugin"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if err := run(logger); err != nil {
		logger.Printf("muster-plugin-template: %v", err)
		os.Exit(1)
	}
}

// Config is everything the plugin reads from its environment.
type Config struct {
	URL      string
	Token    string
	Interval time.Duration
	StateDir string
}

// LoadConfig reads and checks the environment through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	base, err := muster.CheckBaseURL(getenv("MUSTER_URL"))
	if err != nil {
		return Config{}, err
	}
	c := Config{URL: base, Token: getenv("MUSTER_PLUGIN_TOKEN"), Interval: 30 * time.Second, StateDir: "./state"}
	if c.Token == "" {
		return Config{}, errors.New("MUSTER_PLUGIN_TOKEN is not set (mint one on muster's /plugins page)")
	}
	if v := getenv("ECHO_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return Config{}, fmt.Errorf("ECHO_INTERVAL %q is not a duration of at least 1s (e.g. 30s)", v)
		}
		c.Interval = d
	}
	if v := getenv("STATE_DIR"); v != "" {
		c.StateDir = v
	}
	return c, nil
}

func run(logger *log.Logger) error {
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	client, err := muster.New(cfg.URL, cfg.Token)
	if err != nil {
		return err
	}
	j, err := journal.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	defer j.Close()
	up, err := echo.OpenUpstream(cfg.StateDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &plugin.Runner{
		Client:   client,
		Upstream: up,
		Sender:   &echo.Sender{Out: os.Stdout},
		Journal:  j,
		Interval: cfg.Interval,
		Log:      logger,
	}
	// Log the scheme and host only: a mistaken path could hold the token.
	shown := cfg.URL
	if u, err := url.Parse(cfg.URL); err == nil {
		shown = u.Scheme + "://" + u.Host
	}
	logger.Printf("muster-plugin-template: talking to %s, a message every %s", shown, cfg.Interval)
	return r.Run(ctx)
}
