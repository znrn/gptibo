CREATE TABLE partner_tickets (
  id TEXT PRIMARY KEY,
  partner_id TEXT NOT NULL REFERENCES partners(id),
  order_id TEXT NOT NULL REFERENCES orders(id),
  subject TEXT NOT NULL CHECK(length(subject) BETWEEN 1 AND 120),
  description TEXT NOT NULL CHECK(length(description) BETWEEN 1 AND 5000),
  status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','processing','waiting','resolved')),
  version INTEGER NOT NULL DEFAULT 0,
  last_event_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX partner_tickets_owner ON partner_tickets(partner_id,updated_at DESC,id);
CREATE INDEX partner_tickets_queue ON partner_tickets(status,updated_at DESC,id);
CREATE TABLE partner_ticket_events (
  id TEXT PRIMARY KEY,
  ticket_id TEXT NOT NULL REFERENCES partner_tickets(id),
  actor_role TEXT NOT NULL CHECK(actor_role IN ('admin','partner')),
  actor_id TEXT NOT NULL,
  body TEXT NOT NULL CHECK(length(body) BETWEEN 1 AND 5000),
  status TEXT NOT NULL CHECK(status IN ('open','processing','waiting','resolved')),
  version INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(ticket_id,version)
);
CREATE TABLE partner_ticket_notifications (
  id INTEGER PRIMARY KEY CHECK(id=1),
  version INTEGER NOT NULL DEFAULT 0
);
INSERT INTO partner_ticket_notifications(id) VALUES(1);
-- Count committed partner messages, including replies that reopen old tickets.
-- Admin replies and idempotent retries must not trigger a new-message alert.
CREATE TRIGGER partner_ticket_notify AFTER INSERT ON partner_ticket_events
WHEN NEW.actor_role='partner'
BEGIN
  UPDATE partner_ticket_notifications SET version=version+1 WHERE id=1;
END;
