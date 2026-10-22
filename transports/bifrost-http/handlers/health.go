package handlers

import (
	"sync"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// HealthHandler manages HTTP requests for health checks.
type HealthHandler struct {
	config *lib.Config
}

// NewHealthHandler creates a new health handler instance.
func NewHealthHandler(config *lib.Config) *HealthHandler {
	return &HealthHandler{
		config: config,
	}
}

// RegisterRoutes registers the health-related routes.
func (h *HealthHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/health", lib.ChainMiddlewares(h.getHealth, middlewares...))
	r.GET("/api/internal/health/model-catalog", lib.ChainMiddlewares(h.getModelCatalogHealth, middlewares...))
}

// getHealth handles GET /api/health - Get the health status of the server.
func (h *HealthHandler) getHealth(ctx *fasthttp.RequestCtx) {
	// If DB pings are disabled, just return OK
	if h.config.ClientConfig.DisableDBPingsInHealth {
		SendJSON(ctx, map[string]any{"status": "ok", "components": map[string]any{"db_pings": "disabled"}})
		return
	}
	// Pinging config store
	reqCtx, cancel := RequestWorkContext(ctx, 10*time.Second)
	defer cancel()
	var errors []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	if h.config.ConfigStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.config.ConfigStore.Ping(reqCtx); err != nil {
				mu.Lock()
				errors = append(errors, "config store not available")
				mu.Unlock()
			}
		}()
	}

	// Pinging log store
	if h.config.LogsStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.config.LogsStore.Ping(reqCtx); err != nil {
				mu.Lock()
				errors = append(errors, "log store not available")
				mu.Unlock()
			}
		}()
	}

	// Pinging vector store
	if h.config.VectorStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.config.VectorStore.Ping(reqCtx); err != nil {
				mu.Lock()
				errors = append(errors, "vector store not available")
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if len(errors) > 0 {
		SendError(ctx, fasthttp.StatusServiceUnavailable, errors[0])
		return
	}
	SendJSON(ctx, map[string]any{"status": "ok", "components": map[string]any{"db_pings": "ok"}})
}

// getModelCatalogHealth handles GET /api/internal/health/model-catalog.
func (h *HealthHandler) getModelCatalogHealth(ctx *fasthttp.RequestCtx) {
	if h.config == nil || h.config.ModelCatalog == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "model catalog is not initialized")
		return
	}

	report := h.config.ModelCatalog.GetProviderModelSnapshotHealthReport()
	statusCode := fasthttp.StatusOK
	if report.Status == modelcatalog.ProviderModelHealthError {
		statusCode = fasthttp.StatusServiceUnavailable
	}

	SendJSONWithStatus(ctx, report, statusCode)
}
