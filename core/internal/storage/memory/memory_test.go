package memory_test

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store {
		return memory.New()
	})
}
