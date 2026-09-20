package platform

import "log/slog"

// LogValue prevents structured JSON logging from serializing the bearer token.
// Stringer alone is not sufficient for slog.Any's JSON encoding.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("role", string(c.Role)), slog.String("dataDir", c.DataDir), slog.Int("sources", len(c.Sources)), slog.String("token", "[redacted]"))
}
