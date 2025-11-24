---
title: Best Practices
weight: 50
type: docs
---

## AI Coding Agent Instructions

For developers using AI coding agents (GitHub Copilot, Cursor, Codeium, etc.), Humus provides modular instruction files that you can copy to your project repository.

**📋 [Browse the instructions directory](https://github.com/z5labs/humus/tree/main/instructions)**

Copy the relevant instruction files to your project's `.github/` directory (or similar location):

**Available instruction files:**
- **[humus-common.instructions.md](https://github.com/z5labs/humus/blob/main/instructions/humus-common.instructions.md)** - Common patterns for all service types (required)
- **[humus-rest.instructions.md](https://github.com/z5labs/humus/blob/main/instructions/humus-rest.instructions.md)** - REST API specific patterns
- **[humus-grpc.instructions.md](https://github.com/z5labs/humus/blob/main/instructions/humus-grpc.instructions.md)** - gRPC service specific patterns
- **[humus-queue.instructions.md](https://github.com/z5labs/humus/blob/main/instructions/humus-queue.instructions.md)** - Queue/Kafka processor specific patterns
- **[humus-job.instructions.md](https://github.com/z5labs/humus/blob/main/instructions/humus-job.instructions.md)** - Job executor specific patterns

**What's included:**
- Project structure patterns (simple, organized, production-ready)
- Service-specific patterns (REST handlers, gRPC services, queue processors, jobs)
- Configuration best practices
- Error handling strategies
- Common pitfalls and anti-patterns
- Testing guidelines

**Usage:** Copy `humus-common.instructions.md` along with the file(s) specific to your application type. For example, a REST API would use `humus-common.instructions.md` + `humus-rest.instructions.md`.

This helps your AI coding agent generate code that follows Humus conventions and best practices.

## General Best Practices

For detailed best practices and patterns, see the [Project Structure guide]({{< ref "/getting-started/project-structure" >}}) and review the [example projects](https://github.com/z5labs/humus/tree/main/example).

## Community Support

See [GitHub Discussions](https://github.com/z5labs/humus/discussions) for community support.
