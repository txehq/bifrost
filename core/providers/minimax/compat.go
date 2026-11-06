// Fork (txehq/bifrost): names the fork's earlier MiniMax provider exported, kept so existing Go
// callers of github.com/capsohq/bifrost/core/providers/minimax still compile after the provider
// moved to upstream's implementation (d20cb6ace).

package minimax

import schemas "github.com/capsohq/bifrost/core/schemas"

// MinimaxProvider is the fork's earlier name for MiniMaxProvider.
//
// Deprecated: use MiniMaxProvider.
type MinimaxProvider = MiniMaxProvider

// NewMinimaxProvider is the fork's earlier constructor, with its original signature.
//
// Deprecated: use NewMiniMaxProvider.
func NewMinimaxProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*MinimaxProvider, error) {
	return NewMiniMaxProvider(config, logger)
}
