package platform

import (
	"errors"
	"sync"
)

var envReloadRegistry = struct {
	sync.RWMutex
	handlers []func() error
}{}

// RegisterEnvReload registers an environment reload handler.
//
// The handler is NOT run here: package init must not depend on the process
// environment (it may still be incomplete at that point, and a bad value must
// not abort the process with a bare panic). State is refreshed by
// ReloadEnvSettings, which core.New calls after the configuration - including
// its root "env" object - has been read, and which reports failures as a
// regular error.
func RegisterEnvReload(handler func() error) {
	if handler == nil {
		return
	}
	envReloadRegistry.Lock()
	defer envReloadRegistry.Unlock()
	envReloadRegistry.handlers = append(envReloadRegistry.handlers, handler)
}

// ReloadEnvSettings refreshes all registered environment-backed package state.
func ReloadEnvSettings() error {
	envReloadRegistry.RLock()
	handlers := append([]func() error{}, envReloadRegistry.handlers...)
	envReloadRegistry.RUnlock()

	var errs []error
	for _, handler := range handlers {
		if err := handler(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
