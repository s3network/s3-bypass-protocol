package platform_test

import (
	"errors"
	"testing"

	. "github.com/xtls/xray-core/common/platform"
)

func TestRegisterEnvReloadDoesNotRunHandler(t *testing.T) {
	calls := 0
	RegisterEnvReload(func() error {
		calls++
		return errors.New("must not run at registration time")
	})

	if calls != 0 {
		t.Fatalf("handler ran %d time(s) during registration, want 0", calls)
	}
}

// ReloadEnvSettings is the only place where environment-backed defaults are
// (re)applied; a handler failure has to surface as an error there instead of
// killing the process from a package init.
func TestReloadEnvSettingsReportsError(t *testing.T) {
	RegisterEnvReload(func() error {
		return errors.New("bad environment value")
	})

	if err := ReloadEnvSettings(); err == nil {
		t.Fatal("expected ReloadEnvSettings to report the handler error")
	}
}
