package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"sync"
	"time"

	"github.com/SigNoz/signoz/pkg/sqlstore"
	"github.com/SigNoz/signoz/pkg/types/opamptypes"
	"github.com/SigNoz/signoz/pkg/valuer"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/open-telemetry/opamp-go/protobufs"
	opampTypes "github.com/open-telemetry/opamp-go/server/types"
)

type Agent struct {
	opamptypes.StorableAgent
	remoteConfig *protobufs.AgentRemoteConfig
	Status       *protobufs.AgentToServer

	// can this agent be load balancer
	CanLB bool

	// is this agent setup as load balancer
	IsLb bool

	conn      opampTypes.Connection
	connMutex sync.Mutex
	mux       sync.RWMutex
	store     sqlstore.SQLStore
	logger    *slog.Logger
}

// set in agent description when agent is capable of supporting
// lb exporter configuration. values: 1 (true) or 0 (false)
const lbExporterFlag = "capabilities.lbexporter"

func New(store sqlstore.SQLStore, logger *slog.Logger, orgID valuer.UUID, agentID string, conn opampTypes.Connection) *Agent {
	return &Agent{
		StorableAgent: opamptypes.NewStorableAgent(store, orgID, agentID, opamptypes.AgentStatusConnected),
		conn:          conn,
		store:         store,
		logger:        logger,
	}
}

// Upsert inserts or updates the agent in the database.
func (agent *Agent) Upsert() error {
	agent.mux.Lock()
	defer agent.mux.Unlock()

	_, err := agent.store.BunDB().NewInsert().
		Model(&agent.StorableAgent).
		On("CONFLICT (agent_id) DO UPDATE").
		Set("updated_at = EXCLUDED.updated_at").
		Set("config = EXCLUDED.config").
		Set("status = EXCLUDED.status").
		Exec(context.Background())
	if err != nil {
		return err
	}

	return nil
}

// keep only the last 50 agents in the database
func (agent *Agent) KeepOnlyLast50Agents(ctx context.Context) {
	// Delete all agents except the last 50 in a single query
	_, err := agent.store.BunDB().
		NewDelete().
		Model(new(opamptypes.StorableAgent)).
		Where("org_id = ?", agent.OrgID).
		Where("agent_id NOT IN (?)",
			agent.store.BunDB().
				NewSelect().
				ColumnExpr("distinct(agent_id)").
				Model(new(opamptypes.StorableAgent)).
				Where("org_id = ?", agent.OrgID).
				OrderExpr("created_at DESC").
				Limit(50)).
		Exec(ctx)
	if err != nil {
		agent.logger.Error("failed to delete old agents", "error", err)
	}
}

// extracts lb exporter support flag from agent description. the flag
// is used to decide if lb exporter can be enabled on the agent.
func ExtractLbFlag(agentDescr *protobufs.AgentDescription) bool {

	if agentDescr == nil {
		return false
	}

	if len(agentDescr.NonIdentifyingAttributes) > 0 {
		for _, kv := range agentDescr.NonIdentifyingAttributes {
			anyvalue, ok := kv.Value.Value.(*protobufs.AnyValue_StringValue)
			if !ok {
				continue
			}
			if kv.Key == lbExporterFlag && anyvalue.StringValue == "1" {
				// agent can support load balancer config
				return true
			}
		}
	}
	return false
}

func (agent *Agent) updateAgentDescription(newStatus *protobufs.AgentToServer) (agentDescrChanged bool) {
	prevStatus := agent.Status

	if agent.Status == nil {
		// First time this Agent reports a status, remember it.
		agent.Status = newStatus
		agentDescrChanged = true
	} else {
		// Not a new Agent. Update the Status.
		agent.Status.SequenceNum = newStatus.SequenceNum

		// Check what's changed in the AgentDescription.
		if newStatus.AgentDescription != nil {
			// If the AgentDescription field is set it means the Agent tells us
			// something is changed in the field since the last status report
			// (or this is the first report).
			// Make full comparison of previous and new descriptions to see if it
			// really is different.
			if prevStatus != nil && proto.Equal(prevStatus.AgentDescription, newStatus.AgentDescription) {
				// Agent description didn't change.
				agentDescrChanged = false
			} else {
				// Yes, the description is different, update it.
				agent.Status.AgentDescription = newStatus.AgentDescription
				agentDescrChanged = true
			}
		} else {
			// AgentDescription field is not set, which means description didn't change.
			agentDescrChanged = false
		}

		// Update remote config status if it is included and is different from what we have.
		if newStatus.RemoteConfigStatus != nil &&
			!proto.Equal(agent.Status.RemoteConfigStatus, newStatus.RemoteConfigStatus) {
			agent.Status.RemoteConfigStatus = newStatus.RemoteConfigStatus

			// todo: need to address multiple agent scenario here
			// for now, the first response will be sent back to the UI
			if agent.Status.RemoteConfigStatus.Status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED {
				onConfigSuccess(agent.OrgID, agent.AgentID, string(agent.Status.RemoteConfigStatus.LastRemoteConfigHash))
			}

			if agent.Status.RemoteConfigStatus.Status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED {
				onConfigFailure(agent.OrgID, agent.AgentID, string(agent.Status.RemoteConfigStatus.LastRemoteConfigHash), agent.Status.RemoteConfigStatus.ErrorMessage)
			}
		}
	}

	if agentDescrChanged {
		agent.CanLB = ExtractLbFlag(newStatus.AgentDescription)
	}

	return agentDescrChanged
}

func (agent *Agent) updateHealth(newStatus *protobufs.AgentToServer) {
	if newStatus.Health == nil {
		return
	}

	agent.Status.Health = newStatus.Health

	if agent.Status != nil && agent.Status.Health != nil && agent.Status.Health.Healthy {
		agent.TimeAuditable.UpdatedAt = time.Unix(0, int64(agent.Status.Health.StartTimeUnixNano)).UTC()
	}
}

func (agent *Agent) updateRemoteConfigStatus(newStatus *protobufs.AgentToServer) {
	// Update remote config status if it is included and is different from what we have.
	if newStatus.RemoteConfigStatus != nil {
		agent.Status.RemoteConfigStatus = newStatus.RemoteConfigStatus
	}
}

func (agent *Agent) updateStatusField(newStatus *protobufs.AgentToServer) (agentDescrChanged bool) {
	if agent.Status == nil {
		// First time this Agent reports a status, remember it.
		agent.Status = newStatus
		agentDescrChanged = true
	}

	agentDescrChanged = agent.updateAgentDescription(newStatus) || agentDescrChanged
	agent.updateRemoteConfigStatus(newStatus)
	agent.updateHealth(newStatus)
	return agentDescrChanged
}

func (agent *Agent) updateEffectiveConfig(newStatus *protobufs.AgentToServer, response *protobufs.ServerToAgent) {
	// Update effective config if provided.
	if newStatus.EffectiveConfig != nil {
		if newStatus.EffectiveConfig.ConfigMap != nil {
			agent.Status.EffectiveConfig = newStatus.EffectiveConfig

			// Convert to string for displaying purposes.
			agent.Config = ""
			// There should be only one config in the map.
			for _, cfg := range newStatus.EffectiveConfig.ConfigMap.ConfigMap {
				agent.Config = string(cfg.Body)
			}
		}
	}
}

func (agent *Agent) hasCapability(capability protobufs.AgentCapabilities) bool {
	return agent.Status.Capabilities&uint64(capability) != 0
}

func (agent *Agent) UpdateStatus(
	statusMsg *protobufs.AgentToServer,
	response *protobufs.ServerToAgent,
	configProvider AgentConfigProvider,
) {
	agent.mux.Lock()
	defer agent.mux.Unlock()
	agent.processStatusUpdate(statusMsg, response, configProvider)
}

func (agent *Agent) processStatusUpdate(
	newStatus *protobufs.AgentToServer,
	response *protobufs.ServerToAgent,
	configProvider AgentConfigProvider,
) {
	// We don't have any status for this Agent, or we lost the previous status update from the Agent, so our
	// current status is not up-to-date.
	lostPreviousUpdate := (agent.Status == nil) || (agent.Status != nil && agent.Status.SequenceNum+1 != newStatus.SequenceNum)

	agentDescrChanged := agent.updateStatusField(newStatus)

	// Check if any fields were omitted in the status report.
	effectiveConfigOmitted := newStatus.EffectiveConfig == nil &&
		agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig)

	remoteConfigStatusOmitted := newStatus.RemoteConfigStatus == nil &&
		agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig)

	healthOmitted := newStatus.Health == nil &&
		agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth)

	// True if the status was not fully reported.
	statusIsCompressed := effectiveConfigOmitted || remoteConfigStatusOmitted || healthOmitted

	if statusIsCompressed && lostPreviousUpdate {
		// The status message is not fully set in the message that we received, but we lost the previous
		// status update. Request full status update from the agent.
		response.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
	}

	// This needs to be done before agent.updateRemoteConfig() to ensure it sees
	// the latest value for agent.EffectiveConfig when generating a config recommendation
	agent.updateEffectiveConfig(newStatus, response)

	configChanged := false
	if agentDescrChanged {
		// Agent description is changed, but effective config is missing, force request agent to send Config
		//
		// Note: ideally this flag should be sent along side ErrorResponse;
		// but OpAMP agent prioritizes Flags before ErrorResponse hence sending
		// requests consequently without respecting the retry cooldown, if in future that changes,
		// it should be shifted there; To test uncomment Flags added in opamp_server.go
		if newStatus.EffectiveConfig == nil || newStatus.EffectiveConfig.ConfigMap == nil {
			response.Flags = uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
			return
		}

		//Get the default org ID
		// agent.

		// We need to recalculate the config.
		configChanged = agent.updateRemoteConfig(configProvider)
	}

	// If remote config is changed and different from what the Agent has then
	// send the new remote config to the Agent.
	if configChanged ||
		(agent.Status.RemoteConfigStatus != nil && agent.remoteConfig != nil &&
			!bytes.Equal(agent.Status.RemoteConfigStatus.LastRemoteConfigHash, agent.remoteConfig.ConfigHash)) {
		// The new status resulted in a change in the config of the Agent or the Agent
		// does not have this config (hash is different). Send the new config the Agent.
		response.RemoteConfig = agent.remoteConfig
		agent.SendToAgent(response)

		ListenToConfigUpdate(
			agent.OrgID,
			agent.AgentID,
			string(response.RemoteConfig.ConfigHash),
			configProvider.ReportConfigDeploymentStatus,
		)
	}
}

func (agent *Agent) updateRemoteConfig(configProvider AgentConfigProvider) bool {
	recommendedConfig, confId, err := configProvider.RecommendAgentConfig(agent.OrgID, []byte(agent.Config))
	if err != nil {
		zap.L().Error("could not generate config recommendation for agent", zap.String("agentID", agent.AgentID), zap.Error(err))
		return false
	}

	cfg := protobufs.AgentRemoteConfig{
		Config: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigFile{},
		},
	}

	cfg.Config.ConfigMap[CollectorConfigFilename] = &protobufs.AgentConfigFile{
		Body:        recommendedConfig,
		ContentType: "application/x-yaml",
	}

	if len(confId) < 1 {
		// Should never happen. Handle gracefully if it does by some chance.
		zap.L().Error("config provider recommended a config with empty confId. Using content hash for configId")

		hash := sha256.New()
		for k, v := range cfg.Config.ConfigMap {
			hash.Write([]byte(k))
			hash.Write(v.Body)
			hash.Write([]byte(v.ContentType))
		}
		cfg.ConfigHash = hash.Sum(nil)
	} else {
		cfg.ConfigHash = []byte(confId)
	}

	configChanged := !isEqualRemoteConfig(agent.remoteConfig, &cfg)

	agent.remoteConfig = &cfg

	return configChanged
}

func isEqualRemoteConfig(c1, c2 *protobufs.AgentRemoteConfig) bool {
	if c1 == c2 {
		return true
	}
	if c1 == nil || c2 == nil {
		return false
	}
	return isEqualConfigSet(c1.Config, c2.Config)
}

func isEqualConfigSet(c1, c2 *protobufs.AgentConfigMap) bool {
	if c1 == c2 {
		return true
	}
	if c1 == nil || c2 == nil {
		return false
	}
	if len(c1.ConfigMap) != len(c2.ConfigMap) {
		return false
	}
	for k, v1 := range c1.ConfigMap {
		v2, ok := c2.ConfigMap[k]
		if !ok {
			return false
		}
		if !isEqualConfigFile(v1, v2) {
			return false
		}
	}
	return true
}

func isEqualConfigFile(f1, f2 *protobufs.AgentConfigFile) bool {
	if f1 == f2 {
		return true
	}
	if f1 == nil || f2 == nil {
		return false
	}
	return bytes.Equal(f1.Body, f2.Body) && f1.ContentType == f2.ContentType
}

func (agent *Agent) SendToAgent(msg *protobufs.ServerToAgent) {
	agent.connMutex.Lock()
	defer agent.connMutex.Unlock()

	_ = agent.conn.Send(context.Background(), msg)
}
