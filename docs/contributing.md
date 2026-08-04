---
layout: default
title: Contributing
---

# Contributing to PVEKube

We welcome contributions! This guide covers development setup, architecture overview, and best practices for contributing.

## Development Setup

### Prerequisites

- **Go 1.23+** — [Download](https://golang.org/dl/)
- **Git**
- **Docker** (for testing template builds)
- **Linux or macOS** (Windows support via WSL2)

### Clone and Build

```bash
git clone https://github.com/mevijays/pvekube
cd golang-proxmox-clusterctl-k8s

# Build for your local platform
go build -o pvekube ./cmd/pvekube

# Run with verbose logging
./pvekube --data-dir ./test-data --listen 0.0.0.0:8080
```

### Development Server

For active development, rebuild and restart on file changes:

```bash
# Install air for hot-reload (optional)
go install github.com/cosmtrek/air@latest

# Create .air.toml in project root
cat > .air.toml <<'EOF'
root = "."
testdata_dir = "testdata"
tmp_dir = "tmp"

[build]
  cmd = "go build -o ./tmp/pvekube ./cmd/pvekube"
  bin = "./tmp/pvekube"
  full_bin = "./tmp/pvekube --data-dir ./test-data --listen 0.0.0.0:8080"
  args_bin = []
  delay = 1000
  poll = false
  poll_interval = 0
  include_ext = ["go", "tpl", "tmpl", "html"]
  exclude_dir = ["assets", "tmp", "vendor", "testdata"]
  include_dir = []
  include_file = []

[color]
  main = "magenta"
  watcher = "cyan"
  build = "yellow"
  command = "green"
EOF

# Run with hot-reload
air
```

## Code Structure

```
.
├── cmd/pvekube/
│   └── main.go               # Entry point
├── internal/
│   ├── config/               # CLI flags, configuration
│   ├── store/                # SQLite database, migrations
│   ├── auth/                 # Authentication, sessions, password hashing
│   ├── secrets/              # Encryption (AES-GCM), redaction
│   ├── jobs/                 # Job engine, persistence, SSE
│   ├── runner/               # Command execution wrapper
│   ├── proxmox/              # Proxmox REST client
│   │   ├── client.go         # HTTP client, auth
│   │   ├── discovery.go      # Node/bridge/pool discovery
│   │   └── permissions.go    # Permission checks
│   ├── bootstrap/            # KIND, clusterctl initialization
│   ├── imagebuilder/         # Packer/Ansible orchestration
│   ├── ipplan/               # Network validation
│   ├── capi/                 # Cluster API generators
│   ├── versions/             # Version pins
│   ├── server/               # HTTP server, handlers
│   │   ├── server.go         # Routes, middleware
│   │   ├── handlers_auth.go
│   │   ├── handlers_dashboard.go
│   │   ├── handlers_prereq.go
│   │   ├── handlers_templates.go
│   │   ├── handlers_clusters.go
│   │   └── handlers_cluster_detail.go
│   └── ui/                   # Embedded templates, assets
│       ├── embed.go          # Template registry
│       ├── templates/        # HTML templates
│       └── static/           # CSS, JavaScript, fonts
└── docs/                     # Documentation (you are here)
```

## Key Packages

### store (Internal Database)

Manages SQLite database with auto-migrations:

```go
// Open database
db, err := store.Open(dataDir)
if err != nil { panic(err) }

// Auto-migrates schema
// Query example
var name string
err := db.QueryRow("SELECT name FROM clusters WHERE id = ?", 1).Scan(&name)
```

**Migrations:** Located in `internal/store/migrations/` (SQL files numbered 0001_init.sql, etc.)

### jobs (Background Tasks)

Persistent job engine with SSE streaming:

```go
// Create a job
job := jobs.NewJob("cluster.apply")
job.AddStep("sync-credentials", "Syncing Proxmox credentials...")
job.AddStep("kubectl-apply", "Applying manifest...")

// Run the job
j.Run(ctx, func(log jobs.Logger) error {
  log("Step 1: Syncing...")
  // Do work...
  return nil
})

// Stream logs to client
w.Header().Set("Content-Type", "text/event-stream")
for line := range job.LogLines() {
  fmt.Fprintf(w, "data: %s\n\n", line)
}
```

### runner (Command Execution)

Wrapper for running external commands:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()

r := runner.New(ctx)
output, err := r.Capture("docker", "ps", "-a")
if err != nil { /* handle error */ }

// Ring buffer for large outputs
for line := range r.Lines() {
  log.Println(line)
}
```

### proxmox (REST Client)

Proxmox API interactions:

```go
// Initialize client
client := proxmox.NewClient("https://pve.example.com:8006", "token", "secret")

// Query nodes
nodes, err := client.GetNodes()

// Allocate VMID
vmid, err := client.NextVMID()

// Discover pools
pools, err := client.GetStoragePools()
```

### capi (Cluster API)

Generate and apply CAPI manifests:

```go
input := capi.GenerateInput{
  ClusterName: "my-k8s",
  TemplateVMID: 100,
  NodeIPRanges: []string{"10.10.10.20-10.10.10.100"},
  ControlPlaneEndpoint: "10.10.10.10",
  // ... more fields
}

spec := capi.GenerateSpec(runner, input)
err := spec.Run(ctx, log)
```

## Testing

### Unit Tests

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run specific package
go test ./internal/ipplan
```

### Test Structure

Create test files alongside source code:

```go
// internal/ipplan/ipplan_test.go
package ipplan_test

import (
  "testing"
  "golang-proxmox-clusterctl-k8s/internal/ipplan"
)

func TestValidateRange(t *testing.T) {
  plan := ipplan.Plan{
    Gateway: "10.10.10.1",
    Prefix: 24,
    ControlPlaneEndpoint: "10.10.10.10",
    NodeIPRange: ipplan.ParseRange("10.10.10.20-10.10.10.100"),
    MachineCount: 5,
  }
  
  issues := plan.Validate()
  if len(issues) > 0 {
    t.Errorf("unexpected validation errors: %v", issues)
  }
}
```

### Integration Testing

For testing with real Proxmox:

1. Set up a test Proxmox environment (or use a dedicated cluster)
2. Export credentials:
   ```bash
   export PVE_TEST_HOST=https://pve.example.com:8006
   export PVE_TEST_TOKEN=capmox@pve!test-token
   export PVE_TEST_SECRET=<token-secret>
   ```
3. Run tests:
   ```bash
   go test -v -run TestProxmox ./internal/proxmox
   ```

## Making Changes

### 1. Create a Feature Branch

```bash
git checkout -b feature/your-feature-name
git branch -u origin/feature/your-feature-name
```

### 2. Code Style

- Follow Go conventions (`gofmt`, `golint`)
- Avoid external dependencies; prefer stdlib
- Keep packages focused and cohesive
- Comment exported functions and types

### 3. Testing

- Add unit tests for new functions
- Update existing tests if behavior changes
- Run `go test ./...` before committing

### 4. Database Migrations

If you modify the SQLite schema:

1. Create a new migration file: `internal/store/migrations/0002_add_field.sql`
2. Update `internal/store/store.go` to increment the migration version
3. Add test coverage in `internal/store/store_test.go`

### 5. UI Changes

If you modify templates:

1. Update `.html` files in `internal/ui/templates/` or `internal/ui/templates/partials/`
2. Keep templates in the embed list in `internal/ui/embed.go`
3. Use Tailwind CSS classes and Font Awesome 4 icons
4. Test in browser: `./pvekube --data-dir ./test-data --listen 0.0.0.0:8080`

### 6. Version Pins

If you update Kubernetes, CAPI, or provider versions:

1. Update `internal/versions/versions.go`
2. Test against that version in `internal/bootstrap/` and `internal/imagebuilder/`
3. Document the change in commit message

## Pull Request Process

1. **Fork the repository** and create a feature branch
2. **Write tests** for new functionality
3. **Update documentation** if behavior changes
4. **Run tests:** `go test ./...`
5. **Submit PR** with clear description:
   - What problem does this solve?
   - How does the solution work?
   - Any breaking changes?
6. **Address review comments** and iterate

### PR Checklist

- [ ] Tests pass: `go test ./...`
- [ ] Code is formatted: `gofmt -s -w .`
- [ ] No hardcoded credentials or secrets
- [ ] Documentation updated (if needed)
- [ ] Commit messages are clear and descriptive

## Debugging

### Enable Debug Logging

```bash
# Set environment variable
export LOG_LEVEL=debug

# Or edit config to increase verbosity
./pvekube --data-dir ./test-data --listen 0.0.0.0:8080
```

### Inspect Database

```bash
# Install sqlite3 CLI
sqlite3 test-data/pvekube.db

# View clusters
SELECT name, status, created_at FROM clusters;

# View recent jobs
SELECT kind, status, error FROM jobs ORDER BY id DESC LIMIT 5;

# View jobs steps
SELECT job_id, step_index, name, status FROM jobs_steps;
```

### Check KIND Cluster

```bash
# Get kubeconfig
export KUBECONFIG=test-data/kubeconfigs/management.yaml

# View Cluster API objects
kubectl get cluster,machine,machinedeployment -A

# Check CAPMOX controller
kubectl logs -n capmox-system -l control-plane=controller-manager --tail=50
```

## Performance Optimization

### Database Queries

Use prepared statements for repeated queries:

```go
stmt, err := db.Prepare("SELECT id, name FROM clusters WHERE status = ?")
if err != nil { panic(err) }
defer stmt.Close()

rows, err := stmt.Query("Provisioned")
```

### Long-Running Operations

Use context for cancellation and timeouts:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

output, err := r.Capture("kubectl", "apply", "-f", manifest)
```

### Job Streaming

Keep SSE connections efficient by batching log lines:

```go
ticker := time.NewTicker(100 * time.Millisecond)
defer ticker.Stop()

for {
  select {
  case <-ticker.C:
    // Batch send logs to client
  }
}
```

## Security Considerations

### Credential Handling

- Always encrypt Proxmox secrets with AES-GCM before storing
- Use `secrets.Redactor` to mask secrets from logs
- Never log tokens or passwords

### Input Validation

- Validate DNS names (DNS-safe characters only)
- Validate IP addresses (CIDR notation)
- Sanitize user input in SQL queries (use parameterized queries)

### CSRF Protection

- Every form submission requires a session CSRF token
- Tokens are generated per session and validated on POST

## Common Tasks

### Add a New Handler

1. Create handler function in `internal/server/handlers_*.go`:
   ```go
   func (s *Server) handleMyFeature(w http.ResponseWriter, r *http.Request) {
     // Handle request
   }
   ```

2. Register route in `internal/server/server.go`:
   ```go
   mux.HandleFunc("GET /my-feature", s.handleMyFeature)
   ```

3. Create template in `internal/ui/templates/my_feature.html`

4. Register template in `internal/ui/embed.go`:
   ```go
   "my_feature": template.Must(template.New("my_feature.html").Parse(...)),
   ```

### Add a Database Field

1. Create migration: `internal/store/migrations/000X_add_field.sql`
   ```sql
   ALTER TABLE clusters ADD COLUMN new_field TEXT;
   ```

2. Update version in `internal/store/store.go`

3. Reload data access layer and test

### Add a New Prerequisite Check

1. Create check function in `internal/server/handlers_prereq.go`
2. Add to `checks` array in `handlePrerequistesCheck`
3. Return status to client

## Reporting Issues

Please include:
- **Go version:** `go version`
- **OS/Arch:** `uname -a` or `Get-WmiObject Win32_OperatingSystem`
- **Proxmox version:** From web UI
- **Error message:** Full error from logs
- **Steps to reproduce:** Clear, reproducible example

## License

By contributing, you agree that your contributions will be licensed under the Apache 2.0 license.

---

**Questions?** Open an issue or start a [discussion](https://github.com/mevijays/pvekube/discussions).

**[← Back to Docs](/)** | **[Troubleshooting](troubleshooting/)**
