-- Trace context for outbox events. See "Observability" in docs/design.md.

-- +goose Up

-- The W3C traceparent of the request that wrote the event. The relay sends
-- it as a queue message attribute and the worker continues the trace, so
-- one trace spans API -> outbox -> queue -> worker -> rail. It is an
-- identifier, not data: 55 characters such as
-- 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01.
ALTER TABLE outbox ADD COLUMN traceparent text
    CONSTRAINT outbox_traceparent_len CHECK (length(traceparent) <= 64);

-- +goose Down
ALTER TABLE outbox DROP COLUMN traceparent;
