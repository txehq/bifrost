package configstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Configstore gives accepted callbacks crash durability, prompt reads, and
// transactional retry updates. Memory KV is ephemeral, and telemetry stores such
// as ClickHouse logstore are unsuitable for mutable queue state.

// pushConfigToTable flattens the domain push config into its row. SecretVars are
// cloned because the row's BeforeSave hook encrypts values in place, and the
// caller's copy must stay usable plaintext.
func pushConfigToTable(c *schemas.AgentPushConfig) tables.TableAgentPushConfig {
	return tables.TableAgentPushConfig{
		AgentName:        c.AgentName,
		TaskID:           c.TaskID,
		ConfigID:         c.ConfigID,
		URL:              c.URL,
		Tenant:           c.Tenant,
		Token:            c.Token.Clone(),
		AuthScheme:       c.AuthScheme,
		AuthCredentials:  c.AuthCredentials.Clone(),
		IngressTokenHash: c.IngressTokenHash,
		CreatedAt:        c.CreatedAt,
		UpdatedAt:        c.UpdatedAt,
	}
}

func tableToPushConfig(row tables.TableAgentPushConfig) *schemas.AgentPushConfig {
	return &schemas.AgentPushConfig{
		AgentName:        row.AgentName,
		TaskID:           row.TaskID,
		ConfigID:         row.ConfigID,
		URL:              row.URL,
		Tenant:           row.Tenant,
		Token:            row.Token,
		AuthScheme:       row.AuthScheme,
		AuthCredentials:  row.AuthCredentials,
		IngressTokenHash: row.IngressTokenHash,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

// ErrAgentPushConfigTenantConflict reports a registration whose (agent, task,
// config) key another tenant already holds.
var ErrAgentPushConfigTenantConflict = errors.New("push configuration is registered for another tenant")

// SaveAgentPushConfig upserts one push callback registration. Upsert semantics
// match the A2A contract for setting a push configuration: re-registering the
// same (agent, task, config) key replaces the callback wholesale. The key does
// not include the tenant, so the replacement only applies to the same tenant's
// row; another tenant's row is left untouched and reported as a conflict.
func (s *RDBConfigStore) SaveAgentPushConfig(ctx context.Context, config *schemas.AgentPushConfig) error {
	row := pushConfigToTable(config)
	result := s.DB().WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "agent_name"}, {Name: "task_id"}, {Name: "config_id"}},
		// A row written before the tenant column existed holds SQL NULL, which reads
		// back as the empty tenant; it must match a tenantless re-registration.
		Where:     clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "COALESCE(config_agent_push_configs.tenant, '') = COALESCE(excluded.tenant, '')"}}},
		UpdateAll: true,
	}).Create(&row)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrAgentPushConfigTenantConflict
	}
	return nil
}

// BindAgentPushConfigTask attaches a task id to the exact pending embedded
// push config identified by its ingress token hash and internal pending task id.
// A row already bound by another path is left untouched. A send that embeds a
// config ID the task already holds re-registers it: like SaveAgentPushConfig,
// the bound row replaces the same tenant's registration, rotating its ingress
// token, while another tenant's row is reported as a conflict.
func (s *RDBConfigStore) BindAgentPushConfigTask(ctx context.Context, agentName, ingressTokenHash, pendingTaskID, taskID string) error {
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pending tables.TableAgentPushConfig
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&pending, "agent_name = ? AND ingress_token_hash = ? AND task_id = ?", agentName, ingressTokenHash, pendingTaskID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var existing tables.TableAgentPushConfig
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&existing, "agent_name = ? AND task_id = ? AND config_id = ?", agentName, taskID, pending.ConfigID).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
		case err != nil:
			return err
		case existing.Tenant != pending.Tenant:
			return ErrAgentPushConfigTenantConflict
		default:
			if err := tx.Delete(&tables.TableAgentPushConfig{}, "agent_name = ? AND task_id = ? AND config_id = ?", agentName, taskID, pending.ConfigID).Error; err != nil {
				return err
			}
		}
		return tx.Model(&tables.TableAgentPushConfig{}).
			Where("agent_name = ? AND ingress_token_hash = ? AND task_id = ?", agentName, ingressTokenHash, pendingTaskID).
			Updates(map[string]any{"task_id": taskID, "updated_at": time.Now().UTC()}).Error
	})
}

// GetAgentPushConfig loads one push config; a missing row is reported as
// (nil, nil) so callers outside this module need no error sentinel.
func (s *RDBConfigStore) GetAgentPushConfig(ctx context.Context, agentName, taskID, configID string) (*schemas.AgentPushConfig, error) {
	var row tables.TableAgentPushConfig
	err := s.DB().WithContext(ctx).First(&row, "agent_name = ? AND task_id = ? AND config_id = ?", agentName, taskID, configID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return tableToPushConfig(row), nil
}

// GetAgentPushConfigByIngressTokenHash resolves the push config an upstream push
// authenticated with, by the SHA-256 digest of the presented ingress token. A
// missing row is reported as (nil, nil).
func (s *RDBConfigStore) GetAgentPushConfigByIngressTokenHash(ctx context.Context, agentName, hash string) (*schemas.AgentPushConfig, error) {
	var row tables.TableAgentPushConfig
	err := s.DB().WithContext(ctx).First(&row, "agent_name = ? AND ingress_token_hash = ?", agentName, hash).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return tableToPushConfig(row), nil
}

// ListAgentPushConfigs returns every push config stored for one task in stable
// order.
func (s *RDBConfigStore) ListAgentPushConfigs(ctx context.Context, agentName, taskID string) ([]schemas.AgentPushConfig, error) {
	var rows []tables.TableAgentPushConfig
	if err := s.DB().WithContext(ctx).Order("config_id ASC").Find(&rows, "agent_name = ? AND task_id = ?", agentName, taskID).Error; err != nil {
		return nil, err
	}
	result := make([]schemas.AgentPushConfig, 0, len(rows))
	for _, row := range rows {
		result = append(result, *tableToPushConfig(row))
	}
	return result, nil
}

// ListAgentPushConfigsPaginated returns one filtered management page in stable
// order and the total number of matching rows before pagination.
func (s *RDBConfigStore) ListAgentPushConfigsPaginated(ctx context.Context, params schemas.AgentPushConfigQuery) ([]schemas.AgentPushConfig, int64, error) {
	query := s.DB().WithContext(ctx).Model(&tables.TableAgentPushConfig{})
	if len(params.AgentNames) > 0 {
		query = query.Where("agent_name IN ?", params.AgentNames)
	}
	for column, value := range map[string]string{"task_id": params.TaskID, "config_id": params.ConfigID, "url": params.URL} {
		if value != "" {
			query = query.Where("LOWER("+column+") LIKE ?", "%"+strings.ToLower(value)+"%")
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []tables.TableAgentPushConfig
	if err := query.Order("updated_at DESC, agent_name ASC, task_id ASC, config_id ASC").Offset(params.Offset).Limit(params.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	result := make([]schemas.AgentPushConfig, 0, len(rows))
	for _, row := range rows {
		result = append(result, *tableToPushConfig(row))
	}
	return result, total, nil
}

// ListAgentPushConfigAgentNames returns every distinct Agent name represented by
// stored push configurations, independent of the current page and filters.
func (s *RDBConfigStore) ListAgentPushConfigAgentNames(ctx context.Context) ([]string, error) {
	var names []string
	if err := s.DB().WithContext(ctx).Model(&tables.TableAgentPushConfig{}).Distinct("agent_name").Order("agent_name ASC").Pluck("agent_name", &names).Error; err != nil {
		return nil, err
	}
	return names, nil
}

// DeleteAgentPushConfig removes one push config, reporting whether a row
// existed. Deletion is idempotent so a repeated protocol delete is not an error.
func (s *RDBConfigStore) DeleteAgentPushConfig(ctx context.Context, agentName, taskID, configID string) (bool, error) {
	result := s.DB().WithContext(ctx).Delete(&tables.TableAgentPushConfig{}, "agent_name = ? AND task_id = ? AND config_id = ?", agentName, taskID, configID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// DeletePendingAgentPushConfig removes only the exact pending row created by
// one embedded send. If ingress already bound it to a durable task ID, the row
// no longer matches and remains intact.
func (s *RDBConfigStore) DeletePendingAgentPushConfig(ctx context.Context, agentName, ingressTokenHash, pendingTaskID string) (bool, error) {
	result := s.DB().WithContext(ctx).Delete(&tables.TableAgentPushConfig{}, "agent_name = ? AND ingress_token_hash = ? AND task_id = ?", agentName, ingressTokenHash, pendingTaskID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// CreateAgentPushDeliveryIfNotExists inserts one outbox row unless a row with the
// same content-digest ID already exists, reporting whether a new row was
// created. This is the deduplication point for upstream retransmissions.
func (s *RDBConfigStore) CreateAgentPushDeliveryIfNotExists(ctx context.Context, delivery *schemas.AgentPushDelivery) (bool, error) {
	row := tables.TableAgentPushDelivery{
		ID:               delivery.ID,
		AgentName:        delivery.AgentName,
		TaskID:           delivery.TaskID,
		ConfigID:         delivery.ConfigID,
		RegistrationHash: delivery.RegistrationHash,
		Payload:          delivery.Payload,
		Status:           delivery.Status,
		Attempts:         delivery.Attempts,
		NextAttemptAt:    delivery.NextAttemptAt,
		LastError:        delivery.LastError,
		CreatedAt:        delivery.CreatedAt,
		UpdatedAt:        delivery.UpdatedAt,
	}
	result := s.DB().WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// ListDueAgentPushDeliveries returns pending outbox rows whose next attempt time
// has arrived, oldest first, bounded so one relay pass cannot load the whole
// backlog.
func (s *RDBConfigStore) ListDueAgentPushDeliveries(ctx context.Context, now time.Time, limit int) ([]schemas.AgentPushDelivery, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("delivery batch limit must be positive")
	}
	var rows []tables.TableAgentPushDelivery
	if err := s.DB().WithContext(ctx).
		Where("status = ? AND next_attempt_at <= ? AND (claimed_until IS NULL OR claimed_until < ?)", schemas.AgentPushDeliveryStatusPending, now, now).
		Where(pushDeliveryHeadOfLine, schemas.AgentPushDeliveryStatusPending).
		Order("next_attempt_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]schemas.AgentPushDelivery, 0, len(rows))
	for _, row := range rows {
		result = append(result, schemas.AgentPushDelivery{
			ID:               row.ID,
			AgentName:        row.AgentName,
			TaskID:           row.TaskID,
			ConfigID:         row.ConfigID,
			RegistrationHash: row.RegistrationHash,
			Payload:          row.Payload,
			Status:           row.Status,
			Attempts:         row.Attempts,
			NextAttemptAt:    row.NextAttemptAt,
			LastError:        row.LastError,
			CreatedAt:        row.CreatedAt,
			UpdatedAt:        row.UpdatedAt,
		})
	}
	return result, nil
}

// pushDeliveryHeadOfLine keeps one registration's deliveries in the order they
// were accepted: a delivery is due only while no earlier one for the same
// agent/task/config accepted under the same registration (registration_hash;
// rows from before the column existed hold NULL and count as one registration)
// is still pending (for instance backing off after a failed attempt), so an
// append cannot overtake the replacement it follows and be overwritten by its
// retry. A replacement registration's deliveries are not held behind the old
// registration's rows, which can never reach the new callback and are
// dead-lettered when they next come due. Claims check it too, in the same
// statement, so the order holds across relay instances. Its one argument is
// the pending status.
const pushDeliveryHeadOfLine = `NOT EXISTS (SELECT 1 FROM config_agent_push_deliveries AS earlier
	WHERE earlier.agent_name = config_agent_push_deliveries.agent_name
	AND earlier.task_id = config_agent_push_deliveries.task_id
	AND earlier.config_id = config_agent_push_deliveries.config_id
	AND COALESCE(earlier.registration_hash, '') = COALESCE(config_agent_push_deliveries.registration_hash, '')
	AND earlier.status = ?
	AND (earlier.created_at < config_agent_push_deliveries.created_at
		OR (earlier.created_at = config_agent_push_deliveries.created_at AND earlier.id < config_agent_push_deliveries.id)))`

// ClaimAgentPushDelivery atomically leases one due pending delivery and returns
// its current persisted state. The exact lease expiry also fences the outcome
// update, so an attempt that outlives its lease cannot overwrite a later
// owner's result.
func (s *RDBConfigStore) ClaimAgentPushDelivery(ctx context.Context, id, runnerID string, leaseUntil time.Time) (*schemas.AgentPushDelivery, error) {
	now := time.Now().UTC()
	leaseUntil = normalizeLeaseTime(leaseUntil)
	if !leaseUntil.After(now) {
		return nil, fmt.Errorf("agent push delivery lease must expire in the future")
	}
	result := s.DB().WithContext(ctx).Model(&tables.TableAgentPushDelivery{}).
		Where("id = ? AND status = ? AND next_attempt_at <= ? AND (claimed_until IS NULL OR claimed_until < ?)", id, schemas.AgentPushDeliveryStatusPending, now, now).
		Where(pushDeliveryHeadOfLine, schemas.AgentPushDeliveryStatusPending).
		Updates(map[string]any{"claimed_by": runnerID, "claimed_until": leaseUntil})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	var row tables.TableAgentPushDelivery
	if err := s.DB().WithContext(ctx).
		Where("id = ? AND claimed_by = ? AND claimed_until = ?", id, runnerID, leaseUntil).
		First(&row).Error; err != nil {
		return nil, err
	}
	return &schemas.AgentPushDelivery{
		ID:               row.ID,
		AgentName:        row.AgentName,
		TaskID:           row.TaskID,
		ConfigID:         row.ConfigID,
		RegistrationHash: row.RegistrationHash,
		Payload:          row.Payload,
		Status:           row.Status,
		Attempts:         row.Attempts,
		NextAttemptAt:    row.NextAttemptAt,
		LastError:        row.LastError,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}, nil
}

// PruneAgentPushDeliveries removes only terminal rows whose final outcome is
// strictly older than before. Pending work and recent deduplication IDs remain.
func (s *RDBConfigStore) PruneAgentPushDeliveries(ctx context.Context, before time.Time) error {
	return s.DB().WithContext(ctx).
		Where("status IN ? AND updated_at < ?", []string{schemas.AgentPushDeliveryStatusDelivered, schemas.AgentPushDeliveryStatusDead}, before).
		Delete(&tables.TableAgentPushDelivery{}).Error
}

// UpdateAgentPushDeliveryOutcome records one attempt's result and releases its
// claim. The ownership fence prevents a stale worker from changing a row that
// another relay reclaimed after lease expiry.
func (s *RDBConfigStore) UpdateAgentPushDeliveryOutcome(ctx context.Context, delivery *schemas.AgentPushDelivery, runnerID string, leaseUntil time.Time) error {
	result := s.DB().WithContext(ctx).Model(&tables.TableAgentPushDelivery{}).
		Where("id = ? AND claimed_by = ? AND claimed_until = ?", delivery.ID, runnerID, normalizeLeaseTime(leaseUntil)).
		Updates(map[string]any{
			"status":          delivery.Status,
			"attempts":        delivery.Attempts,
			"next_attempt_at": delivery.NextAttemptAt,
			"last_error":      delivery.LastError,
			"claimed_by":      "",
			"claimed_until":   nil,
			"updated_at":      delivery.UpdatedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("agent push delivery not found or no longer owned by caller")
	}
	return nil
}
