package bale_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/strio-dev/bale"
)

func TestNewClientDoesNotReadDockerConfiguration(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", directory)
	if _, err := bale.NewClient(bale.ClientOptions{}); err != nil {
		t.Fatalf("anonymous SDK construction read Docker configuration: %v", err)
	}
}

func TestRecipeErrorsAreTyped(t *testing.T) {
	for _, parse := range []func() error{
		func() error { _, err := bale.ParseRecipe([]byte("include: []\n")); return err },
		func() error { _, err := bale.NormalizeRecipe(bale.Recipe{}); return err },
	} {
		err := parse()
		var detail *bale.Error
		if !errors.Is(err, bale.ErrInvalid) || !errors.As(err, &detail) || errors.Unwrap(err) == nil {
			t.Fatalf("expected typed invalid recipe error, got %v", err)
		}
	}
}
