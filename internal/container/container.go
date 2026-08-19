// Package container provides a dependency injection container for the application.
package container

import (
	"any-load/internal/app"
	"any-load/internal/channel"
	"any-load/internal/config"
	"any-load/internal/db"
	"any-load/internal/encryption"
	"any-load/internal/handler"
	"any-load/internal/httpclient"
	"any-load/internal/keypool"
	"any-load/internal/proxy"
	"any-load/internal/router"
	"any-load/internal/services"
	"any-load/internal/store"
	"any-load/internal/types"

	"go.uber.org/dig"
)

// BuildContainer creates a new dependency injection container and provides all the application's services.
func BuildContainer() (*dig.Container, error) {
	container := dig.New()

	// Infrastructure Services
	if err := container.Provide(config.NewManager); err != nil {
		return nil, err
	}
	if err := container.Provide(func(configManager types.ConfigManager) (encryption.Service, error) {
		return encryption.NewService(configManager.GetEncryptionKey())
	}); err != nil {
		return nil, err
	}
	if err := container.Provide(db.NewDB); err != nil {
		return nil, err
	}
	if err := container.Provide(config.NewSystemSettingsManager); err != nil {
		return nil, err
	}
	if err := container.Provide(store.NewStore); err != nil {
		return nil, err
	}
	if err := container.Provide(httpclient.NewHTTPClientManager); err != nil {
		return nil, err
	}
	if err := container.Provide(channel.NewFactory); err != nil {
		return nil, err
	}

	// Business Services
	if err := container.Provide(services.NewTaskService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewKeyManualValidationService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewKeyService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewKeyImportService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewKeyDeleteService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewLogService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewLogCleanupService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewRequestLogService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewSubGroupManager); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewGroupManager); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewGroupService); err != nil {
		return nil, err
	}
	if err := container.Provide(services.NewAggregateGroupService); err != nil {
		return nil, err
	}
	if err := container.Provide(keypool.NewProvider); err != nil {
		return nil, err
	}
	if err := container.Provide(keypool.NewKeyValidator); err != nil {
		return nil, err
	}
	if err := container.Provide(keypool.NewCronChecker); err != nil {
		return nil, err
	}

	// Handlers
	if err := container.Provide(handler.NewServer); err != nil {
		return nil, err
	}
	if err := container.Provide(handler.NewCommonHandler); err != nil {
		return nil, err
	}

	// Proxy & Router
	if err := container.Provide(proxy.NewProxyServer); err != nil {
		return nil, err
	}
	if err := container.Provide(router.NewRouter); err != nil {
		return nil, err
	}

	// Application Layer
	if err := container.Provide(app.NewApp); err != nil {
		return nil, err
	}

	return container, nil
}
