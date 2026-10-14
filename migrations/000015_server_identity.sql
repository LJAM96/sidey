-- Stable server identity for companion discovery (Phase 6).
--
-- One row, seeded at migration time. The UUID never changes: companions
-- pin it to recognise this control plane across LAN DHCP and tailnet IP
-- changes. Rename with: UPDATE server_identity SET name = '...';
CREATE TABLE server_identity (
    id uuid NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    name text NOT NULL DEFAULT 'Sidey',
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO server_identity (name) VALUES ('Sidey');
COMMENT ON TABLE server_identity IS 'Stable control-plane UUID for companion auto-discovery; advertised via mDNS and server-identity.';
