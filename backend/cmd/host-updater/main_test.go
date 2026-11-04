package main

import (
	"testing"

	"yingce/backend/internal/hostupdate"
)

func TestUpdaterRepositoryEnvOverride(t *testing.T) {
	t.Setenv("CANVAS_UPDATER_REPOSITORY", "")
	if got := env("CANVAS_UPDATER_REPOSITORY", hostupdate.DefaultRepository); got != "antcloudsman/open-ai-canvas" {
		t.Fatalf("unset repository = %q", got)
	}
	t.Setenv("CANVAS_UPDATER_REPOSITORY", "  example/override  ")
	if got := env("CANVAS_UPDATER_REPOSITORY", hostupdate.DefaultRepository); got != "example/override" {
		t.Fatalf("override repository = %q", got)
	}
}
