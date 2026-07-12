package example

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/bartek5186/procyon-core/authz"
	coreplugins "github.com/bartek5186/procyon-core/plugins"
	"github.com/labstack/echo/v4"
)

const Version = "0.1.0"

type Plugin struct{}

func New(context.Context, coreplugins.Dependencies, json.RawMessage) (coreplugins.Plugin, error) {
	return &Plugin{}, nil
}

func (p *Plugin) Name() string                   { return "example" }
func (p *Plugin) Migrate(context.Context) error  { return nil }
func (p *Plugin) Policies() []authz.Policy       { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }
func (p *Plugin) RegisterRoutes(routes coreplugins.Routes) {
	if routes.Public != nil {
		routes.Public.GET("/example", func(ctx echo.Context) error {
			return ctx.JSON(http.StatusOK, map[string]string{"module": "example", "version": Version})
		})
	}
}
