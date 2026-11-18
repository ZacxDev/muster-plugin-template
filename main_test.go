package main

import (
	"strings"
	"testing"
	"time"
)

// The startup line names muster by scheme and host only. cfg is built here,
// not by LoadConfig (which now refuses a path), so the line's own redaction is
// what is tested.
func TestTheStartupLineNamesTheHostOnly(t *testing.T) {
	got := startupLine(Config{URL: "https://muster.example.com:8443/mpl_s3cretT0ken?t=mpl_s3cretT0ken", Interval: 30 * time.Second})
	want := "muster-plugin-template: talking to https://muster.example.com:8443, a message every 30s"
	if got != want {
		t.Fatalf("startup line = %q, want %q", got, want)
	}
	if strings.Contains(got, "s3cretT0ken") {
		t.Fatalf("startup line names the path: %q", got)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"MUSTER_URL": "http://localhost:8105/", "MUSTER_PLUGIN_TOKEN": "mpl_x"}))
	if err != nil || c.URL != "http://localhost:8105" || c.Token != "mpl_x" || c.Interval != 30*time.Second || c.StateDir != "./state" {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	c, err = LoadConfig(env(map[string]string{"MUSTER_URL": "https://muster.example.com", "MUSTER_PLUGIN_TOKEN": "mpl_x",
		"ECHO_INTERVAL": "5s", "STATE_DIR": "/tmp/s"}))
	if err != nil || c.Interval != 5*time.Second || c.StateDir != "/tmp/s" {
		t.Fatalf("set = %+v, %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"MUSTER_PLUGIN_TOKEN": "mpl_x"},
		{"MUSTER_URL": "http://muster.example.com", "MUSTER_PLUGIN_TOKEN": "mpl_x"},
		{"MUSTER_URL": "https://muster.example.com/mpl_x", "MUSTER_PLUGIN_TOKEN": "mpl_x"},
		{"MUSTER_URL": "https://muster.example.com"},
		{"MUSTER_URL": "https://muster.example.com", "MUSTER_PLUGIN_TOKEN": "mpl_x", "ECHO_INTERVAL": "soon"},
		{"MUSTER_URL": "https://muster.example.com", "MUSTER_PLUGIN_TOKEN": "mpl_x", "ECHO_INTERVAL": "10ms"},
	} {
		if _, err := LoadConfig(env(bad)); err == nil {
			t.Errorf("LoadConfig(%v) accepted it", bad)
		}
	}
}
