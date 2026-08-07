package api

import (
	"net/http"
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
			if b, ok := v.([]byte); ok {
				v = string(b)
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
		       error_category, error_details, retry_at, created_at,
		       started_at, completed_at
		FROM jobs ORDER BY created_at DESC LIMIT 200`)
}

func (s *Server) handleListApplications(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT id, name, publisher, description, icon_ref,
		       default_update_policy, trust_classification, created_at
		FROM applications ORDER BY created_at`)
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT id, device_id, channel_id, target, desired_version,
		       update_policy, current_state, updated_at
		FROM deployments ORDER BY updated_at DESC LIMIT 200`)
}
