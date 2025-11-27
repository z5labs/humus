---
title: Contributing
description: How to contribute to Humus
weight: 8
type: docs
---


Thank you for your interest in contributing to Humus! This guide will help you get started.

## Ways to Contribute

- **Report Bugs** - File issues on GitHub
- **Suggest Features** - Start a discussion
- **Improve Documentation** - Fix typos, add examples, clarify explanations
- **Write Code** - Fix bugs or implement features
- **Share Examples** - Contribute example applications

## Getting Started

1. **Fork the Repository** - https://github.com/z5labs/humus
2. **Clone Your Fork**
   ```bash
   git clone https://github.com/YOUR-USERNAME/humus.git
   cd humus
   ```
3. **Set Up Development Environment** - See [Development Setup]({{< ref "development-setup" >}})
4. **Create a Branch**
   ```bash
   git checkout -b feature/my-feature
   ```

## Development Workflow

### Running Tests

```bash
# Run all tests with race detection and coverage
go test -race -cover ./...

# Run tests for a specific package
go test -race -cover ./rest

# Run a specific test
go test -race -run TestName ./path/to/package
```

### Linting

```bash
# Run golangci-lint
golangci-lint run

# Auto-fix issues where possible
golangci-lint run --fix
```

### Building

```bash
# Build all packages
go build ./...

# Verify no build errors
go vet ./...
```

## Code Guidelines

### Style

- Follow standard Go conventions
- Use `gofmt` for formatting
- Run `golangci-lint` before committing
- Write clear, descriptive commit messages

### Testing

- Write tests for new features
- Maintain or improve code coverage
- Use table-driven tests where appropriate
- Mock external dependencies

### Documentation

- Document exported types and functions
- Include examples in godoc
- Update relevant documentation pages
- Add package-level documentation

## Pull Request Process

1. **Ensure Tests Pass**
   ```bash
   go test -race -cover ./...
   ```

2. **Ensure Linting Passes**
   ```bash
   golangci-lint run
   ```

3. **Update Documentation** - If you're adding features or changing behavior

4. **Write a Clear PR Description**
   - What does this PR do?
   - Why is this change needed?
   - How was it tested?

5. **Link Related Issues** - Use "Fixes #123" or "Relates to #456"

6. **Be Responsive** - Address review comments promptly

## Code of Conduct

- Be respectful and inclusive
- Welcome newcomers
- Focus on constructive feedback
- Assume good intentions

## Questions?

- **Development Questions** - See [Development Setup]({{< ref "development-setup" >}})
- **Testing Questions** - See [Testing Guide]({{< ref "testing-guide" >}})
- **Documentation** - See [Documentation Guide]({{< ref "documentation" >}})
- **General Questions** - Visit [GitHub Discussions](https://github.com/z5labs/humus/discussions)

## Resources

- [GitHub Repository](https://github.com/z5labs/humus)
- [Issue Tracker](https://github.com/z5labs/humus/issues)
- [Discussions](https://github.com/z5labs/humus/discussions)
- [Development Setup Guide]({{< ref "development-setup" >}})
- [Testing Guide]({{< ref "testing-guide" >}})
