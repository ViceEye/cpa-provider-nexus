package workbuddy

// This file exposes the workbuddy protocol entry points that the host plugin's
// dispatcher (internal/provider) calls when a credential's type is "workbuddy".
// Each function receives the same raw plugin-method payload the dispatcher
// got and returns the plugin-method response envelope.
