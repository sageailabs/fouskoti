# Fouskoti: Helm Release Expander

## Purpose
Fouskoti expands Flux `HelmRelease` resources into their actual Kubernetes manifests for CI verification. It resolves charts from Git, Helm, and OCI repositories, allowing tools like kubeconform and Pluto to validate generated resources before deployment.

## Architecture

### Core Components
- **[cmd/](cmd/)**: Cobra CLI with `expand` as the default command. Commands inject context-bound loggers via `contextKeyLogger`
- **[pkg/repository/](pkg/repository/)**: Multi-repository chart loading system with three specialized loaders:
  - `gitRepoChartLoader` ([git.go](pkg/repository/git.go)): Clones Git repositories using Flux's gogit client
  - `helmRepoChartLoader` ([helm.go](pkg/repository/helm.go)): Downloads from HTTP/HTTPS Helm repos
  - `ociRepoChartLoader` ([oci.go](pkg/repository/oci.go)): Pulls OCI artifacts with auto-ECR authentication
- **[pkg/yaml/](pkg/yaml/)**: Kustomize kyaml wrappers for YAML node manipulation

### Key Data Flow
1. Read YAML from stdin/files → Parse for `HelmRelease` + source repos (GitRepository/HelmRepository/OCIRepository)
2. Route chart loading through factory pattern based on source kind
3. Render with Helm engine → Inject namespace → Recursively expand nested `HelmRelease` (up to `--max-expansions`)
4. Stream expanded manifests to stdout

## Development Workflows

### Building & Testing
```bash
make build          # Produces ./fouskoti binary
make test           # Runs Ginkgo test suite (requires internet for chart fetches)
make test/fast      # Uses go test (faster, less output)
make lint           # Runs go vet + golangci-lint
```

### CI Pipeline ([.github/workflows/](.github/workflows/))
- **[ci.yaml](.github/workflows/ci.yaml)**: Runs on push/PR to main - builds, tests with Ginkgo (`make test`), lints with golangci-lint
- **[release.yaml](.github/workflows/release.yaml)**: Triggered on tags - uses GoReleaser to build multi-platform binaries and create GitHub releases
- **[semantic_pr.yaml](.github/workflows/semantic_pr.yaml)**: Validates PR titles follow semantic commit conventions

Note: CI tests require internet access to fetch charts from remote repositories.

### Running Locally
```bash
# Default expand command (expand is implicit)
./fouskoti < input.yaml

# With explicit command
./fouskoti expand --kube-version=1.30 < manifests.yaml

# Working copy substitution for local chart testing
./fouskoti expand --working-copy-subst='ssh://git@github.com/myrepo/charts.git#main#/path/to/local/clone' < test.yaml
```

## Coding Patterns

### Error Handling
Always wrap errors with context using `fmt.Errorf` with `%w`:
```go
return fmt.Errorf("unable to parse %s: %w", fieldName, err)
```

### Logging
Use structured logging with context fields. Logger is injected via context in commands:
```go
ctx, logger := getContextAndLogger(cmd)
logger.With("namespace", ns, "name", name).Info("Processing")
```

### Testing with Ginkgo
- Use `ginkgo.Describe` + `ginkgo.It` BDD style (see [repository_test.go](pkg/repository/repository_test.go))
- Mock external dependencies: `GitClientMock` for Git, `repoClientMock` for OCI
- Tests create temporary HTTP servers for Helm repos using `gorilla/handlers`
- Chart test data uses in-memory tar.gz archives via `createChartArchive`

### Repository Loader Pattern
Each loader implements `repositoryLoader` interface:
```go
type repositoryLoader interface {
    loadRepositoryChart(repoNode *yaml.RNode, repoURL string, ...) (*chart.Chart, error)
}
```
Factory selects loader based on source kind (GitRepository/HelmRepository/OCIRepository). See [repository.go](pkg/repository/repository.go#L69-L81).

### YAML Node Manipulation
Use kustomize kyaml's `yaml.RNode` for manipulation, not raw YAML parsing:
```go
// Get API group
group := yaml.GetGroup(node)  // Extracts "apps" from "apps/v1"

// Get string with default fallback
value, err := yaml.GetStringOr(node, "spec.field", "default")
```

## Key Integration Points

### Flux Dependency Management
- Uses Flux's `@fluxcd/helm-controller/api` for `HelmRelease` types
- Uses Flux's `@fluxcd/source-controller/api` for repository source types
- Git cloning via Flux's `@fluxcd/pkg/git/gogit` (not direct go-git calls)

### Helm Integration
- Chart loading: `helm.sh/helm/v3/pkg/chart/loader`
- Template rendering: `helm.sh/helm/v3/pkg/engine`
- Always passes `KubeVersion` and `APIVersions` to chart rendering for accurate capability checks

### Authentication
Credentials file supports environment variable expansion with `$VAR` syntax:
```yaml
ssh://git@github.com/:
  credentials:
    identity: $SSH_KEY  # Expands from environment
```
AWS ECR auto-auth is built-in for OCI repos (see [oci.go](pkg/repository/oci.go)).

## Common Pitfalls
- Default command is `expand` - CLI auto-inserts it if no subcommand provided ([main.go](main.go#L24-L26))
- Git substitution format is strict: `<repo-url>#[<branch>#]<path>` - branch is optional
- Chart caching is per-URL+version - use `--chart-cache-dir` for persistent cache across runs
- Recursive expansion limit (`--max-expansions`) prevents infinite loops when charts produce more `HelmRelease` resources
