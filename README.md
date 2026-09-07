# controller-template

A production-ready starting point for building Kubernetes controller services on the Milo platform. Fork this once and you get a working kubebuilder v4 controller, example CRD with reconciler/finalizer/conditions, defaulting and validating webhooks, a full Kustomize deployment tree, a Remix UI, Taskfile-based dev workflow, GitHub Actions CI, and a Claude Code integration that guides you through naming and initialization before you write a single line of code.

This template exists because building all of that from scratch — correctly — takes days. Forking this takes minutes.

---

## Using with Claude Code

This template is designed for AI-assisted development. Claude Code has first-class support baked in: a session hook, a `/init-service` command, and a `CLAUDE.md` that gives Claude full context about the codebase conventions.

### First open: automatic product discovery

When you open this repo in Claude Code for the first time (before initialization), a session hook fires automatically. Claude starts a product discovery conversation:

1. It silently researches the existing `milo-os` services on GitHub to understand what already exists, which API groups are taken, and which naming patterns are used across the org.
2. It asks what you're trying to build — starting with the problem, not the name.
3. It proposes a service name, API group, and primary resource kind based on your description and the existing ecosystem. It will flag if something similar already exists.
4. Once you confirm the design, it runs `hack/rename.sh` with the agreed values and creates the `.claude/.initialized` marker.

You never touch the rename script manually. Claude runs it for you after you've agreed on the design.

### After initialization

Once initialized, Claude Code has full context via `CLAUDE.md` to help you develop the service iteratively:

- Adding new resource types (`kubebuilder create api`)
- Writing reconciler logic
- Adding validation rules to webhooks
- Extending the Remix UI with new routes and components
- Generating and applying manifests

This is not a one-time scaffold. It's a development partner for the full lifecycle of the service.

### Manual trigger

If you want to restart the discovery flow, run:

```
/init-service
```

---

## Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| Go | 1.25+ | https://go.dev/dl |
| Docker | recent | https://docs.docker.com/get-docker |
| kind | v0.20+ | `go install sigs.k8s.io/kind@latest` |
| Task | v3+ | https://taskfile.dev/installation |
| pnpm | v9+ | `npm install -g pnpm` |
| kubebuilder | v4 (reference) | https://book.kubebuilder.io/quick-start |
| gh | any | https://cli.github.com |

kubebuilder is listed as a reference tool — you need it to add new API types after initialization, but not for the initial setup.

---

## Getting started manually

If you're not using Claude Code, run the rename script yourself after forking:

```bash
chmod +x hack/rename.sh
./hack/rename.sh \
  --service-name billing \
  --api-group billing.miloapis.com \
  --kind BillingAccount
```

Use `--dry-run` to preview changes without writing anything.

The script replaces all template placeholders throughout the codebase:

| Placeholder | Replaced with |
|-------------|---------------|
| `controller-template` | `--service-name` (e.g. `billing`) |
| `example.miloapis.com` | `--api-group` (e.g. `billing.miloapis.com`) |
| `Resource` | `--kind` (e.g. `BillingAccount`) |
| `resource` | lowercase kind (e.g. `billingaccount`) |
| `ControllerTemplateOperator` | `<Kind>Operator` |
| `CONTROLLER_TEMPLATE_API_` | `<SERVICE>_API_` env prefix |
| `go.miloapis.com/controller-template` | `go.miloapis.com/<service-name>` |

File and directory names containing these strings are renamed as well.

After renaming, verify no placeholders remain:

```bash
grep -r "controller-template\|example\.miloapis\.com" \
  --include="*.go" --include="*.yaml" --include="*.ts" --include="*.tsx" .
```

An empty result means all placeholders were replaced. Hits in `zz_generated.*` files are expected and will be overwritten in the next step.

Then regenerate and verify the build:

```bash
task generate && task manifests
task build && task test
```

---

## Local development

Bootstrap a local kind cluster and deploy the controller:

```bash
task dev:setup
```

This creates a kind cluster (via the shared test-infra Taskfile), builds the controller image, loads it into kind, waits for cert-manager, and applies the dev Kustomize overlay. Expect it to take a couple of minutes on first run.

After making code changes:

```bash
task dev:redeploy
```

This rebuilds the image, loads it into the kind cluster, and cycles the controller pod. Faster than a full `dev:setup`.

Start the Remix UI dev server:

```bash
task ui:dev   # http://localhost:3000
```

The UI connects to the Kubernetes API server using environment variables. Copy `ui/.env.example` to `ui/.env` and configure:

```bash
# Option 1: explicit credentials
<SERVICE>_API_SERVER_URL=https://your-api-server:6443
<SERVICE>_API_CA_FILE=/path/to/ca.crt
<SERVICE>_API_TOKEN_FILE=/path/to/token

# Option 2: local kubeconfig (dev default)
# Leave the URL unset — falls back to .test-infra/kubeconfig automatically
```

Run Chainsaw end-to-end tests:

```bash
task e2e
```

---

## Project structure

```
controller-template/
├── cmd/controller-template/    # Binary entrypoint
├── api/v1alpha1/               # CRD type definitions (*_types.go)
├── internal/
│   ├── config/                 # Operator config type (kubeconfigPath, etc.)
│   └── controller/             # Reconciler — finalizer, conditions, status
├── internal/webhook/v1alpha1/  # Defaulting + validating webhook
├── config/
│   ├── base/                   # Core manifests (CRD, RBAC, webhook, manager)
│   ├── components/             # Optional overlayable components
│   └── overlays/               # dev and prod environment overlays
├── ui/                         # Remix UI (React + datum-ui, Tailwind)
│   ├── app/routes/             # File-based Remix routes
│   └── app/lib/                # k8s server client, kubeconfig, types
├── test/e2e/                   # Chainsaw test cases
└── hack/
    └── rename.sh               # One-shot placeholder replacement script
```

---

## Task reference

| Task | What it does |
|------|-------------|
| `task build` | Compile the controller binary |
| `task test` | Run Go tests |
| `task lint` | Run golangci-lint |
| `task generate` | Generate deepcopy and defaulter code |
| `task manifests` | Generate CRD, RBAC, and webhook manifests |
| `task dev:setup` | Create kind cluster and deploy controller |
| `task dev:redeploy` | Rebuild image and cycle the controller pod |
| `task ui:dev` | Start Remix UI at http://localhost:3000 |
| `task ui:build` | Production build of the UI |
| `task ui:type-check` | TypeScript type check |
| `task e2e` | Run Chainsaw e2e tests |

---

## Connecting to the Milo control plane

The operator config supports a `kubeconfigPath` field that points the controller at Milo's API server instead of its local cluster:

```yaml
apiVersion: apiserver.config.miloapis.com/v1alpha1
kind: ControllerTemplateOperator
metricsServer:
  bindAddress: "0"
kubeconfigPath: /etc/milo/kubeconfig
```

When `kubeconfigPath` is empty, the controller falls back to in-cluster config — the default for local kind development.

---

## Deploying

The repository includes a GitHub Actions publish workflow that triggers on merge to `main`. It builds and pushes the controller Docker image and Kustomize bundles to `ghcr.io/milo-os/<service-name>`. No additional configuration is needed beyond setting the standard `GITHUB_TOKEN` secret, which Actions provides automatically.
