package reader

import (
	"database/sql"
	"encoding/json"
	"errors"
)

const analysisIdentityOwner = "analysis-model-identity"

type analysisModelIdentity struct {
	Version     int    `json:"version"`
	Selection   string `json:"selection"`
	Connection  string `json:"connection"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Fingerprint string `json:"fingerprint"`
}

func (s *Server) rememberAnalysisIdentity(c paperAnalysisConnection) error {
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	identity := analysisModelIdentity{Version: 1, Selection: c.selection, Connection: c.connection, Provider: c.provider, Model: c.model, Fingerprint: c.fingerprint}
	if _, err := tx.Exec("INSERT INTO collection_cursors(owner_key,body,updated_at) VALUES(?,?,?) ON CONFLICT(owner_key) DO UPDATE SET body=excluded.body,updated_at=excluded.updated_at", analysisIdentityOwner, mustJSONText(identity), now()); err != nil {
		return err
	}
	return tx.Commit()
}

// Lists and details use this already-resolved identity without contacting a
// model. A configuration change invalidates projections immediately.
func (s *Server) activeAnalysisFingerprint() (string, error) {
	var body string
	if err := s.Store.DB.QueryRow("SELECT body FROM collection_cursors WHERE owner_key=?", analysisIdentityOwner).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	var identity analysisModelIdentity
	if err := json.Unmarshal([]byte(body), &identity); err != nil {
		return "", err
	}
	if identity.Version != 1 {
		return "", errors.New("分析身份版本不受支持")
	}
	cfg, err := s.Store.CollectionSettings()
	if err != nil {
		return "", err
	}
	ai := s.aiConfig()
	capability := identity.Provider
	if capability == "api" {
		capability = apiCapabilityKey(identity.Model)
	}
	if identity.Selection != analysisSelection(cfg, ai.Primary) || identity.Connection != configPrint(ai, capability) {
		return "", nil
	}
	return identity.Fingerprint, nil
}
