package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type table struct {
	Columns []string  `json:"columns"`
	Rows    []jsonRow `json:"rows"`
}

type jsonRow map[string]any

func (s *Server) queryTable(w http.ResponseWriter, r *http.Request, sql string, args ...any) {
	rows, err := s.pool.Query(r.Context(), sql, args...)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	columns := make([]string, len(fields))
	for i, f := range fields {
		columns[i] = string(f.Name)
	}
	table := table{Columns: columns, Rows: []jsonRow{}}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "row decode failed")
			return
		}
		row := make(jsonRow, len(columns))
		for i, v := range values {
			if u, ok := v.(uuid.UUID); ok {
				v = u.String()
			} else if b, ok := v.([16]byte); ok {
				v = uuid.UUID(b).String()
			} else if b, ok := v.([16]uint8); ok {
				v = uuid.UUID(b).String()
			} else if b, ok := v.([]byte); ok {
				if len(b) == 16 && (columns[i] == "id" || strings.HasSuffix(columns[i], "_id")) {
					v = uuid.UUID(b).String()
				} else {
					v = string(b)
				}
			}
			row[columns[i]] = v
		}
		table.Rows = append(table.Rows, row)
	}
	if rows.Err() != nil {
		s.writeError(w, http.StatusInternalServerError, "row iteration failed")
		return
	}
	writeJSON(w, http.StatusOK, table)
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT id, name, architecture, operating_system, software_version,
		       tailnet_identity, connection_state, last_heartbeat_at,
		       capabilities, enrolled_at
		FROM agents ORDER BY created_at`)
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT d.id, d.udid, d.platform, d.device_name, d.model, d.os_version,
		       a.name AS agent_name, d.pairing_status,
		       d.developer_mode_enabled, d.last_connected_at,
		       d.last_inventory_scan_at
		FROM devices d LEFT JOIN agents a ON a.id = d.agent_id
		ORDER BY d.created_at`)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT id, job_type, device_id, application_id, state, attempt,
		       progress, parameters, claimed_by, lease_expires_at,
		       error_category, error_details, retry_at, result, created_at,
		       started_at, completed_at
		FROM jobs ORDER BY created_at DESC LIMIT 200`)
}

func (s *Server) handleListApplications(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT id, name, publisher, description, icon_ref,
		       default_update_policy, trust_classification, created_at
		FROM applications ORDER BY created_at`)
}

// handleListArtifacts lists the IPA repository with metadata and quarantine
// state, newest first.
func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT id, sha256, filename, version, build_number, bundle_identifier,
		       platform, min_os_version, source, quarantine_state, imported_at
		FROM artifacts ORDER BY imported_at DESC LIMIT 200`)
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT id, device_id, channel_id, target, desired_version,
		       update_policy, current_state, updated_at
		FROM deployments ORDER BY updated_at DESC LIMIT 200`)
}

// handleListRefresh exposes the refresh calendar: each deployment's current
// profile expiry, when the next refresh is due, and the outcome of the last
// one, alongside the state of its most recent refresh job.
func (s *Server) handleListRefresh(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT d.device_name, d.udid, a.name AS agent_name,
		       ir.provisioning_expiry_at AS profile_expiry_at,
		       dep.next_refresh_due_at,
		       dep.last_refresh_at, dep.last_refresh_result, dep.last_refresh_error,
		       j.state AS refresh_job_state, j.attempt, j.error_category,
		       j.error_details AS job_error, j.updated_at AS job_updated_at
		FROM deployments dep
		JOIN devices d ON d.id = dep.device_id
		JOIN installation_records ir ON ir.deployment_id = dep.id
		LEFT JOIN agents a ON a.id = d.agent_id
		LEFT JOIN LATERAL (
			SELECT j.state, j.attempt, j.error_category, j.error_details, j.updated_at
			FROM jobs j
			WHERE j.device_id = dep.device_id AND j.job_type = 'refresh'
			ORDER BY j.created_at DESC LIMIT 1
		) j ON true
		ORDER BY COALESCE(ir.provisioning_expiry_at, dep.next_refresh_due_at)`)
}
