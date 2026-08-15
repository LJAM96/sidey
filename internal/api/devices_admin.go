package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"sidey/internal/audit"
)

type adminCreateDeviceRequest struct {
	Udid          string `json:"udid"`
	Platform      string `json:"platform"`
	DeviceName    string `json:"device_name"`
	Model         string `json:"model"`
	OsVersion     string `json:"os_version"`
	PairingStatus string `json:"pairing_status"`
}

func (s *Server) handleAdminCreateDevice(w http.ResponseWriter, r *http.Request) {
	var req adminCreateDeviceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Udid = strings.TrimSpace(req.Udid)
	req.Platform = strings.ToLower(strings.TrimSpace(req.Platform))
	req.DeviceName = strings.TrimSpace(req.DeviceName)
	req.Model = strings.TrimSpace(req.Model)
	req.OsVersion = strings.TrimSpace(req.OsVersion)
	req.PairingStatus = strings.ToLower(strings.TrimSpace(req.PairingStatus))

	if req.Udid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "udid is required"})
		return
	}
	if req.Platform == "" {
		req.Platform = "ios"
	}
	if req.PairingStatus == "" {
		req.PairingStatus = "paired"
	}

	var id uuid.UUID
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO devices (udid, platform, device_name, model, os_version, pairing_status, last_connected_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (udid) DO UPDATE SET
			platform = EXCLUDED.platform,
			device_name = CASE WHEN EXCLUDED.device_name <> '' THEN EXCLUDED.device_name ELSE devices.device_name END,
			model = CASE WHEN EXCLUDED.model <> '' THEN EXCLUDED.model ELSE devices.model END,
			os_version = CASE WHEN EXCLUDED.os_version <> '' THEN EXCLUDED.os_version ELSE devices.os_version END,
			pairing_status = EXCLUDED.pairing_status,
			last_connected_at = now()
		RETURNING id`,
		req.Udid, req.Platform, req.DeviceName, req.Model, req.OsVersion, req.PairingStatus).Scan(&id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "saving device failed")
		return
	}

	s.audit.Record(r.Context(), "admin", "device.created", audit.WithData(map[string]any{
		"device_id": id,
		"udid":      req.Udid,
		"platform":  req.Platform,
		"name":      req.DeviceName,
	}))

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             id.String(),
		"udid":           req.Udid,
		"device_name":    req.DeviceName,
		"platform":       req.Platform,
		"model":          req.Model,
		"os_version":     req.OsVersion,
		"pairing_status": req.PairingStatus,
	})
}

func (s *Server) handleAdminDeleteDevice(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid device id"})
		return
	}

	res, err := s.pool.Exec(r.Context(), `DELETE FROM devices WHERE id = $1`, id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "device delete failed")
		return
	}
	if res.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}

	s.audit.Record(r.Context(), "admin", "device.deleted", audit.WithData(map[string]any{"device_id": id}))
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id.String()})
}
