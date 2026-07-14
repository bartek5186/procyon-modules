# Procyon Modules

This workspace contains compile-time Procyon plugins. Each plugin is a normal
Go module with its own business logic, routes, migrations, configuration and
version. Each directory is a Go submodule published from this monorepo.

Modules are installed from a Procyon project with:

```bash
procyon-cli module add example --registry ../procyon-modules/registry.json
procyon-cli module add payment-system --provider stripe --registry ../procyon-modules/registry.json
```

The CLI does not copy plugin implementation files into an application. It adds
the Go module dependency and generates only `plugins_gen.go`, which registers
the plugin factory in the application binary.

Update a plugin after changing its manifest version (or checking out a newer
tag in the local source):

```bash
procyon-cli module update payment-system
```

After publishing a tagged module repository, use `--published` to avoid the
development `replace` directive:

```bash
procyon-cli module add payment-system --provider stripe --published
procyon-cli module update payment-system --published --version v0.3.2
```

The local registry and the parent workspace `go.work` are intended for
development. Publish the required Procyon Core version first, then tag
submodules using Go monorepo tag names such as `example/v0.1.1` and
`payment-system/v0.3.2`.
