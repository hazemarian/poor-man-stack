# Security Findings & Improvement Plan

This document outlines key security findings, architectural vulnerabilities, and reliability improvements for `pmcluster`. Each section includes the root cause, impacted files, and actionable steps for implementation.

---

## 1. Denial-of-Service in Token Lookup (Critical / High Priority)

### Problem
In [`store/users.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/store/users.go#L42-L68), `UserByToken` iterates through **all** registered user rows and executes `auth.VerifyToken` (Argon2id) on every single row until a match is found:

```go
func (s *Store) UserByToken(ctx context.Context, token string) (*auth.User, error) {
    rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash FROM users`)
    ...
    for rows.Next() {
        ...
        ok, err := auth.VerifyToken(token, hash) // Runs Argon2id (64MB memory) per row
    }
}
```

Since Argon2id is intentionally CPU and memory heavy (64 MiB RAM per call), an unauthenticated attacker sending arbitrary invalid tokens will cause the daemon to evaluate Argon2id $N$ times per request (where $N$ is the number of stored users), leading to host CPU/memory exhaustion.

### Solution / Task Steps
- **Token Format:** Change generated API tokens to include an unhashed public lookup key, e.g., `pmc_<token_id>_<secret_key>` (or store a fast hash like SHA256 of the token as a database index).
- **Store Lookup:** Look up the exact user row by token prefix/ID or SHA256 index first.
- **Verification:** Run `auth.VerifyToken` **only** against the single target row returned by SQLite.
- **Impacted Files:**
  - [`internal/auth/token.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/auth/token.go)
  - [`internal/store/users.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/store/users.go)
  - [`internal/auth/middleware.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/auth/middleware.go)

---

## 2. Webhook Replay Attack Vulnerability (Medium Priority)

### Problem
The webhook receiver in [`webhook/webhook.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/webhook/webhook.go#L117-L121) verifies HMAC-SHA256 signatures over the payload body:

```go
mac := hmac.New(sha256.New, secret)
mac.Write(body)
```

However, it does not check for timestamps or nonces. If an attacker intercepts a legitimate deployment webhook request and header, they can replay the exact HTTP request indefinitely to force unauthorized redeployments or rollbacks.

### Solution / Task Steps
- **Timestamp Validation:** Require a timestamp header (e.g., `X-Pmcluster-Timestamp` or `X-GitHub-Delivery` / timestamp field) or accept timestamps in the JSON payload body.
- **Tolerance Window:** Calculate HMAC over `timestamp + body` (or enforce timestamp freshness within a 5-minute tolerance window). Reject stale requests.
- **Impacted Files:**
  - [`internal/webhook/webhook.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/webhook/webhook.go)

---

## 3. Unvalidated `RealIP` Proxy Headers (Medium Priority)

### Problem
In [`server/server.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/server/server.go#L40), chi's default `middleware.RealIP` is mounted at the top of the middleware stack:

```go
r.Use(middleware.RealIP)
```

`middleware.RealIP` blindly accepts `X-Forwarded-For` and `X-Real-IP` from any caller. If `pmcluster` receives requests directly (bypassing Traefik) or if Traefik passes untrusted upstream headers, callers can spoof client IP addresses in audit logs.

### Solution / Task Steps
- **Trusted Proxies:** Replace generic `middleware.RealIP` with header validation restricted to trusted proxy CIDRs (e.g. Docker network gateways or localhost `127.0.0.1`).
- **Impacted Files:**
  - [`internal/server/server.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/server/server.go)

---

## 4. Rate-Limiting Protection (Medium Priority)

### Problem
Neither `/api/*` nor `/webhook/{source}` currently enforce rate limits. An attacker or malfunctioning CI job can flood endpoints with requests.

### Solution / Task Steps
- **Middleware:** Add a rate-limiting middleware using `golang.org/x/time/rate` or a bucket rate-limiter.
- **Limits:** Set sane per-IP and per-source limits (e.g., 100 req/min for general API, 20 req/min for webhooks).
- **Impacted Files:**
  - [`internal/server/server.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/server/server.go)

---

## 5. Strict Mode for Pre-Deployment Backups (Low / Reliability Priority)

### Problem
In [`deploy/deploy.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/deploy/deploy.go#L165-L167), pre-deployment backups run in "best-effort" mode:

```go
if app.BackupBeforeDeploy {
    s.runPreDeployBackup(ctx, app.Name, revision)
}
```

If the backup fails, the failure is logged, but `DeployStack` executes anyway. For critical applications, operators may prefer to fail the deployment if the pre-deploy backup fails.

### Solution / Task Steps
- **Manifest Field:** Add a `strict_backup: true` boolean option to manifest specifications and `deploy.Payload`.
- **Abort Logic:** When enabled, if `runPreDeployBackup` returns an error, abort the deployment and return an error to the caller.
- **Impacted Files:**
  - [`internal/manifest/manifest.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/manifest/)
  - [`internal/deploy/deploy.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/deploy/deploy.go)

---

## 6. SQLite WAL Checkpoint Before Snapshot Backups (Reliability Priority)

### Problem
`offen/docker-volume-backup` creates tarball snapshots of Docker volumes. Since `pmcluster` operates SQLite in WAL mode (`journal_mode(WAL)`), taking a file-level snapshot while transactions are active in the WAL file can lead to incomplete database snapshots.

### Solution / Task Steps
- **Checkpoint Exec:** Before executing volume backups via `internal/backup`, execute `PRAGMA wal_checkpoint(TRUNCATE)` on the SQLite database connection to ensure all transactions are flushed to the main `.db` file.
- **Impacted Files:**
  - [`internal/store/store.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/store/store.go)
  - [`internal/backup/backup.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/backup/)
