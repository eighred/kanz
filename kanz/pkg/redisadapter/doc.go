// Package redisadapter holds the concrete go-redis bindings for the platform's
// shared-state seams (PARITY-04h / DEBT-02): bus.RedisClient (the RedisDedup
// cross-pod seen-set) and integrity.RedisEval (the reconciler's Lua-atomic
// PendingStore). The real binding lives behind the `redis` build tag
// (goredis.go), so the default module build — every `go build/vet ./...`, all
// tests, the arch test — pulls NO go-redis dependency (the CLAUDE.md no-bloat
// rule, the same composition-root stance as the copilot anthropic adapter,
// PARITY-04a). Deployments that run N replicas build their consumers with
// `-tags redis` and wire New over their *redis.Client / cluster / Dragonfly
// client; a single value satisfies both seams, so one connection serves dedup
// and reconciliation.
package redisadapter
