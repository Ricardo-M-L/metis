package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestRegisterDoesNotCreateDeprecatedMemoryRepository(t *testing.T) {
	base := t.TempDir()
	sessions := filepath.Join(base, "sessions")
	RegisterWithDirs(
		tools.NewRegistry(),
		&config.Config{},
		permission.New(permission.ModeAsk),
		filepath.Join(base, "skills"),
		sessions,
	)
	if info, err := os.Stat(sessions); err != nil || !info.IsDir() {
		t.Fatalf("private session root was not created: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(base, "memories")); !os.IsNotExist(err) {
		t.Fatalf("deprecated parallel memory root was recreated: %v", err)
	}
}
