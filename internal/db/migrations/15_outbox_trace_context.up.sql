-- The trace context of the change that wrote an outbox event (#50): the W3C
-- traceparent of its request, so that the relay that publishes the event,
-- maybe on another instance, continues the trace of that request. NULL
-- for an event written outside a sampled trace.
ALTER TABLE outbox_events ADD COLUMN trace_context TEXT
    CONSTRAINT outbox_events_trace_context_check CHECK (length(trace_context) <= 256);
