---
title: Renovate Dependency Management Research
description: >
    Research findings on Renovate's Go language support, dependency grouping strategies,
    and automerge capabilities for Go projects.
weight: 1
type: docs
---

This document presents comprehensive research on Renovate Bot's capabilities for managing dependencies in Go projects. It covers Go language support, dependency grouping strategies, automerge capabilities, real-world examples, and best practices for implementing effective dependency automation.

## Table of Contents

1. [Overview](#overview)
2. [Go Language Support](#go-language-support)
3. [Dependency Grouping Strategies](#dependency-grouping-strategies)
4. [Automerge Capabilities](#automerge-capabilities)
5. [Real-World Examples](#real-world-examples)
6. [Best Practices and Recommendations](#best-practices-and-recommendations)

---

## Overview

Renovate is an automated dependency update tool that provides comprehensive support for Go modules through its `gomod` manager. It offers powerful automation capabilities while maintaining security and stability through configurable safety mechanisms.

### Key Capabilities

- **Automatic Detection**: Scans repositories for `go.mod` files at any depth
- **SemVer Support**: Understands semantic versioning and update types
- **Monorepo Grouping**: Intelligently groups related dependencies
- **Automerge**: Reduces manual work with configurable safety mechanisms
- **Security Integration**: OSV vulnerability database integration
- **Customization**: Extensive configuration options via `packageRules`

---

## Go Language Support

### Detection and Update Process

Renovate automatically detects Go modules by finding files matching `/(^|/)go\.mod$/` and:

1. Extracts existing dependencies from `require` statements
2. Resolves each dependency's source repository
3. Checks for SemVer tags in the source repository
4. Proposes pull requests when newer versions are available

### go.mod and go.sum Handling

| Aspect | Behavior |
|--------|----------|
| **go.sum Updates** | Automatically updated when dependencies change |
| **go mod tidy** | Opt-in via `postUpdateOptions: ["gomodTidy"]` |
| **Vendoring** | Auto-detected via `vendor/modules.txt`, commits vendor/ changes |
| **Major Updates** | Requires module path changes (e.g., `/v2` suffix) |

### Indirect Dependencies

**Default Behavior**: Disabled to reduce noise

**Enable with:**
```json
{
  "packageRules": [
    {
      "matchManagers": ["gomod"],
      "matchDepTypes": ["indirect"],
      "enabled": true
    }
  ]
}
```

**Why it matters:**
- Hugo modules are often indirect dependencies
- Enables updates to transitive dependencies with security patches
- Critical for frameworks and libraries that others depend on

### Hugo Module Support

Hugo uses Go modules for themes but has unique requirements:

**Critical Configuration:**
```json
{
  "packageRules": [
    {
      "matchManagers": ["gomod"],
      "matchDepTypes": ["indirect"],
      "enabled": true
    }
  ]
}
```

**Important: Do NOT use `gomodTidy` with Hugo modules** - it will remove all dependencies since Hugo doesn't import them as Go code. Hugo uses `hugo mod tidy` instead.

### Post-Update Options

| Option | Command | When to Use |
|--------|---------|-------------|
| `gomodTidy` | `go mod tidy` | Standard Go projects (NOT Hugo) |
| `gomodUpdateImportPaths` | Uses `mod` tool | Major version updates requiring import path changes |
| `gomodTidyE` | `go mod tidy -e` | Projects with expected tidy errors |
| `gomodMassage` | Pre-processes replace directives | Projects with relative replace statements |
| `goGenerate` | `go generate ./...` | Projects with generated code |

**Recommended for Go projects:**
```json
{
  "postUpdateOptions": [
    "gomodTidy",
    "gomodUpdateImportPaths"
  ]
}
```

### Go Toolchain Version Updates

Go 1.21+ introduced separate `go` and `toolchain` directives:

| Directive | Meaning | Default Renovate Behavior |
|-----------|---------|---------------------------|
| `go 1.22` | Minimum compatible version | Does NOT update (opt-in) |
| `toolchain go1.22.5` | Exact toolchain version | Updates automatically |

**To enable `go` directive updates:**
```json
{
  "packageRules": [
    {
      "matchDatasources": ["golang-version"],
      "rangeStrategy": "bump"
    }
  ]
}
```

---

## Dependency Grouping Strategies

Grouping reduces PR noise by combining related updates into single pull requests.

### When to Group Dependencies

**Group when:**
1. Same monorepo (OpenTelemetry, Kubernetes packages)
2. Tightly coupled (react + react-dom)
3. Framework components (all ESLint plugins)
4. Noise reduction (type definitions, dev dependencies)

**Keep separate when:**
1. Major updates (except monorepo packages)
2. Different ecosystems (database driver + UI library)
3. Different risk levels (production vs dev dependencies)
4. Incompatible version requirements

### Package Rules Structure

```json
{
  "packageRules": [
    {
      // At least one match* property required
      "matchSourceUrls": ["https://github.com/open-telemetry/opentelemetry-go"],

      // At least one action property required
      "groupName": "OpenTelemetry packages",
      "automerge": true
    }
  ]
}
```

**Match Logic:**
- Within a rule: ALL conditions must match (AND logic)
- Exception: `matchPackageNames` and `matchPackagePatterns` are OR'd
- Multiple rules: Last matching rule wins

### Matching Strategies

| Strategy | Use Case | Example |
|----------|----------|---------|
| `matchSourceUrls` | GitHub monorepos | OpenTelemetry, Kubernetes |
| `matchPackageNames` | Exact package names | `golang.org/x/**` (glob pattern) |
| `matchPackagePatterns` | Regex/glob patterns | `@types/*`, `^angular` |
| `matchDatasources` | By package manager | `go`, `npm`, `docker` |
| `matchUpdateTypes` | By SemVer type | `major`, `minor`, `patch` |

### Monorepo Grouping Patterns

**Common monorepo grouping examples:**

```json
{
  "packageRules": [
    {
      "groupName": "opentelemetry-go and opentelemetry-go-contrib monorepos",
      "matchSourceUrls": [
        "https://github.com/open-telemetry/opentelemetry-go",
        "https://github.com/open-telemetry/opentelemetry-go-contrib"
      ],
      "matchUpdateTypes": ["digest", "patch", "minor", "major"]
    },
    {
      "groupName": "golang.org/x monorepo",
      "matchDatasources": ["go"],
      "matchPackageNames": ["golang.org/x/**"],
      "matchUpdateTypes": ["digest", "patch", "minor", "major"]
    }
  ]
}
```

**Benefits:**
- Ensures compatible versions across related packages
- Single PR for coordinated releases
- Reduces merge conflicts
- Easier testing of combined updates

### Update Type Filtering

**Available types:**

| Type | Example | Risk Level |
|------|---------|------------|
| `lockFileMaintenance` | Lock file refresh | Lowest |
| `patch` | 1.0.0 → 1.0.1 | Very low |
| `minor` | 1.0.0 → 1.1.0 | Low |
| `major` | 1.0.0 → 2.0.0 | Higher |
| `digest` | Docker SHA updates | Low |

**Common pattern:**
```json
{
  "packageRules": [
    {
      "matchUpdateTypes": ["minor", "patch"],
      "automerge": true
    },
    {
      "matchUpdateTypes": ["major"],
      "dependencyDashboardApproval": true
    }
  ]
}
```

---

## Automerge Capabilities

Automerge drastically decreases the burden of handling dependency update PRs while maintaining safety through status checks and conditions.

### Safety Mechanisms

**Built-in protections:**
1. **Status Check Requirements** - All CI tests must pass
2. **Branch Up-to-Date** - Branch must be current with base branch
3. **Conservative Defaults** - Will not merge until all checks pass
4. **One at a Time** - Only one PR per target branch per run

**Prerequisites:**
- Comprehensive test coverage
- Reliable CI/CD pipeline
- Configured status checks in branch protection rules

### Branch Protection Requirements

| Requirement | Configuration |
|-------------|---------------|
| **GitHub Status Checks** | At least one required check configured |
| **Allow Auto-Merge** | Must be enabled in repository settings |
| **Committer Permissions** | Renovate bot must be allowed committer |
| **Review Bypass** | Optional: Add Renovate to bypass list |

### Platform Automerge vs Renovate Automerge

| Aspect | Platform Automerge | Renovate Automerge |
|--------|-------------------|-------------------|
| **Speed** | Faster (1 run) | Slower (2+ runs) |
| **Schedule** | Cannot honor `automergeSchedule` | Can honor schedule |
| **Control** | Platform manages merge | Renovate manages merge |
| **Configuration** | `platformAutomerge: true` | `platformAutomerge: false` |

### Stability Days and Confidence

**Stability Days (`minimumReleaseAge`):**
```json
{
  "minimumReleaseAge": "14 days"
}
```

**Purpose:** Delays updates to let community identify issues first

**Security Best Practice:** Set to "14 days" for third-party dependencies to allow time for malicious packages to be detected and removed from registries.

**Merge Confidence** (requires Mend.io API key):

| Level | Meaning | Factors |
|-------|---------|---------|
| **Very High** | Excellent adoption, age, CI status | Recommended for automerge |
| **High** | Good adoption and age | Safe for patch automerge |
| **Neutral** | Average confidence | Manual review recommended |
| **Low** | Limited data | Requires approval |

### Major vs Non-Major Strategy

**Recommended approach:**

```json
{
  "packageRules": [
    {
      "description": "Automerge non-major updates",
      "matchUpdateTypes": ["minor", "patch"],
      "automerge": true
    },
    {
      "description": "Require approval for major updates",
      "matchUpdateTypes": ["major"],
      "dependencyDashboardApproval": true
    }
  ]
}
```

**Rationale:**
- Minor and patch shouldn't have breaking changes (per SemVer)
- Major updates may require code changes
- Pre-1.0.0 dependencies can break at any time

**Exclude pre-1.0.0:**
```json
{
  "matchUpdateTypes": ["minor", "patch"],
  "matchCurrentVersion": "!/^0/",
  "automerge": true
}
```

### Scheduling Options

**Automerge Schedule:**
```json
{
  "schedule": ["before 4am"],
  "automergeSchedule": ["after 11pm", "before 4am"],
  "platformAutomerge": false
}
```

**Schedule presets:**
- `schedule:daily` - Daily before 4 AM
- `schedule:weekly` - Monday mornings before 4 AM
- `schedule:monthly` - First day of month before 4 AM

**Timezone configuration:**
```json
{
  "timezone": "America/New_York"
}
```

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| **Supply chain attacks** | Use `minimumReleaseAge: "14 days"` |
| **Inadequate test coverage** | Ensure comprehensive CI before enabling |
| **Missing status checks** | Configure at least one required check |
| **Bypassed reviews** | Limit to devDependencies and patch updates |

---

## Real-World Examples

### Example 1: k8sgpt-ai Project

**Configuration highlights:**
```json
{
  "extends": ["config:base"],
  "postUpdateOptions": ["gomodTidy", "gomodMassage"],
  "packageRules": [
    {
      "groupName": "kubernetes packages",
      "matchPackageNames": ["k8s.io/**", "sigs.k8s.io/**"]
    },
    {
      "matchUpdateTypes": ["minor", "patch"],
      "automerge": true
    }
  ]
}
```

**Key patterns:**
- Groups all Kubernetes packages together
- Enables automerge for non-major updates
- Uses `gomodMassage` for relative replace directives

### Example 2: SAP dns-masquerading-operator

**Version constraint handling:**
```json
{
  "postUpdateOptions": ["gomodUpdateImportPaths"],
  "packageRules": [
    {
      "matchDatasources": ["golang-version"],
      "rangeStrategy": "bump"
    },
    {
      "matchPackageNames": ["k8s.io/client-go"],
      "allowedVersions": "<1.4.0"
    }
  ]
}
```

**Key patterns:**
- Configures Go version bumping
- Restricts specific package versions
- Updates import paths automatically

### Example 3: Multi-Workspace Monorepo

**From Jamie Tanna's blog:**
```json
{
  "extends": ["config:best-practices"],
  "packageRules": [
    {
      "matchFileNames": ["projects/internal/**"],
      "matchManagers": ["gomod"],
      "groupName": "Internal services dependencies",
      "schedule": ["before 6am on Monday"]
    },
    {
      "matchFileNames": ["projects/internal/apk-builder/**"],
      "reviewers": ["team:platform"]
    }
  ]
}
```

**Key patterns:**
- Per-directory configuration
- Team-based reviewer assignment
- Service-specific scheduling

### Hugo-Specific Configuration

**Basic Hugo module setup:**
```json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": ["config:base"],
  "ignorePaths": ["exampleSite/**"],
  "packageRules": [
    {
      "matchManagers": ["gomod"],
      "matchDepTypes": ["indirect"],
      "enabled": true
    }
  ]
}
```

**Ignoring specific Hugo themes:**
```json
{
  "packageRules": [
    {
      "description": "Disable Docsy theme updates - customized theme",
      "matchPackageNames": ["github.com/google/docsy"],
      "matchManagers": ["gomod"],
      "enabled": false
    }
  ]
}
```

**Complete Hugo documentation site example:**
```json
{
  "extends": ["config:best-practices"],
  "ignorePaths": ["docs/exampleSite/**"],
  "packageRules": [
    {
      "description": "Enable Hugo module updates (indirect deps)",
      "matchManagers": ["gomod"],
      "matchDepTypes": ["indirect"],
      "matchFileNames": ["docs/go.mod"],
      "enabled": true
    },
    {
      "description": "Ignore Docsy theme - customized",
      "matchPackageNames": ["github.com/google/docsy"],
      "enabled": false
    },
    {
      "description": "Automerge minor Hugo module updates",
      "matchFileNames": ["docs/go.mod"],
      "matchUpdateTypes": ["minor", "patch"],
      "automerge": true
    }
  ]
}
```

---

## Best Practices and Recommendations

### Core Configuration Patterns

Based on research findings, these are recommended configuration patterns for Go projects:

#### 1. Add Stability Days for Security

```json
{
  "minimumReleaseAge": "7 days"
}
```

**Benefits:**
- Gives community time to identify issues in new releases
- Reduces risk of incorporating compromised packages
- Can be overridden for security patches

**For critical production systems, consider:**
```json
{
  "minimumReleaseAge": "14 days"
}
```

#### 2. Exclude Pre-1.0.0 from Automerge

```json
{
  "packageRules": [
    {
      "description": "Automerge non-major updates (excluding pre-1.0.0)",
      "matchUpdateTypes": ["minor", "patch"],
      "matchCurrentVersion": "!/^0/",
      "automerge": true
    }
  ]
}
```

**Rationale:** Pre-1.0.0 dependencies can introduce breaking changes in minor/patch updates per SemVer specification.

#### 3. Require Dashboard Approval for Major Updates

```json
{
  "packageRules": [
    {
      "description": "Require approval for major updates",
      "matchUpdateTypes": ["major"],
      "dependencyDashboardApproval": true
    }
  ]
}
```

**Benefits:**
- Major updates appear in Dependency Dashboard for manual review
- Prevents unexpected breaking changes
- Allows batch approval of major updates

#### 4. Add Explicit Automerge Schedule

```json
{
  "automergeSchedule": ["after 11pm", "before 4am"],
  "platformAutomerge": false
}
```

**Benefits:**
- More restrictive than PR creation schedule
- Reduces risk of disruption during work hours
- Note: Requires `platformAutomerge: false` to be honored

#### 5. Add Timezone Clarity

```json
{
  "timezone": "UTC"
}
```

**Benefits:**
- Makes timezone explicit in configuration
- Adjust to team's timezone if needed (e.g., "America/New_York")

#### 6. Bypass Stability Days for Security Patches

```json
{
  "packageRules": [
    {
      "description": "Skip stability days for security patches",
      "matchUpdateTypes": ["patch"],
      "matchPatchVersions": "security",
      "minimumReleaseAge": "0 days",
      "automerge": true
    }
  ]
}
```

**Benefits:**
- Security patches merge immediately
- Non-security patches wait for stability period

### Comprehensive Configuration Example

A well-balanced configuration combining security, automation, and stability:

```json
{
  "extends": ["config:best-practices"],
  "osvVulnerabilityAlerts": true,
  "minimumReleaseAge": "7 days",
  "timezone": "UTC",
  "schedule": ["before 4am"],
  "automergeSchedule": ["after 11pm", "before 4am"],
  "platformAutomerge": false,
  "baseBranchPatterns": ["main"],
  "labels": ["dependencies"],
  "packageRules": [
    {
      "description": "Automerge non-major updates (excluding pre-1.0.0)",
      "matchUpdateTypes": ["minor", "patch"],
      "matchCurrentVersion": "!/^0/",
      "automerge": true
    },
    {
      "description": "Require approval for major updates",
      "matchUpdateTypes": ["major"],
      "dependencyDashboardApproval": true
    },
    {
      "description": "Skip stability days for security patches",
      "matchUpdateTypes": ["patch"],
      "matchPatchVersions": "security",
      "minimumReleaseAge": "0 days",
      "automerge": true
    },
    {
      "matchManagers": ["gomod"],
      "matchDepTypes": ["indirect"],
      "enabled": true
    },
    {
      "groupName": "opentelemetry-go and opentelemetry-go-contrib monorepos",
      "matchSourceUrls": [
        "https://github.com/open-telemetry/opentelemetry-go",
        "https://github.com/open-telemetry/opentelemetry-go-contrib"
      ],
      "matchUpdateTypes": ["digest", "patch", "minor", "major"]
    },
    {
      "groupName": "google.golang.org/genproto/googleapis monorepo",
      "matchDatasources": ["go"],
      "matchPackageNames": ["google.golang.org/genproto/googleapis/**"],
      "matchUpdateTypes": ["digest", "patch", "minor", "major"]
    },
    {
      "groupName": "golang.org/x monorepo",
      "matchDatasources": ["go"],
      "matchPackageNames": ["golang.org/x/**"],
      "matchUpdateTypes": ["digest", "patch", "minor", "major"]
    }
  ],
  "postUpdateOptions": ["gomodTidy", "gomodUpdateImportPaths"]
}
```

### General Best Practices

1. **Start Conservative** - Begin with non-major automerge only, expand as confidence grows

2. **Group Thoughtfully** - Monorepo packages should be grouped, but avoid over-grouping unrelated dependencies

3. **Monitor Automerge Results** - Track success rates and adjust configuration based on actual outcomes

4. **Use Stability Days** - For production systems, 7-14 day delays add valuable safety against supply chain attacks

5. **Enable Indirect Dependencies** - Critical for Go projects, especially those with Hugo modules or library dependencies

6. **Document Hugo Module Handling** - Ensure contributors understand why `gomodTidy` is disabled for Hugo projects

### When NOT to Use Automerge

Consider disabling automerge for:

1. **Core Framework Dependencies**
   - Major version updates of base frameworks
   - SDK updates that may affect API compatibility
   - Router or server framework major versions

2. **gRPC and Protobuf**
   - Major version updates can break API compatibility
   - Requires manual testing across service boundaries

3. **Custom or Forked Dependencies**
   - Customized themes or libraries
   - Internal forks requiring careful version management

4. **Pre-1.0.0 Dependencies**
   - Breaking changes can occur in minor/patch versions
   - Requires manual review for each update

---

## Sources

### Official Renovate Documentation
- [Go Modules - Renovate Docs](https://docs.renovatebot.com/golang/)
- [Automated Dependency Updates for Go Modules](https://docs.renovatebot.com/modules/manager/gomod/)
- [Automerge configuration and troubleshooting](https://docs.renovatebot.com/key-concepts/automerge/)
- [Configuration Options](https://docs.renovatebot.com/configuration-options/)
- [Package Rules Guide](https://docs.mend.io/wsk/renovate-package-rules-guide)
- [Group Presets](https://docs.renovatebot.com/presets-group/)
- [Monorepo Presets](https://docs.renovatebot.com/presets-monorepo/)
- [String Pattern Matching](https://docs.renovatebot.com/string-pattern-matching/)
- [Merge Confidence](https://docs.renovatebot.com/merge-confidence/)
- [Upgrade Best Practices](https://docs.renovatebot.com/upgrade-best-practices/)
- [Renovate Scheduling](https://docs.renovatebot.com/key-concepts/scheduling/)

### Mend/WhiteSource Resources
- [Automating GO Module Dependency Updates](https://www.mend.io/blog/automating-go-module-dependency-updates/)
- [Common Practices for Renovate Configuration](https://docs.mend.io/wsk/common-practices-for-renovate-configuration)
- [Renovate Smart Merge Control Implementation Examples](https://docs.mend.io/wsk/renovate-smart-merge-control-implementation-exampl)

### Community Resources
- [Set Up Renovate for Hugo Modules and Modular Sites - HugoMods](https://hugomods.com/blog/2023/03/set-up-renovate-for-hugo-modules-and-modular-sites/)
- [A few tips for optimising Renovate for multi-team monorepos · Jamie Tanna](https://www.jvt.me/posts/2025/07/07/renovate-monorepo/)
- [Renovate: Merge dependencies with confidence | secustor](https://secustor.dev/blog/renovate_prevent_merging_bugs/)
- [Renovate – Keeping Your Updates Secure? – Compass Security Blog](https://blog.compass-security.com/2025/05/renovate-keeping-your-updates-secure/)

### Real-World Examples
- [k8sgpt/renovate.json at main · k8sgpt-ai/k8sgpt](https://github.com/k8sgpt-ai/k8sgpt/blob/main/renovate.json)
- [dns-masquerading-operator/renovate.json at main · SAP/dns-masquerading-operator](https://github.com/SAP/dns-masquerading-operator/blob/main/renovate.json)
- [proxy/.github/renovate.json5 at main · cilium/proxy](https://github.com/cilium/proxy/blob/main/.github/renovate.json5)

---

## Conclusion

Renovate provides powerful automation for managing dependencies in Go projects with comprehensive support for:

- **Security-first approach** via OSV vulnerability scanning and configurable safety mechanisms
- **Intelligent automation** through monorepo grouping and selective automerge
- **Developer-friendly** scheduling and automatic labeling
- **Go-specific features** including indirect dependency handling, major version path updates, and Hugo module support

The combination of stability days, pre-1.0.0 exclusions, dashboard approval for major updates, and thoughtful automerge policies creates a balanced approach between automation and safety.

Renovate's Go support is comprehensive, with special handling for Hugo modules, major version upgrades, and post-update cleanup making it well-suited for modern Go development workflows.
