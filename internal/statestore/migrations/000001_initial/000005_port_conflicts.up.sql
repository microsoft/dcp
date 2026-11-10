CREATE INDEX IF NOT EXISTS idx_port_allocations_protocol_port ON port_allocations(protocol, port, ip);
